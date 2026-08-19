from __future__ import annotations

from pathlib import PurePosixPath
from typing import Literal, Self, cast

from pydantic import BaseModel, ConfigDict, Field, model_validator

from .canonical import JsonValue, canonical_json, sha256_ref, utf16_sort_key


class HashingSpec(BaseModel):
    """Versioned recipe for one persisted content identity."""

    model_config = ConfigDict(frozen=True, extra="forbid", populate_by_name=True)

    algorithm: Literal["sha256"] = "sha256"
    canonicalization: Literal["canonical-json-v0"] = "canonical-json-v0"
    recipe: Literal["workflow-spec", "workflow-plan", "source-package"]
    recipe_version: Literal[1] = Field(default=1, alias="recipeVersion")

    def as_json(self) -> dict[str, JsonValue]:
        return cast(dict[str, JsonValue], self.model_dump(mode="json", by_alias=True))


WORKFLOW_SPEC_HASHING = HashingSpec(recipe="workflow-spec")
SOURCE_PACKAGE_HASHING = HashingSpec(recipe="source-package")


class SourcePackageFileHash(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    path: str
    hash: str


class SourcePackageHashInput(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid", populate_by_name=True)

    files: tuple[SourcePackageFileHash, ...]
    hashing: HashingSpec = SOURCE_PACKAGE_HASHING
    kind: Literal["SourcePackageHashInput"] = "SourcePackageHashInput"
    schema_version: Literal[0] = Field(default=0, alias="schemaVersion")

    @model_validator(mode="after")
    def validate_canonical_files(self) -> Self:
        for index, file in enumerate(self.files):
            normalized = PurePosixPath(file.path).as_posix()
            segments = file.path.split("/")
            if (
                not file.path
                or "\\" in file.path
                or file.path.startswith("/")
                or any(segment in {"", ".", ".."} for segment in segments)
                or normalized != file.path
                or file.path == "."
            ):
                raise ValueError(
                    f"source package file {index} path is not a normalized relative path"
                )
            if index > 0 and utf16_sort_key(self.files[index - 1].path) >= utf16_sort_key(file.path):
                raise ValueError(
                    "source package files must have unique paths in UTF-16 code-unit order"
                )
        return self

    def digest(self) -> str:
        value = cast(JsonValue, self.model_dump(mode="json", by_alias=True))
        return sha256_ref(canonical_json(value))
