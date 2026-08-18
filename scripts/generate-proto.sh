#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v protoc >/dev/null || {
  echo "generate-proto: protoc is required" >&2
  exit 1
}
command -v protoc-gen-go >/dev/null || {
  echo "generate-proto: protoc-gen-go is required; install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11" >&2
  exit 1
}
if [[ "$(protoc --version)" != "libprotoc 29.3" ]]; then
  echo "generate-proto: protoc 29.3 is required to reproduce checked-in bindings" >&2
  exit 1
fi
if [[ "$(protoc-gen-go --version)" != "protoc-gen-go v1.36.11" ]]; then
  echo "generate-proto: protoc-gen-go v1.36.11 is required to reproduce checked-in bindings" >&2
  exit 1
fi

cd "$repo_root/conformance/schema"
protoc \
  -I . \
  --go_out=paths=source_relative:planpb \
  workflow-plan.proto \
  bundle-manifest.proto
