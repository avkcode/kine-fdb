# Development and CI image for kine-fdb: Go toolchain plus the FoundationDB
# client library and headers that the cgo bindings need at build and test time.

ARG GO_IMAGE=golang:1.26-trixie
FROM ${GO_IMAGE}

ARG FDB_VERSION=7.3.77
ARG TARGETARCH

RUN set -eux; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) fdb_arch=amd64 ;; \
      arm64) fdb_arch=aarch64 ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl jq rsync; \
    curl -fsSL -o /tmp/fdb-clients.deb \
      "https://github.com/apple/foundationdb/releases/download/${FDB_VERSION}/foundationdb-clients_${FDB_VERSION}-1_${fdb_arch}.deb"; \
    dpkg -i /tmp/fdb-clients.deb; \
    rm -f /tmp/fdb-clients.deb; \
    rm -rf /var/lib/apt/lists/*; \
    git config --system --add safe.directory '*'

ENV CGO_ENABLED=1
WORKDIR /src
