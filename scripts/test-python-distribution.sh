#!/usr/bin/env bash
set -euo pipefail

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/massive-distribution.XXXXXX")"

cleanup() {
  case "$test_root" in
    "${TMPDIR:-/tmp}"/massive-distribution.*)
      find "$test_root" -type d -exec chmod 755 {} + 2>/dev/null || true
      rm -rf -- "$test_root"
      ;;
  esac
}
trap cleanup EXIT

uv build --out-dir "$test_root/dist" "$repository/packages/python"
wheel="$(find "$test_root/dist" -maxdepth 1 -name '*.whl' -print -quit)"
test -n "$wheel"

uv venv "$test_root/venv"
python="$test_root/venv/bin/python"
massive="$test_root/venv/bin/massive"
uv pip install --python "$python" "$wheel"

"$python" -c 'from importlib.metadata import version; assert version("massive-workflows") == "0.1.0"'
test "$("$massive" version)" = "massive 0.1.0"

mkdir -p "$test_root/project"
cp "$repository/packages/cli/test/fixtures/python-linear/workflow.py" "$test_root/project/workflow.py"
cp "$repository/packages/cli/test/fixtures/python-linear/helper.py" "$test_root/project/helper.py"

run_result="$(
  cd "$test_root/project"
  "$massive" run workflow.py \
    --input '{"value": 41}' \
    --store "$test_root/store" \
    --project massive/distribution-test \
    --run-id clean-wheel \
    --json \
    --verbose
)"

bundle_result="$(
  cd "$test_root/project"
  "$massive" build workflow.py \
    --target argo \
    --output "$test_root/bundle" \
    --namespace workflows \
    --service-account massive-runner \
    --artifact-store massive-artifacts \
    --name python-linear \
    --json
)"

"$python" - "$run_result" "$bundle_result" "$test_root/bundle" <<'PY'
import json
import sys
from pathlib import Path

run = json.loads(sys.argv[1])
bundle = json.loads(sys.argv[2])
bundle_root = Path(sys.argv[3])

assert run["status"] == "succeeded", run
assert run["result"] == {"value": 42}, run
assert bundle["status"] == "built", bundle
assert bundle["planHash"] == run["planHash"], (run, bundle)
for name in (
    "bundle-manifest.json",
    "deployment-spec.json",
    "massive-plan.json",
    "workflow-spec.json",
    "workflow-template.yaml",
):
    assert (bundle_root / name).is_file(), name
PY
