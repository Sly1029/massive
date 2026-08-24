from __future__ import annotations

import subprocess
import zipfile
from pathlib import Path


def test_platform_wheel_packages_the_control_plane_and_canonical_schema(
    tmp_path: Path,
) -> None:
    repository = Path(__file__).resolve().parents[3]
    result = subprocess.run(
        [
            "uv",
            "build",
            "--wheel",
            "--out-dir",
            str(tmp_path),
        ],
        cwd=repository / "packages/python",
        check=False,
        capture_output=True,
        text=True,
    )

    assert result.returncode == 0, result.stderr
    wheel = next(tmp_path.glob("massive_workflows-*.whl"))
    with zipfile.ZipFile(wheel) as archive:
        packaged = archive.read("massive/schemas/workflow-spec.schema.json")
        binary = next(
            info
            for info in archive.infolist()
            if info.filename in {"massive/_bin/massive", "massive/_bin/massive.exe"}
        )
        wheel_metadata = next(
            archive.read(info).decode()
            for info in archive.infolist()
            if info.filename.endswith(".dist-info/WHEEL")
        )

    canonical = (repository / "conformance/schema/workflow-spec.schema.json").read_bytes()
    assert packaged == canonical
    assert binary.file_size > 0
    assert "Root-Is-Purelib: false" in wheel_metadata
    assert "Tag: py3-none-" in wheel_metadata
