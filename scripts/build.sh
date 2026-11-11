#!/usr/bin/env bash
# Chagent 构建脚本：生成 ./bin/chagent（当前平台）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BIN_DIR="${BIN_DIR:-$ROOT/bin}"
BIN="${BIN_DIR}/chagent"
CMD="./cmd/chagent"
CONFIG_PKG="github.com/rlds/chagent/pkg/config"

GO="${GO:-go}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short=8 HEAD 2>/dev/null || echo dev)}"
BUILD_TIME="${BUILD_TIME:-$(date +%FT%T%z)}"
GO_VERSION="${GO_VERSION:-$("$GO" env GOVERSION 2>/dev/null || echo unknown)}"

mkdir -p "$BIN_DIR"
echo "==> building chagent"
echo "    version: $VERSION ($GIT_COMMIT)"
echo "    output:  $BIN"

CGO_ENABLED="${CGO_ENABLED:-0}" "$GO" build \
  -trimpath \
  -buildvcs=false \
  -tags "${GO_BUILD_TAGS:-goolm,stdjson}" \
  -ldflags "-s -w \
    -X ${CONFIG_PKG}.Version=${VERSION} \
    -X ${CONFIG_PKG}.GitCommit=${GIT_COMMIT} \
    -X ${CONFIG_PKG}.BuildTime=${BUILD_TIME} \
    -X ${CONFIG_PKG}.GoVersion=${GO_VERSION}" \
  -o "$BIN" "$CMD"

chmod +x "$BIN"
echo "==> build complete: $BIN"
"$BIN" version 2>/dev/null || true
