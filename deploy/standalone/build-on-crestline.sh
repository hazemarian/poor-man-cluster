#!/bin/sh
# Run on Crestline after syncing the complete poor-man-stack repository to
# ~/nextrum-build/repository (Go module is repository/pmcluster).
set -eu
build_root="${NEXTRUM_BUILD_ROOT:-$HOME/nextrum-build}"
mkdir -p "$build_root/release"
rm -f "$build_root/release/tests-passed"
docker run --rm --name nextrum-go-checks --memory 4g --cpus 3 \
  -e GOMAXPROCS=2 -v "$build_root/repository:/repo" \
  -v "$build_root/release:/release" -v nextrum-gomod:/go/pkg/mod \
  -v nextrum-gobuild:/root/.cache/go-build -w /repo/pmcluster golang:1.25 \
  bash -c 'set -e
    CGO_ENABLED=0 go build -p 2 -trimpath -ldflags="-s -w" -o /release/pmcluster ./cmd/pmcluster
    CGO_ENABLED=0 go build -p 2 -trimpath -ldflags="-s -w" -o /release/edge ./cmd/edge
    sha256sum /release/pmcluster /release/edge > /release/SHA256SUMS
    go vet -p 3 ./...
    go test -race -p 3 -count=1 ./... > /release/race.log 2>&1
    touch /release/tests-passed'
