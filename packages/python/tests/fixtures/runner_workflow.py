from __future__ import annotations

from decimal import Decimal
from pathlib import Path

from pydantic import BaseModel, field_validator

from massive import Blob, GraphBuilder, StepContext, container, execution


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


def double(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value * 2)


async def increment(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value + 1)


def capture_sync_invocation(context: StepContext[Request]) -> InvocationResult:
    return InvocationResult(idempotency_key=context.invocation.idempotency_key)


async def capture_async_invocation(context: StepContext[Request]) -> InvocationResult:
    return InvocationResult(idempotency_key=context.invocation.idempotency_key)


def explode(context: StepContext[Request]) -> Result:
    raise RuntimeError("intentional runner failure")


def invalid_output(context: StepContext[Request]) -> Result:
    return {"value": -1}  # type: ignore[return-value]


def decimal_result(context: StepContext[Request]) -> DecimalResult:
    return DecimalResult(value=Decimal(context.inputs.value) / Decimal(2))


def decimal_echo(context: StepContext[DecimalResult]) -> DecimalResult:
    return context.inputs


def changed_file(context: StepContext[Request]) -> Blob:
    path = context.workspace / "output.txt"
    path.write_text("snapshot")
    result = Blob.from_path(path)
    path.write_text("changed after snapshot")
    return result


async def lazy_increment(context: StepContext[Request]) -> Result:
    from lazy_step_helper import increment_value

    return Result(value=increment_value(context.inputs.value))


def workspace_file(ctx: StepContext[Request]) -> Blob:
    import json

    print(json.dumps(str(ctx.workspace)))
    assert ctx.workspace != Path(__file__).parent
    assert Path(__file__).parent not in ctx.workspace.parents
    path = ctx.workspace / "report.txt"
    path.write_text("workspace artifact")
    return Blob.from_path(path)


def workspace_failure(ctx: StepContext[Request]) -> Blob:
    workspace_file(ctx)
    raise RuntimeError("workspace failure")


def workspace_invalid_output(ctx: StepContext[Request]) -> Blob:
    blob = workspace_file(ctx)
    blob.path().write_text("changed after snapshot")
    return blob
