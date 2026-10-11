# Container image for kine-fdb and kine-fdb-admin. The FoundationDB client
# library is installed from the official release packages; FDB_VERSION must
# match the major.minor version of the cluster the image connects to.
#
#   docker build --build-arg FDB_VERSION=7.3.77 -t kine-fdb .

ARG GO_IMAGE=golang:1.26-trixie
ARG RUNTIME_IMAGE=debian:trixie-slim

FROM ${GO_IMAGE} AS build
ARG FDB_VERSION=7.3.77
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""

RUN set -eux; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) fdb_arch=amd64 ;; \
      arm64) fdb_arch=aarch64 ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/fdb-clients.deb \
      "https://github.com/apple/foundationdb/releases/download/${FDB_VERSION}/foundationdb-clients_${FDB_VERSION}-1_${fdb_arch}.deb"; \
    dpkg -i /tmp/fdb-clients.deb; \
    rm /tmp/fdb-clients.deb

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eux; \
    ldflags="-s -w -X github.com/avkcode/kine-fdb/pkg/version.Version=${VERSION} -X github.com/avkcode/kine-fdb/pkg/version.Commit=${COMMIT}"; \
    CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags "${ldflags}" -o /out/kine-fdb ./cmd/kine-fdb; \
    CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags "${ldflags}" -o /out/kine-fdb-admin ./cmd/kine-fdb-admin; \
    mkdir -p /out/lib /out/bin; \
    cp /usr/lib/libfdb_c.so /out/lib/; \
    cp /usr/bin/fdbcli /out/bin/

FROM ${RUNTIME_IMAGE}
ARG FDB_VERSION=7.3.77
ARG VERSION=dev
LABEL org.opencontainers.image.title="kine-fdb" \
      org.opencontainers.image.description="etcd v3 API for Kubernetes, stored in FoundationDB" \
      org.opencontainers.image.source="https://github.com/avkcode/kine-fdb" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      io.foundationdb.client.version="${FDB_VERSION}"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    useradd --system --uid 65532 --user-group --home-dir /var/lib/kine-fdb --create-home kine-fdb; \
    install -d -o kine-fdb -g kine-fdb /etc/foundationdb

COPY --from=build /out/lib/libfdb_c.so /usr/lib/libfdb_c.so
COPY --from=build /out/bin/fdbcli /usr/local/bin/fdbcli
COPY --from=build /out/kine-fdb /out/kine-fdb-admin /usr/local/bin/
RUN ldconfig

USER 65532:65532
WORKDIR /var/lib/kine-fdb
ENV FDB_CLUSTER_FILE=/etc/foundationdb/fdb.cluster
EXPOSE 2379 8080
ENTRYPOINT ["/usr/local/bin/kine-fdb"]
CMD ["--listen-address=0.0.0.0:2379", "--metrics-bind-address=0.0.0.0:8080"]
