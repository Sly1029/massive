from __future__ import annotations

import builtins
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Generic, TypeVar

if TYPE_CHECKING:
    from .identity import MapItemIdentity

InputT = TypeVar("InputT")


class NonRetryableError(Exception):
    """Fail the step without further retries.

    Raise it, a subclass, or ``raise NonRetryableError(...) from error`` when another
    attempt cannot succeed. An exception group containing one, such as a failed
    ``asyncio.TaskGroup``, is not retried either.
    """


class FatalRunError(NonRetryableError):
    """Stop the whole run with a safe message and optional error type.

    Neither retries nor collecting maps absorb this signal. Chained exceptions
    are never included in its durable cause; supply only text safe to persist.
    Oversized text is truncated, and invalid inputs use a safe fallback.
    """

    def __init__(self, message: object, type: object = None) -> None:
        safe_message = (
            str.__getitem__(message, slice(0, 1024))
            if builtins.type(message) is str
            else "Fatal run failure"
        )
        self.type: str | None = (
            str.__getitem__(type, slice(0, 64)) if builtins.type(type) is str else None
        )
        super().__init__(safe_message)


@dataclass(frozen=True, slots=True)
class InvocationContext:
    run_id: str
    step_id: str
    idempotency_key: str
    attempt: int
    max_attempts: int
    item: MapItemIdentity | None


@dataclass(frozen=True, slots=True)
class StepContext(Generic[InputT]):
    inputs: InputT
    invocation: InvocationContext
    workspace: Path
