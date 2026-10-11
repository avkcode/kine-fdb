# Security policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them
privately through GitHub's
[private vulnerability reporting](https://github.com/avkcode/kine-fdb/security/advisories/new)
for this repository. Include the affected version, a description of the
problem, and steps to reproduce it.

You should receive an acknowledgement within a week. Fixes are released as
soon as they are ready and are announced in a GitHub security advisory.

## Supported versions

Security fixes are made for the latest minor release.

## Deployment notes

kine-fdb serves the full contents of a Kubernetes datastore, including
Secrets. Protect it like etcd:

- Expose the etcd API only to the control plane, and use TLS with client
  certificates (`--server-cert-file`, `--server-key-file`,
  `--trusted-ca-file`) when it crosses an untrusted network.
- Use FoundationDB TLS between kine-fdb and the cluster when that network is
  not trusted, and restrict who can reach the FoundationDB cluster: anyone
  who can connect to it can read every namespace.
- Enable encryption at rest in the Kubernetes apiserver for Secrets; kine-fdb
  stores values as it receives them.
