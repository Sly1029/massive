from __future__ import annotations

from dataclasses import dataclass
from typing import Generic, TypeVar

DepsT = TypeVar("DepsT")
InputT = TypeVar("InputT")


@dataclass(frozen=True, slots=True)
class InvocationContext:
    run_id: str
    step_id: str
    idempotency_key: str


@dataclass(frozen=True, slots=True)
class StepContext(Generic[DepsT, InputT]):
    inputs: InputT
    deps: DepsT
    invocation: InvocationContext
