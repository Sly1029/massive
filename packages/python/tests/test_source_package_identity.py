from __future__ import annotations

import json
from pathlib import Path

import pytest
from pydantic import ValidationError

from massive.hashing import SourcePackageHashInput
from massive.source_package import SourcePackage, source_package


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
        ["../a.py"],
        ["src//a.py"],
        ["src/../a.py"],
        ["src/"],
    ],
)
def test_source_package_identity_rejects_noncanonical_paths(paths: list[str]) -> None:
    with pytest.raises(ValidationError, match="normalized relative path|UTF-16"):
        SourcePackageHashInput.model_validate(
            {"files": [{"path": path, "hash": "sha256:" + "a" * 64} for path in paths]}
        )


def test_source_package_identity_rejects_empty_files_and_invalid_hashes() -> None:
    with pytest.raises(ValidationError):
        SourcePackageHashInput(files=())
    with pytest.raises(ValidationError, match="pattern"):
        SourcePackageHashInput.model_validate(
            {"files": [{"path": "a.py", "hash": "sha256:not-a-digest"}]}
        )


def test_project_source_includes_nested_modules_resources_and_dependency_inputs(tmp_path: Path) -> None:
    (tmp_path / "workflow.py").write_text("value = 1\n")
    (tmp_path / "steps").mkdir()
    (tmp_path / "steps/__init__.py").write_text("")
    (tmp_path / "steps/format.py").write_text("value = 2\n")
    resource = tmp_path / "steps/prompt.txt"
    resource.write_text("first")
    (tmp_path / ".env").write_text("NOT_A_CREDENTIAL=fixture\n")
    project = tmp_path / "pyproject.toml"
    project.write_text(
        '[project]\nname = "example"\n'
        '[tool.other]\nsetting = true\n'
        '[tool.massive.source]\ninclude = ["*.py", "steps/**/*.py", "steps/*.txt"]\n'
    )
    lock = tmp_path / "uv.lock"
    lock.write_text("version = 1\n")

    files, original = SourcePackage.from_project(tmp_path).manifest()
    assert [file["path"] for file in files] == [
        "pyproject.toml", "steps/__init__.py", "steps/format.py", "steps/prompt.txt",
        "uv.lock", "workflow.py",
    ]
    resource.write_text("second")
    _, changed_resource = SourcePackage.from_project(tmp_path).manifest()
    assert changed_resource != original
    lock.write_text("version = 2\n")
    _, changed_lock = SourcePackage.from_project(tmp_path).manifest()
    assert changed_lock != changed_resource
    project.write_text(project.read_text() + "# configuration edit\n")
    _, changed_project = SourcePackage.from_project(tmp_path).manifest()
    assert changed_project != changed_lock


def test_project_configuration_does_not_leak_between_workflow_directories(tmp_path: Path) -> None:
    (tmp_path / "pyproject.toml").write_text(
        '[tool.massive.source]\ninclude = ["not-the-child.py"]\n'
    )
    for name in ("first", "second"):
        root = tmp_path / name
        root.mkdir()
        (root / "workflow.py").write_text(f'name = "{name}"\n')
        (root / "asset.txt").write_text(name)
    (tmp_path / "first/pyproject.toml").write_text(
        '[tool.massive.source]\ninclude = ["workflow.py", "asset.txt"]\n'
    )

    first, _ = SourcePackage.from_project(tmp_path / "first").manifest()
    second, _ = SourcePackage.from_project(tmp_path / "second").manifest()
    assert [file["path"] for file in first] == ["asset.txt", "pyproject.toml", "workflow.py"]
    assert [file["path"] for file in second] == ["workflow.py"]


@pytest.mark.parametrize("config", [
    '[tool.massive.source]\ninclude = []',
    '[tool.massive.source]\ninclude = "*.py"',
    '[tool.massive.source]\ninclude = [1]',
    '[tool.massive.source]\ninclude = [""]',
    '[tool.massive.source]\nincludes = ["*.py"]',
    '[tool.massive]\nsources = {}',
])
def test_invalid_source_configuration_is_rejected(tmp_path: Path, config: str) -> None:
    (tmp_path / "pyproject.toml").write_text(config)
    with pytest.raises(ValidationError):
        SourcePackage.from_project(tmp_path)


@pytest.mark.parametrize("pattern", ["../*.py", "/tmp/*.py"])
def test_source_patterns_cannot_escape_the_package(tmp_path: Path, pattern: str) -> None:
    with pytest.raises(ValueError, match="within the workflow directory"):
        source_package(root=tmp_path, include=[pattern], package_id="test").manifest()


@pytest.mark.parametrize("directory", [False, True])
def test_source_rejects_selected_symlinks(tmp_path: Path, directory: bool) -> None:
    target = tmp_path / "actual"
    target.mkdir()
    (target / "step.py").write_text("value = 1\n")
    link = tmp_path / "linked"
    link.symlink_to(target if directory else target / "step.py", target_is_directory=directory)
    with pytest.raises(ValueError, match="symlinks"):
        source_package(
            root=tmp_path, include=["linked/*.py" if directory else "linked"], package_id="test"
        ).manifest()


def test_project_metadata_symlink_is_rejected_before_parsing(tmp_path: Path) -> None:
    target = tmp_path / "not-project-metadata"
    target.write_text("this is not valid TOML")
    (tmp_path / "pyproject.toml").symlink_to(target)
    with pytest.raises(ValueError, match="symlinks: pyproject.toml"):
        SourcePackage.from_project(tmp_path)
