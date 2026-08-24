#!/usr/bin/env bash
set -euo pipefail

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${1:-$repository/dist/python-release}"
mkdir -p "$output"
if find "$output" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  echo "build-python-release: output directory must be empty: $output" >&2
  exit 1
fi

uv build --sdist --out-dir "$output" "$repository/packages/python"
for target in \
  linux/amd64 \
  linux/arm64 \
  darwin/amd64 \
  darwin/arm64 \
  windows/amd64 \
  windows/arm64
do
  MASSIVE_BUILD_GOOS="${target%/*}" \
  MASSIVE_BUILD_GOARCH="${target#*/}" \
    uv build --wheel --out-dir "$output" "$repository/packages/python"
done

uv run --no-project python - "$output" <<'PY'
import sys
import tarfile
import zipfile
from pathlib import Path

output = Path(sys.argv[1])
wheels = sorted(output.glob("massive_workflows-0.1.0-*.whl"))
sdists = list(output.glob("massive_workflows-0.1.0.tar.gz"))
expected_tags = {
    "manylinux_2_17_x86_64",
    "manylinux_2_17_aarch64",
    "macosx_10_15_x86_64",
    "macosx_11_0_arm64",
    "win_amd64",
    "win_arm64",
}
actual_tags = {wheel.stem.removeprefix("massive_workflows-0.1.0-py3-none-") for wheel in wheels}
assert actual_tags == expected_tags, (actual_tags, expected_tags)
assert len(sdists) == 1

for wheel in wheels:
    with zipfile.ZipFile(wheel) as archive:
        names = set(archive.namelist())
        assert {"massive/_bin/massive", "massive/_bin/massive.exe"} & names, wheel
        wheel_metadata = next(
            archive.read(name).decode()
            for name in names
            if name.endswith(".dist-info/WHEEL")
        )
        assert "Root-Is-Purelib: false" in wheel_metadata, wheel

with tarfile.open(sdists[0]) as archive:
    names = set(archive.getnames())
    root = "massive_workflows-0.1.0"
    for required in ("go.mod", "cmd/massive/main.go", "internal/controlplane/controlplane.go"):
        assert f"{root}/{required}" in names, required
PY

echo "Built Massive 0.1.0 release artifacts in $output"
