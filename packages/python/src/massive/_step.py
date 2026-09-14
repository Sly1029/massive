from __future__ import annotations

import inspect
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Generic, TypeVar, get_args, get_origin, get_type_hints

from .context import InputT, StepContext
from .contracts import ExecutionContract

OutputT = TypeVar("OutputT")


@dataclass(frozen=True, slots=True)
class StepDefinition(Generic[InputT, OutputT]):
    function: Callable[[StepContext[InputT]], OutputT | Awaitable[OutputT]]
    input_type: Any
    output_type: Any
    contract: ExecutionContract | None

    @classmethod
    def from_callable(
        cls,
        function: Callable[[StepContext[InputT]], OutputT | Awaitable[OutputT]],
        *,
        contract: ExecutionContract | None = None,
    ) -> StepDefinition[InputT, OutputT]:
        if (
            not inspect.isfunction(function)
            or function.__qualname__ != function.__name__
            or function.__name__ == "<lambda>"
        ):
            raise TypeError("workflow steps must be top-level named functions")
        hints = get_type_hints(function, include_extras=True)
        parameters = list(inspect.signature(function).parameters.values())
        if (
            len(parameters) != 1
            or parameters[0].name not in hints
            or parameters[0].kind
            not in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD)
        ):
            raise TypeError(
                "a workflow step requires one annotated positional StepContext parameter"
            )
        context_type = hints[parameters[0].name]
        if get_origin(context_type) is not StepContext:
            raise TypeError("a workflow step parameter must be StepContext[Input]")
        (input_type,) = get_args(context_type)
        if hints.get("return", inspect.Signature.empty) is inspect.Signature.empty:
            raise TypeError("a workflow step requires a return annotation")
        return cls(
            function=function,
            input_type=input_type,
            output_type=hints["return"],
            contract=contract,
        )
