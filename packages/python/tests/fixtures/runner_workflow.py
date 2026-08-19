from __future__ import annotations

from decimal import Decimal

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


class DecimalResult(BaseModel):
    value: Decimal


class InvocationResult(BaseModel):
    idempotency_key: str


graph = GraphBuilder(
    name="runner-fixture",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
        )
    ),
)


@graph.step()
def double(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value * 2)


@graph.step()
async def increment(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


@graph.step()
def capture_sync_invocation(context: StepContext[None, Request]) -> InvocationResult:
    return InvocationResult(idempotency_key=context.invocation.idempotency_key)


@graph.step()
async def capture_async_invocation(context: StepContext[None, Request]) -> InvocationResult:
    return InvocationResult(idempotency_key=context.invocation.idempotency_key)


@graph.step()
def explode(context: StepContext[None, Request]) -> Result:
    raise RuntimeError("intentional runner failure")


@graph.step()
def invalid_output(context: StepContext[None, Request]) -> Result:
    return {"value": -1}  # type: ignore[return-value]


@graph.step()
def decimal_result(context: StepContext[None, Request]) -> DecimalResult:
    return DecimalResult(value=Decimal(context.inputs.value) / Decimal(2))


@graph.step()
def decimal_echo(context: StepContext[None, DecimalResult]) -> DecimalResult:
    return context.inputs
