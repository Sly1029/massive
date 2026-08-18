from __future__ import annotations

import subprocess
import zipfile
from pathlib import Path


def test_wheel_packages_the_canonical_workflow_spec_schema(tmp_path: Path) -> None:
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
    wheel = next(tmp_path.glob("massive-*.whl"))
    with zipfile.ZipFile(wheel) as archive:
        packaged = archive.read("massive/schemas/workflow-spec.schema.json")

    canonical = (repository / "conformance/schema/workflow-spec.schema.json").read_bytes()
    assert packaged == canonical
