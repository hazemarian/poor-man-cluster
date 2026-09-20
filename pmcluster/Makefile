# pmcluster Makefile
# Targets: build, test, e2e, lint, tidy, clean, install

BINARY      := pmcluster
PKG         := github.com/hazemarian/poor-man-cluster/pmcluster
CMD         := ./cmd/pmcluster
BIN_DIR     := bin
OUT         := $(BIN_DIR)/$(BINARY)

# Build-time metadata, injected via -ldflags
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)

GO          ?= go
GOFLAGS     ?=

.PHONY: all build test e2e lint tidy fmt vet clean install help

all: build

## build : compile the pmcluster binary into ./bin/
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(OUT) $(CMD)
	@echo "→ $(OUT) ($(VERSION))"

## install : go install pmcluster into $GOBIN
install:
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(CMD)

## test : run unit tests (skips ./e2e)
test:
	$(GO) test $(GOFLAGS) -race -timeout 60s ./...

## e2e : run end-to-end tests in ./e2e (requires Docker, may be slow)
e2e:
	$(GO) test $(GOFLAGS) -timeout 10m -tags=e2e ./e2e/...

## lint : run golangci-lint (install via: brew install golangci-lint)
lint:
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed; brew install golangci-lint" >&2; exit 1; }
	golangci-lint run ./...

## fmt : gofmt the codebase
fmt:
	$(GO) fmt ./...

## vet : go vet the codebase
vet:
	$(GO) vet ./...

## tidy : go mod tidy
tidy:
	$(GO) mod tidy

## clean : remove built binaries
clean:
	rm -rf $(BIN_DIR)

## help : show available targets
help:
	@awk 'BEGIN { FS = ":.*?## " } /^## / { sub(/^## /, ""); split($$0, a, " : "); printf "  %-10s %s\n", a[1], a[2] }' $(MAKEFILE_LIST)
