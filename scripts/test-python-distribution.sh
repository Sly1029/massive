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

mkdir -p "$test_root/runtime-mount"
"$python" - "$test_root/bundle/runtime-configmap.json" "$test_root/runtime-mount" <<'PY'
import base64
import json
import sys
from pathlib import Path

config = json.loads(Path(sys.argv[1]).read_text())
mount = Path(sys.argv[2])
for name, body in config["binaryData"].items():
    (mount / name).write_bytes(base64.b64decode(body, validate=True))
PY

"$massive" runtime step \
  --plan "$test_root/runtime-mount/massive-plan.json" \
  --bundle-dir "$test_root/runtime-mount" \
  --node add_one \
  --input '{"value": 41}' \
  --output "$test_root/remote-result.json" \
  --project argo/python-linear \
  --run-id isolated-wheel \
  --store "$test_root/remote-store"

"$python" - "$run_result" "$bundle_result" "$test_root/bundle" <<'PY'
import base64
import hashlib
import io
import json
import sys
import tarfile
from pathlib import Path

run = json.loads(sys.argv[1])
bundle = json.loads(sys.argv[2])
bundle_root = Path(sys.argv[3])

assert run["status"] == "succeeded", run
assert run["result"] == {"value": 42}, run
assert bundle["status"] == "built", bundle
assert bundle["runtimeTransport"] == "embedded-v0", bundle
assert bundle["planHash"] == run["planHash"], (run, bundle)
for name in (
    "bundle-manifest.json",
    "deployment-spec.json",
    "massive-plan.json",
    "runtime-configmap.json",
    "workflow-spec.json",
    "workflow-template.json",
    "workflow-template.yaml",
):
    assert (bundle_root / name).is_file(), name

plan = json.loads((bundle_root / "massive-plan.json").read_text())
package_hash = plan["sourcePackages"][0]["packageHash"]
archive_name = f"source-sha256-{package_hash.removeprefix('sha256:')}.tar"
archive_path = bundle_root / "runtime-assets" / archive_name
archive = archive_path.read_bytes()
config_map = json.loads((bundle_root / "runtime-configmap.json").read_text())
assert config_map["immutable"] is True
assert base64.b64decode(config_map["binaryData"][archive_name], validate=True) == archive

template = json.loads((bundle_root / "workflow-template.json").read_text())
assert template["apiVersion"] == "argoproj.io/v1alpha1"
assert template["kind"] == "WorkflowTemplate"
assert template["spec"]["serviceAccountName"] == "massive-runner"
assert template["spec"]["automountServiceAccountToken"] is False
assert template["spec"]["entrypoint"] == "main"
step = next(item for item in template["spec"]["templates"] if "container" in item)
assert step["name"].startswith("step-add-one-")
assert step["nodeSelector"] == {
    "kubernetes.io/arch": "amd64",
    "kubernetes.io/os": "linux",
}
assert step["container"]["command"] == ["massive"]
assert step["container"]["args"][:2] == ["runtime", "step"]
assert step["container"]["volumeMounts"] == [
    {"mountPath": "/var/run/massive", "name": "massive-runtime", "readOnly": True}
]

with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as source:
    members = source.getmembers()
assert [member.name for member in members] == ["helper.py", "workflow.py"]
assert all(
    member.isfile()
    and member.mode == 0o644
    and member.mtime == 0
    and member.uid == 0
    and member.gid == 0
    for member in members
)

manifest = json.loads((bundle_root / "bundle-manifest.json").read_text())
archive_entry = next(entry for entry in manifest["files"] if entry["path"] == f"runtime-assets/{archive_name}")
assert archive_entry["role"] == "source-archive"
assert archive_entry["artifact"] == {
    "key": f"runtime-assets/{archive_name}",
    "hash": f"sha256:{hashlib.sha256(archive).hexdigest()}",
    "contentType": "application/vnd.massive.source-tar",
}
assert json.loads((bundle_root.parent / "remote-result.json").read_text()) == {"value": 42}
PY

# A real workflow-local package and resource must work through the installed CLI,
# including map collection, and remain executable after the checkout moves.
cp -R "$repository/examples/07-package" "$test_root/packaged"
packaged_run="$(
  cd "$test_root/packaged"
  "$massive" run workflow.py \
    --input '{"values":[3,1,3]}' \
    --store "$test_root/packaged-store" \
    --project massive/distribution-test \
    --run-id packaged-wheel \
    --json
)"
(
  cd "$test_root/packaged"
  "$massive" build workflow.py \
    --output "$test_root/packaged-bundle" \
    --namespace workflows \
    --service-account massive-runner \
    --json
)
mv "$test_root/packaged" "$test_root/moved-checkout"
"$massive" runtime map item \
  --plan "$test_root/packaged-bundle/massive-plan.json" \
  --bundle-dir "$test_root/packaged-bundle/runtime-assets" \
  --node labels \
  --item '{"index":0,"value":7}' \
  --output "$test_root/packaged-result.json" \
  --project argo/packaged-example \
  --run-id isolated-package \
  --store "$test_root/packaged-remote-store"

"$python" - "$packaged_run" "$test_root" <<'PY'
import json
import sys
from pathlib import Path

run = json.loads(sys.argv[1])
root = Path(sys.argv[2])
assert run["status"] == "succeeded", run
assert run["result"] == {"labels": ["item:3", "item:1", "item:3"]}, run
assert json.loads((root / "packaged-result.json").read_text()) == {
    "index": 0, "value": "item:7",
}
spec = json.loads((root / "packaged-bundle/workflow-spec.json").read_text())
assert [file["path"] for file in spec["sourcePackages"]["python-main"]["files"]] == [
    "packaged_steps/__init__.py",
    "packaged_steps/formatters.py",
    "packaged_steps/prefix.txt",
    "pyproject.toml",
    "workflow.py",
]
PY
