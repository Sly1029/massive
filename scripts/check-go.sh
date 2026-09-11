#!/usr/bin/env bash
set -euo pipefail
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repository"
export PATH="$repository/scripts:$PATH"
go vet ./...
go test ./...
