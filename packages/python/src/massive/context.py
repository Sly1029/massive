from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Generic, TypeVar

if TYPE_CHECKING:
    from .identity import MapItemIdentity

InputT = TypeVar("InputT")


class FatalRunError(Exception):
    """Stop the whole run with an explicitly safe category and message.

    Neither retries nor collecting maps absorb this signal. Chained exceptions
    are never included in its durable cause; supply only text safe to persist.
    """

    def __init__(self, category: str, message: str) -> None:
        from typing import Annotated

        from pydantic import Field, TypeAdapter

        self.category = TypeAdapter[str](
            Annotated[str, Field(strict=True, pattern=r"^[a-z][a-z0-9_.-]{0,63}$")]
        ).validate_python(category)
        safe_message = TypeAdapter[str](
            Annotated[str, Field(strict=True, max_length=1024)]
        ).validate_python(message)
        super().__init__(safe_message)


class NonRetryableError(Exception):
    """Fail the step without further retries.

    Raise it, a subclass, or ``raise NonRetryableError(...) from error`` when another
    attempt cannot succeed. An exception group containing one, such as a failed
    ``asyncio.TaskGroup``, is not retried either.
    """


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
