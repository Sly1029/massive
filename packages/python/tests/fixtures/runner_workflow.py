from __future__ import annotations

from pydantic import BaseModel, field_validator

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int

    @field_validator("value")
    @classmethod
    def value_must_be_non_negative(cls, value: int) -> int:
        if value < 0:
            raise ValueError("value must be non-negative")
        return value


graph = GraphBuilder(
    name="runner-fixture",
    input_type=Request,
    output_type=Result,
    defaults=execution(environment=container("example.invalid/runner:latest")),
)


@graph.step()
def double(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value * 2)


@graph.step()
async def increment(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


@graph.step()
def explode(context: StepContext[None, Request]) -> Result:
    raise RuntimeError("intentional runner failure")


@graph.step()
def invalid_output(context: StepContext[None, Request]) -> Result:
    return {"value": -1}  # type: ignore[return-value]
