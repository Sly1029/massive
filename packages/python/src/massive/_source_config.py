"""Validated workflow source selection, loaded only when reading a project."""

from typing import Annotated

from pydantic import BaseModel, ConfigDict, Field, StrictStr


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


class SourceProjectConfig(BaseModel):
    tool: _ProjectTools = Field(default_factory=_ProjectTools)
