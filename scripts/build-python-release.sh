#!/usr/bin/env bash
set -euo pipefail

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${1:-$repository/dist/python-release}"
mkdir -p "$output"
if find "$output" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  echo "build-python-release: output directory must be empty: $output" >&2
  exit 1
fi

# PEP 639 requires the license inside the package directory; keep it identical.
cmp "$repository/LICENSE" "$repository/packages/python/LICENSE"

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

version="$(uv run --no-project python -c 'import sys, tomllib; print(tomllib.load(open(sys.argv[1], "rb"))["project"]["version"])' "$repository/packages/python/pyproject.toml")"

uv run --no-project python - "$output" "$version" <<'PY'
import sys
import tarfile
import zipfile
from pathlib import Path

output = Path(sys.argv[1])
version = sys.argv[2]
prefix = f"massive_workflows-{version}"
wheels = sorted(output.glob(f"{prefix}-*.whl"))
sdists = list(output.glob(f"{prefix}.tar.gz"))
expected_tags = {
    "manylinux_2_17_x86_64",
    "manylinux_2_17_aarch64",
    "macosx_13_0_x86_64",
    "macosx_13_0_arm64",
    "win_amd64",
    "win_arm64",
}
actual_tags = {wheel.stem.removeprefix(f"{prefix}-py3-none-") for wheel in wheels}
assert actual_tags == expected_tags, (actual_tags, expected_tags)
assert len(sdists) == 1

for wheel in wheels:
    with zipfile.ZipFile(wheel) as archive:
        names = set(archive.namelist())
        assert {"massive/_bin/massive", "massive/_bin/massive.exe"} & names, wheel
        assert "massive/py.typed" in names, wheel
        assert "massive/AGENTS.md" not in names, wheel
        assert f"{prefix}.dist-info/licenses/LICENSE" in names, wheel
        wheel_metadata = archive.read(f"{prefix}.dist-info/WHEEL").decode()
        assert "Root-Is-Purelib: false" in wheel_metadata, wheel

with tarfile.open(sdists[0]) as archive:
    names = set(archive.getnames())
    for required in ("LICENSE", "go.mod", "cmd/massive/main.go", "internal/controlplane/controlplane.go"):
        assert f"{prefix}/{required}" in names, required
PY

# The source distribution is the fallback for unsupported platforms, so it must
# rebuild a working native wheel without the checkout.
sdist_check="$(mktemp -d "${TMPDIR:-/tmp}/massive-sdist.XXXXXX")"
trap 'rm -rf -- "$sdist_check"' EXIT
uv build --wheel --out-dir "$sdist_check" "$output/massive_workflows-$version.tar.gz"
uv run --no-project python - "$sdist_check" <<'PY'
import sys
import zipfile
from pathlib import Path

(wheel,) = Path(sys.argv[1]).glob("*.whl")
with zipfile.ZipFile(wheel) as archive:
    assert {"massive/_bin/massive", "massive/_bin/massive.exe"} & set(archive.namelist()), wheel
PY

echo "Built Massive $version release artifacts in $output"
