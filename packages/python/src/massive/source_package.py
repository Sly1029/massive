from __future__ import annotations

import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import Annotated

from pydantic import BaseModel, ConfigDict, Field, StrictStr

from .canonical import sha256_ref, utf16_sort_key
from .hashing import SourcePackageFileHash, SourcePackageHashInput


class _SourceConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    include: tuple[Annotated[StrictStr, Field(min_length=1)], ...] = Field(
        default=("*.py",), min_length=1
    )


class _MassiveConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    source: _SourceConfig = Field(default_factory=_SourceConfig)


class _ProjectTools(BaseModel):
    massive: _MassiveConfig = Field(default_factory=_MassiveConfig)


class _Pyproject(BaseModel):
    tool: _ProjectTools = Field(default_factory=_ProjectTools)


@dataclass(frozen=True, slots=True)
class SourcePackage:
    root: Path
    include: tuple[str, ...]
    package_id: str

    @classmethod
    def from_project(cls, root: Path) -> SourcePackage:
        """Read only this workflow directory's pyproject; never inherit parent config."""
        project_file = root / "pyproject.toml"
        if project_file.is_symlink():
            raise ValueError("source package must not include symlinks: pyproject.toml")
        config = (
            _Pyproject.model_validate(tomllib.loads(project_file.read_text(encoding="utf-8")))
            if project_file.is_file()
            else _Pyproject()
        )
        # Keep dependency declarations and lock inputs in source identity even when
        # the author customizes inclusion. They are not installed by the runner.
        include = (*config.tool.massive.source.include, "pyproject.toml", "uv.lock")
        return source_package(root=root, include=list(include), package_id="python-main")

    def manifest(self) -> tuple[list[dict[str, str]], str]:
        root = self.root.resolve(strict=True)
        files: dict[str, Path] = {}
        for pattern in self.include:
            if Path(pattern).is_absolute() or ".." in Path(pattern).parts:
                raise ValueError("source include patterns must stay within the workflow directory")
            for candidate in root.glob(pattern):
                relative = candidate.relative_to(root)
                if any((root / part).is_symlink() for part in (relative, *relative.parents)):
                    raise ValueError(f"source package must not include symlinks: {relative}")
                if not candidate.is_file():
                    continue
                files[relative.as_posix()] = candidate
        if not files:
            raise ValueError("source package include did not select any files")
        identity_files = [
            SourcePackageFileHash(path=path, hash=sha256_ref(file.read_bytes()))
            for path, file in sorted(files.items(), key=lambda item: utf16_sort_key(item[0]))
        ]
        entries = [file.model_dump(mode="json") for file in identity_files]
        return entries, SourcePackageHashInput(files=tuple(identity_files)).digest()


def source_package(*, root: Path, include: list[str], package_id: str) -> SourcePackage:
    if not include:
        raise ValueError("source package include must not be empty")
    if not package_id:
        raise ValueError("source package id must not be empty")
    return SourcePackage(root=root, include=tuple(include), package_id=package_id)
