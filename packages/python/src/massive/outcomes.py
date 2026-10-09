"""Per-item outcomes of a map that collects item failures.

``graph.map(..., item_failures="collect")`` produces ``list[MapItemOutcome[R]]``
in source order. The Go compiler checks that this schema wraps the mapper's
output schema, and executors build exactly these records.
"""

from __future__ import annotations

from typing import Annotated, Generic, Literal, TypeVar

from pydantic import BaseModel, ConfigDict, Field

ValueT = TypeVar("ValueT")

# Fields are declared in sorted order and carry no defaults or docstrings, so
# validation and serialization emit one schema: the outcome contract in
# docs/spec/ir-and-datastore.md.


class MapItemFailure(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    # Attempts the item made, including the one that failed terminally.
    attempts: Annotated[int, Field(ge=1)]
    # The runner's failure summary and, for exceptions, the exception message;
    # at most 1,024 characters. It never contains other task output.
    diagnostic: Annotated[str, Field(max_length=1024)]
    # error: an exception or a nonzero exit; killed: ended by a signal, such
    # as an out-of-memory kill; non-retryable: NonRetryableError; timeout: the
    # attempt exceeded its per-attempt deadline.
    kind: Literal["error", "killed", "non-retryable", "timeout"]


class MapItemFailed(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    failure: MapItemFailure
    status: Literal["failed"]


class MapItemSucceeded(BaseModel, Generic[ValueT]):
    model_config = ConfigDict(extra="forbid", frozen=True)

    status: Literal["succeeded"]
    value: ValueT


type MapItemOutcome[OutcomeT] = Annotated[
    MapItemSucceeded[OutcomeT] | MapItemFailed, Field(discriminator="status")
]
