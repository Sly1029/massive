#!/usr/bin/env bash
set -euo pipefail

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="$(cat "$repository/conformance/minio/image-reference")"
commit="${image##*:}"
# Snap Docker cannot read /tmp or hidden checkout directories.
scratch="$(mktemp -d "${TMPDIR:-$HOME}/massive-minio-build.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
mkdir "$scratch/tmp"
chmod 1777 "$scratch/tmp"

source_root="$(go mod download -json "github.com/minio/minio@$commit" |
  uv run --no-project python -c 'import json, sys; print(json.load(sys.stdin)["Dir"])')"
CGO_ENABLED=0 GOTOOLCHAIN=go1.24.2 go build -C "$source_root" -mod=readonly -trimpath -buildvcs=false \
  -o "$scratch/minio" .
cp "$repository/conformance/minio/Dockerfile" "$scratch/Dockerfile"
docker build --tag "$image" --iidfile "$scratch/image-id" "$scratch" >&2
cat "$scratch/image-id"
