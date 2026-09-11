#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
for target in FuzzWorkflowParsing FuzzGraphShapes FuzzDecisionGraphs; do
  go test ./internal/plan -run '^$' -fuzz "^${target}$" \
    -fuzztime="${FUZZ_TIME:-30s}" -parallel="${FUZZ_WORKERS:-2}"
done
