# Architecture

kine-fdb implements kine's `server.Backend` interface on FoundationDB. kine
turns the etcd v3 gRPC API that Kubernetes uses into a small set of
operations — get, list, count, create, compare-and-swap update, delete,
watch, and compact — and kine-fdb maps each of them onto FoundationDB
transactions.

## Namespaces

Every endpoint names a *namespace*: an independent etcd keyspace with its own
revision sequence. A namespace is a FoundationDB
[directory](https://apple.github.io/foundationdb/developer-guide.html#directories)
at path `["kine", <namespace>]`, created with the layer tag `kine-fdb`. The
directory layer gives each namespace a short, unique key prefix, so
namespaces never overlap — `/a/` and `/a/b/` are siblings, not parent and
child — and every key carries only a few bytes of prefix.

Namespace names are normalized: `tenant-a`, `/tenant-a`, and `/tenant-a/` are
the same namespace.

## Key layout

Inside its directory a namespace stores (tuple notation, `<key>` is the etcd
key as bytes):

| Key | Value | Purpose |
|---|---|---|
| `(m, "format")` | int | Storage format version (1). |
| `(m, "rev")` | int | Current revision. |
| `(m, "compact")` | int | Compact revision: reads and watches below it fail with `ErrCompacted`. |
| `(m, "compacted")` | int | Cleanup point: history below it has been deleted. |
| `(k)` + `<key>` | index record | Latest state of the key (revision, create revision, version, lease, value size, deleted flag). Raw key bytes follow the prefix so an etcd range maps to one FoundationDB range. |
| `(h, <key>, rev)` | entry record | The version of the key written at `rev`, with its value inline when it is at most 90,000 bytes. |
| `(v, <key>, rev, i)` | bytes | Chunk `i` of a larger value (FoundationDB values are limited to 100 kB). |
| `(r, rev)` | log record | Change log: which key changed at `rev`, the revision it superseded, and flags. |

Integers are 8-byte little-endian. Records start with a format byte and use
varints; `internal/record` defines and fuzz-tests them. Unknown formats are
refused rather than misread, and a namespace whose format is newer than the
binary refuses to open.

## Revisions and writes

A write is one FoundationDB transaction that:

1. reads the current revision and the key's newest version,
2. checks the precondition (create: absent or deleted; update and delete: at
   the expected revision),
3. writes the version at `current + 1`, the index row, the change-log entry,
   and the new current revision.

All writers read the revision counter, so FoundationDB's optimistic
concurrency serializes them: revisions are dense and totally ordered, which
is what etcd clients and the Kubernetes watch cache rely on. Writers do not
read the compaction markers, so compaction never makes them retry.

Within a process, writes go through one committer (group commit). The
writes that queue up while a transaction commits form the next batch: the
committer reads the newest version of every key in the batch at once, then
decides each write in order — later writes see earlier writes of the same
key — and gives each its own consecutive revision, all in one transaction.
A write that fails its precondition reports its result without affecting the
rest of the batch. Concurrent writers therefore share commits instead of
conflicting with each other; see [performance](performance.md).

A commit can end with `commit_unknown_result`: the transaction may or may not
have been applied. Retrying blindly would make a successful create look like
a conflict. kine-fdb remembers the revision each write was assigned in such
an attempt and, on retry, first checks whether a version with exactly that
revision, precondition, and value exists — if it does, the earlier attempt
succeeded and its result is returned instead of writing again.

Every transaction honors the request's context: cancellation cancels the
transaction, and the deadline (or the endpoint `timeout`, whichever is
sooner) bounds it, including retries. When FoundationDB is unreachable,
requests fail with gRPC `Unavailable` instead of hanging.

## Reads

`Get` reads the newest version of a key at or below the requested revision
with one reverse range read over `(h, <key>)`.

`List` and `Count` walk the index. Rows whose latest revision is at or below
the target are answered from the index (keys-only and counts) or with one
point read of the version; rows changed after the target fall back to a
floor read of the key's history, and rows that are deletions are skipped.
Large ranges are read in pages, each its own transaction, all pinned to the
same target revision: because history is versioned, pages stay consistent
without one long transaction, and a page fails with `ErrCompacted` if
compaction passes the target revision in between. Pages are bounded by row
count and by the value bytes recorded in the index.

kine's range conventions apply: an empty key means every key, an empty end
means exactly the key, and otherwise the range is `[key, end)`.

## Watches

kine's watch contract is "every change from revision N onward"; kine itself
filters by key range. Each kine-fdb process runs one poller:

- It reads the change log from the last delivered revision, resolves each
  entry to a full event (current value and, except for creates, the previous
  value), and hands the batch to every subscriber.
- When it reaches the current revision it sets a FoundationDB watch on the
  revision counter in the same transaction, so the next write anywhere —
  through this process or any other kine-fdb replica — wakes it. A fallback
  timer (`poll_interval`) covers missed notifications.
- Local writes also wake it directly.

A subscriber that starts in the past reads the log itself until it reaches
the poller's position, then switches to the poller's batches without gaps or
duplicates. If a subscriber stops reading and its queue fills, the poller
stops queueing for it and it reads the log itself until it has caught up
again. Nobody is dropped for being slow — kine's own lease-expiry watcher
would never be restarted if it were.

Batches carry a strictly increasing `CurrentRev`, which kine requires.
Following etcd, events at or below the compact revision carry no previous
value, a watch may start exactly at the compact revision, and a watch that
starts below it is refused with the compact and current revisions.

## Compaction

Compaction has two steps:

1. **Logical:** move `(m, "compact")` forward. From then on reads and watches
   below it fail with `ErrCompacted`.
2. **Cleanup:** walk the change log from `(m, "compacted")` up to the compact
   revision, in batches of `--compact-batch-size` revisions per transaction.
   Each entry names the version it superseded; that version (and its chunks)
   is deleted. A deletion's tombstone is deleted too, and so is the key's
   index row unless the key has been recreated since. The processed change
   log is cleared and `(m, "compacted")` advances.

The change-log entry at the compact revision itself is kept so that a watch
may start there. Cleanup reads only the small log records, never values, and
reads the revision counter as a snapshot read, so it does not conflict with
writers. It is resumable: an interrupted run continues from `(m,
"compacted")`, and several processes may run it concurrently.

With `--compact-interval` above zero (the default, five minutes) each process
compacts automatically to the revision that was current one interval
earlier, keeping at least `--compact-min-retain` revisions, exactly like
kine's SQL backends; compaction requests from clients are then accepted
without effect. With `--compact-interval=0` client requests (for example the
kube-apiserver's) perform the compaction.

## Leases

kine grants leases whose ID is their TTL and stores the TTL with each key;
kine's TTL runner watches for leased keys and deletes them when they expire.
kine-fdb stores the lease in every version and index row. Because watches
keep working through compactions and restarts, so does lease expiry.

## Database size

`DbSize` (reported by `etcdctl endpoint status` and the apiserver's storage
size metric) is FoundationDB's estimate of the namespace's byte size, sampled
by the storage servers and cached for ten seconds. `kine-fdb-admin stats`
computes exact sizes by scanning.

## Consistency checking

`kine-fdb-admin verify` checks the invariants above: every index row points at
its key's newest version, the change log is dense from the cleanup point to
the current revision, chunked values are complete, and nothing that cleanup
should have removed is left behind. The test suite runs it after its
scenarios.
