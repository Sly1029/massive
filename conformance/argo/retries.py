from __future__ import annotations

from datetime import timedelta

from massive import GraphBuilder, NonRetryableError, StepContext, container, execution, retry
from pydantic import BaseModel

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


class Request(BaseModel):
    permanent: bool


class Item(BaseModel):
    permanent: bool
    value: int


environment = container(IMAGE, platform=PLATFORM)
graph = GraphBuilder(
    name="argo-retries",
    input_type=Request,
    output_type=int,
    defaults=execution(environment=environment, retry=retry(3, delay=timedelta(seconds=1))),
)


def flaky(ctx: StepContext[Request]) -> list[Item]:
    if ctx.invocation.attempt == 1:
        raise RuntimeError("first attempt fails")
    return [Item(permanent=ctx.inputs.permanent, value=value) for value in (1, 2)]


def guarded(ctx: StepContext[Item]) -> int:
    if ctx.inputs.permanent:
        raise NonRetryableError("another attempt cannot succeed")
    if ctx.invocation.attempt == 1:
        raise RuntimeError("first attempt fails")
    return ctx.inputs.value * ctx.invocation.attempt


def collect(ctx: StepContext[list[int]]) -> int:
    return sum(ctx.inputs)


items = graph.add(flaky)
results = graph.map(items, guarded, id="guarded", retry=retry(2), concurrency=2)
summary = graph.add(collect)
graph.edge_from(graph.start).to(items)
graph.edge_from(results).to(summary).to(graph.end)
