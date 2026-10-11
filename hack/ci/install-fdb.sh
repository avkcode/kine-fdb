#!/usr/bin/env bash
# Installs the FoundationDB client and server packages on a Debian or Ubuntu
# CI runner and waits until the local single-process database is available.
set -euo pipefail

version="${FDB_VERSION:-7.3.77}"
case "$(dpkg --print-architecture)" in
  amd64) arch=amd64 ;;
  arm64) arch=aarch64 ;;
  *) echo "unsupported architecture $(dpkg --print-architecture)" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
base="https://github.com/apple/foundationdb/releases/download/${version}"
curl -fsSL -o "${tmp}/clients.deb" "${base}/foundationdb-clients_${version}-1_${arch}.deb"
curl -fsSL -o "${tmp}/server.deb" "${base}/foundationdb-server_${version}-1_${arch}.deb"
sudo dpkg -i "${tmp}/clients.deb" "${tmp}/server.deb"

# The server package normally creates a database; make sure one exists.
for _ in $(seq 1 60); do
  if fdbcli --timeout 5 --exec "status minimal" 2>/dev/null | grep -q "The database is available"; then
    sudo chmod a+r /etc/foundationdb/fdb.cluster
    echo "FoundationDB ${version} is available"
    exit 0
  fi
  sudo fdbcli --timeout 5 --exec "configure new single ssd" >/dev/null 2>&1 || true
  sleep 2
done
echo "FoundationDB did not become available" >&2
fdbcli --timeout 5 --exec status >&2 || true
exit 1
