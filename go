#!/bin/bash
# NetBootX Go 包装器：把 build cache / module cache 都放进工作区，
# 避免沙箱拒绝写 ~/Library/Caches/go-build。
# 用法：./go build ./...    ./go test ./...    ./go vet ./...
set -e
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export GOCACHE="$ROOT/.tools/cache"
export GOMODCACHE="$ROOT/.tools/mod"
export GOPATH="$ROOT/.tools/gopath"
export GOFLAGS="-mod=mod"
export CGO_ENABLED=0
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH"
exec "$ROOT/.tools/go/bin/go" "$@"
