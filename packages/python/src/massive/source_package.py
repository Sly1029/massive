from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import cast

from .canonical import JsonValue, canonical_json, sha256_ref


@dataclass(frozen=True, slots=True)
class SourcePackage:
    root: Path
    include: tuple[str, ...]
    package_id: str

    def manifest(self) -> tuple[list[dict[str, str]], str]:
        root = self.root.resolve(strict=True)
        files: dict[str, Path] = {}
        for pattern in self.include:
            for candidate in root.glob(pattern):
                resolved = candidate.resolve(strict=True)
                if not resolved.is_file() or root not in resolved.parents:
                    continue
                files[resolved.relative_to(root).as_posix()] = resolved
        if not files:
            raise ValueError("source package include did not select any files")
        entries = [
            {"path": path, "hash": sha256_ref(file.read_bytes())}
            for path, file in sorted(files.items())
        ]
        return entries, sha256_ref(canonical_json(cast(JsonValue, entries)))


def source_package(*, root: Path, include: list[str], package_id: str) -> SourcePackage:
    if not include:
        raise ValueError("source package include must not be empty")
    if not package_id:
        raise ValueError("source package id must not be empty")
    return SourcePackage(root=root, include=tuple(include), package_id=package_id)
