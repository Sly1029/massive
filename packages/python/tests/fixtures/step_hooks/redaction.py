from __future__ import annotations

from collections.abc import Callable
from functools import wraps

from pydantic import BaseModel

from massive import StepContext


def redacted[InputT, OutputT: BaseModel](
    step: Callable[[StepContext[InputT]], OutputT],
) -> Callable[[StepContext[InputT]], OutputT]:
    """A post-step hook: mask a secret marker in every string field of the output."""

    @wraps(step)
    def hooked(context: StepContext[InputT]) -> OutputT:
        output = step(context)
        return output.model_copy(
            update={
                name: value.replace("secret", "[redacted]")
                for name, value in output
                if isinstance(value, str)
            }
        )

    return hooked
