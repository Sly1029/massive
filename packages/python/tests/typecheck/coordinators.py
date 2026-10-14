from __future__ import annotations

from typing import assert_type

from pydantic import BaseModel

from massive import (
    MapItemFailed,
    MapItemOutcome,
    MapItemSucceeded,
    StepContext,
    StepFailed,
    Workflow,
)


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


def sync_step(context: StepContext[Request]) -> Result:
    return Result(value=context.inputs.value)


async def async_step(context: StepContext[Request]) -> str:
    return str(context.inputs.value)


async def coordinator(wf: Workflow, request: Request) -> list[MapItemOutcome[Result]]:
    assert_type(await wf.run(sync_step, request), Result)
    assert_type(await wf.run(async_step, request), str)
    pair = await wf.gather(wf.run(sync_step, request), wf.run(async_step, request))
    assert_type(pair, tuple[Result, str])
    assert_type(pair[0], Result)
    assert_type(pair[1], str)
    values = await wf.gather([wf.run(sync_step, request, key=str(index)) for index in range(2)])
    assert_type(values, list[Result])
    assert_type(values[0], Result)
    async for value in wf.as_completed([wf.run(sync_step, request)]):
        assert_type(value, Result)
    await wf.run(sync_step, "wrong")  # pyright: ignore[reportCallIssue,reportArgumentType]  # ty: ignore[no-matching-overload]
    await wf.run(async_step, 123)  # pyright: ignore[reportCallIssue,reportArgumentType]  # ty: ignore[no-matching-overload]
    wrong: int = await wf.run(async_step, request)  # pyright: ignore[reportAssignmentType]  # ty: ignore[invalid-assignment]
    assert_type(wrong, int)
    try:
        result = await wf.run(sync_step, request)
        return [MapItemSucceeded(status="succeeded", value=result)]
    except StepFailed as error:
        return [MapItemFailed(status="failed", failure=error.failure)]
