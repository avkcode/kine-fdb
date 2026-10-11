# Build, test, and packaging entry points for kine-fdb. Building needs the
# FoundationDB client library and headers; `make dev-up` provides them in a
# container together with a test cluster.

GO          ?= go
FDB_VERSION ?= 7.3.77
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
IMAGE       ?= ghcr.io/avkcode/kine-fdb
PLATFORMS   ?= linux/amd64,linux/arm64
LDFLAGS     := -s -w \
	-X github.com/avkcode/kine-fdb/pkg/version.Version=$(VERSION) \
	-X github.com/avkcode/kine-fdb/pkg/version.Commit=$(COMMIT)
DEVENV      := ./hack/dev/devenv.sh

.PHONY: all build test test-unit test-integration test-race test-apiserver test-e2e \
	fuzz lint fmt vet tidy image image-multiarch helm-lint dev-up dev-down dev-shell \
	dev-test clean help

all: lint test-unit build ## Lint, run unit tests, and build

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/kine-fdb and bin/kine-fdb-admin
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/kine-fdb ./cmd/kine-fdb
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/kine-fdb-admin ./cmd/kine-fdb-admin

test-unit: ## Run tests that need no FoundationDB cluster
	$(GO) test ./internal/...

test: ## Run all tests; integration tests need KINE_FDB_TEST_CLUSTER_FILE
	$(GO) test -count=1 ./...

test-integration: ## Run the driver tests against KINE_FDB_TEST_CLUSTER_FILE
	@test -n "$$KINE_FDB_TEST_CLUSTER_FILE" || (echo "set KINE_FDB_TEST_CLUSTER_FILE" >&2; exit 1)
	$(GO) test -count=1 -v ./pkg/...

test-race: ## Run the driver tests with the race detector
	$(GO) test -race -count=1 ./...

test-apiserver: ## Run the Kubernetes apiserver storage test suite against kine-fdb
	./test/apiserver/run.sh

test-e2e: ## Run a K3s cluster on kine-fdb in Docker and exercise it (see hack/e2e)
	./hack/e2e/k3s-soak.sh

fuzz: ## Fuzz the record decoders for a minute each
	$(GO) test -run XXX -fuzz FuzzDecodeEntry -fuzztime 60s ./internal/record
	$(GO) test -run XXX -fuzz FuzzDecodeIndex -fuzztime 60s ./internal/record
	$(GO) test -run XXX -fuzz FuzzDecodeLog -fuzztime 60s ./internal/record

fmt: ## Format the code
	gofmt -w -s $$(find . -name '*.go' -not -path './_output/*')

vet: ## Run go vet
	$(GO) vet ./...

lint: vet ## Check formatting and run go vet and staticcheck
	@unformatted="$$(gofmt -l -s $$(find . -name '*.go' -not -path './_output/*'))"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...

tidy: ## Tidy both Go modules
	$(GO) mod tidy
	cd test/apiserver && $(GO) mod tidy

image: ## Build the container image for the local platform
	docker build --build-arg FDB_VERSION=$(FDB_VERSION) --build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

image-multiarch: ## Build and push the multi-architecture image
	docker buildx build --platform $(PLATFORMS) --build-arg FDB_VERSION=$(FDB_VERSION) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) --push .

helm-lint: ## Lint and render the Helm chart with each CI values file
	@for f in charts/kine-fdb/ci/*.yaml; do \
		echo "== $$f"; \
		helm lint charts/kine-fdb -f $$f && helm template test charts/kine-fdb -f $$f >/dev/null || exit 1; \
	done

dev-up: ## Start a FoundationDB test cluster and a build container
	$(DEVENV) up

dev-down: ## Remove the development containers (PURGE=1 also removes data)
	$(DEVENV) down

dev-shell: ## Open a shell in the build container
	docker exec -it kinefdb-dev bash

dev-test: ## Run every Go test inside the build container
	$(DEVENV) sh 'go test -count=1 ./... && go test -race -count=1 ./pkg/...'

clean: ## Remove build outputs
	rm -rf bin dist _output
