"""Pydantic-backed identities shared by workflow authoring and artifact publication."""

from __future__ import annotations

from typing import Annotated, Literal

from pydantic import (
    AfterValidator,
    BaseModel,
    ConfigDict,
    Field,
    StrictInt,
    StrictStr,
    StringConstraints,
    TypeAdapter,
)


def _not_dot_segment(value: str) -> str:
    if value in {".", ".."}:
        raise ValueError("must not be '.' or '..'")
    return value


type SafePathSegment = Annotated[
    StrictStr,
    StringConstraints(pattern=r"^[A-Za-z0-9_.@:#-]+$", min_length=1, max_length=128),
    AfterValidator(_not_dot_segment),
]
type Sha256Reference = Annotated[
    StrictStr,
    StringConstraints(pattern=r"^sha256:[0-9a-f]{64}$"),
]
type ProjectKey = Annotated[
    StrictStr,
    StringConstraints(pattern=r"^sha256-[0-9a-f]{64}$"),
]
type PositiveAttempt = Annotated[StrictInt, Field(ge=1, le=(1 << 53) - 1)]
type ScopeIndex = Annotated[StrictInt, Field(ge=0, le=(1 << 53) - 1)]


class MapItemScopeFrame(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    kind: Literal["map-item"]
    map_id: SafePathSegment = Field(validation_alias="mapId", serialization_alias="mapId")
    index: ScopeIndex


class ExecutionScope(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    frames: tuple[MapItemScopeFrame, ...] = Field(min_length=1)

SAFE_PATH_SEGMENT: TypeAdapter[str] = TypeAdapter(SafePathSegment)
SHA256_REFERENCE: TypeAdapter[str] = TypeAdapter(Sha256Reference)
