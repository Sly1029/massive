from __future__ import annotations

import json
from pathlib import Path

import pytest
from pydantic import ValidationError

from massive.hashing import SourcePackageHashInput
from massive.source_package import source_package


def test_source_package_hash_consumes_the_versioned_shared_recipe_vector() -> None:
    repository = Path(__file__).resolve().parents[3]
    fixture = repository / "conformance/fixtures/hashing/source-package-v1.json"
    expected = fixture.with_suffix(".sha256").read_text().strip()
    value = SourcePackageHashInput.model_validate(json.loads(fixture.read_text()))

    assert value.digest() == expected


def test_source_package_orders_paths_by_utf16_code_units(tmp_path: Path) -> None:
    (tmp_path / "\U0001f600.py").write_text("emoji = True\n")
    (tmp_path / "\ue000.py").write_text("private_use = True\n")

    files, _ = source_package(
        root=tmp_path,
        include=["*.py"],
        package_id="python-main",
    ).manifest()

    assert [file["path"] for file in files] == ["\U0001f600.py", "\ue000.py"]


@pytest.mark.parametrize(
    "paths",
    [
        ["b.py", "a.py"],
        ["a.py", "a.py"],
        ["./a.py"],
        ["src//a.py"],
        ["src/../a.py"],
    ],
)
def test_source_package_identity_rejects_noncanonical_paths(paths: list[str]) -> None:
    with pytest.raises(ValidationError, match="normalized relative path|UTF-16"):
        SourcePackageHashInput.model_validate(
            {"files": [{"path": path, "hash": "sha256:" + "a" * 64} for path in paths]}
        )
