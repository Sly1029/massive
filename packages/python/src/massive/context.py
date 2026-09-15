from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Generic, TypeVar

InputT = TypeVar("InputT")


class NonRetryableError(Exception):
    """Fail the step without further retries.

    Raise it, a subclass, or ``raise NonRetryableError(...) from error`` when another
    attempt cannot succeed.
    """


@dataclass(frozen=True, slots=True)
class InvocationContext:
    run_id: str
    step_id: str
    idempotency_key: str
    attempt: int
    max_attempts: int


@dataclass(frozen=True, slots=True)
class StepContext(Generic[InputT]):
    inputs: InputT
    invocation: InvocationContext
    workspace: Path
