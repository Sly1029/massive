from __future__ import annotations

from typing import Literal, cast

from pydantic import BaseModel, ConfigDict, Field

from .canonical import JsonValue, canonical_json, sha256_ref


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

    files: list[SourcePackageFileHash]
    hashing: HashingSpec = SOURCE_PACKAGE_HASHING
    kind: Literal["SourcePackageHashInput"] = "SourcePackageHashInput"
    schema_version: Literal[0] = Field(default=0, alias="schemaVersion")

    def digest(self) -> str:
        value = cast(JsonValue, self.model_dump(mode="json", by_alias=True))
        return sha256_ref(canonical_json(value))
