# Performance

## How writes scale

etcd clients, and the Kubernetes watch cache in particular, rely on dense,
totally ordered revisions. Every kine-fdb write therefore takes the next
revision of its namespace, and writes to one namespace are serialized.

To keep that from becoming one FoundationDB commit per write, each kine-fdb
process sends all of its writes through one committer (group commit): the
writes that queue up while a transaction commits go into the next
transaction together, each with its own consecutive revision. A single
writer sees the latency of one commit; many concurrent writers share
commits, and throughput grows with concurrency instead of collapsing into
retries.

Replicas of the same namespace serialize against each other through
FoundationDB's conflict detection; most deployments send a control plane's
writes through its replicas evenly, and every batch still carries many
writes. Separate namespaces never conflict, so the total write rate of a
FoundationDB cluster shared by many control planes scales with the cluster.

## Measurements

`go test -run XXX -bench . -benchtime 3s -cpu 1,8,32,128 ./pkg/fdbdriver`
on one host (2 x Xeon E5-2680 v4, 56 threads), against a **single-process**
FoundationDB 7.3.77 (`single` redundancy, `ssd-2` engine) in a container on
the same host — the smallest possible cluster, so treat the numbers as a
floor. Values are 2 KiB, typical for Kubernetes objects. `-cpu N` runs N
concurrent clients.

| Operation | 1 client | 8 clients | 32 clients | 128 clients |
|---|---|---|---|---|
| Create | 510/s | 1,920/s | 4,190/s | 6,160/s |
| Update, distinct keys (lease renewals) | 410/s | 1,730/s | 3,610/s | 5,350/s |
| Get | 1,420/s | 5,130/s | 7,910/s | |
| List of 500 objects | 58/s | 71/s | 60/s | |

A single write takes about 2 ms, one FoundationDB commit on this host.
Without group commit the same Create benchmark ran at 460, 345, and 250
writes per second with 1, 8, and 32 clients: every additional writer added
conflicts instead of throughput.

For scale: a Kubernetes cluster with 1,000 nodes renews about 100 node leases
per second, and the apiserver's own writes add to that. Reads are served
mostly from the apiserver's watch cache.

## Lists and counts

Lists read the index, then the values, in pages of up to 1,000 keys or 8 MiB.
Counts and keys-only lists read only the index. kine counts the remaining
keys for every page of a paginated list, so very large paginated lists cost
one count per page; the apiserver serves most lists from its watch cache.

## Watches

Watch latency is the time for the poller to be woken by FoundationDB's watch
on the revision counter and read the change log. On the host above an event
written through one kine-fdb server reached a watcher on another server
1.5 ms after the write committed, on average (`BenchmarkWatchDelivery`, with
the fallback poll disabled). One poller serves all watchers of a process; a
watcher that falls behind reads the change log itself rather than slowing the
others.

## Tuning

- `--compact-interval` and `--compact-min-retain` bound how much history is
  kept; less history makes reads at old revisions and watch catch-up
  cheaper.
- `--poll-batch-size` bounds the events read per watch poll.
- Use the `ssd-redwood-1` or `ssd-2` engine and a FoundationDB cluster sized
  for your write rate; kine-fdb itself needs little CPU or memory.
