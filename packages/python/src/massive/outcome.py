from __future__ import annotations

from datetime import datetime
from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field


class FatalFailure(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    message: Annotated[str, Field(max_length=1024)]
    type: Annotated[str, Field(max_length=64)] | None = None


# What an exit hook receives once its run has settled, on every target. Its
# JSON schema is the shared contract conformance/schema/run-outcome.schema.json,
# which the Go compiler requires of an exit hook's input. The model has no
# docstring because Pydantic would copy it into that contract.
class RunOutcome(BaseModel):
    model_config = ConfigDict(frozen=True, extra="forbid")

    run_id: str
    # Argo reports a stopped workflow as failed; only the local target
    # distinguishes an operator's cancellation.
    status: Literal["succeeded", "failed", "cancelled"]
    # The node whose failure ended a failed run, when the target knows it.
    failed_node: str | None
    fatal: FatalFailure | None
    started_at: datetime
    finished_at: datetime
