"""Real-cluster fixture: nested decisions, unequal branches, maps and failures."""

from typing import Annotated, Literal

from massive import GraphBuilder, StepContext, container, execution
from pydantic import BaseModel, Field

# The conformance driver replaces these declarations before source packaging.
IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


class Request(BaseModel):
    score: int
    copies: int = 2
    fail: bool = False


class Work(BaseModel):
    kind: Literal["work'{{arbitrary}}"] = "work'{{arbitrary}}"
    request: Request


class Skip(BaseModel):
    kind: Literal["skip"] = "skip"


class Positive(BaseModel):
    kind: Literal["positive"] = "positive"
    request: Request


class Zero(BaseModel):
    kind: Literal["zero"] = "zero"


Outer = Annotated[Work | Skip, Field(discriminator="kind")]
Inner = Annotated[Positive | Zero, Field(discriminator="kind")]


def classify(ctx: StepContext[None, Request]) -> Outer:
    return Work(request=ctx.inputs) if ctx.inputs.score >= 0 else Skip()


def classify_inner(ctx: StepContext[None, Work]) -> Inner:
    return (
        Positive(request=ctx.inputs.request) if ctx.inputs.request.score > 0 else Zero()
    )


def prepare(ctx: StepContext[None, Positive]) -> list[Request]:
    return [ctx.inputs.request] * ctx.inputs.request.copies


def evaluate(ctx: StepContext[None, Request]) -> int:
    if ctx.inputs.fail:
        raise ValueError("Deliberate conformance failure")
    return ctx.inputs.score


def collect(ctx: StepContext[None, list[int]]) -> int:
    return sum(ctx.inputs)


def zero(ctx: StepContext[None, Zero]) -> int:
    return 0


def skip(ctx: StepContext[None, Skip]) -> int:
    return -1


graph = GraphBuilder(
    name="argo-decisions",
    input_type=Request,
    output_type=int,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM)),
)
outer_value = graph.add(graph.step()(classify))
outer = graph.decision(outer_value, on="kind", id="outer")
inner_value = graph.add(graph.step()(classify_inner))
inner = graph.decision(inner_value, on="kind", id="inner")
items = graph.add(graph.step()(prepare))
mapped = graph.map(items, graph.step()(evaluate), id="evaluate", concurrency=2)
total = graph.add(graph.step()(collect))
zero_value = graph.add(graph.step()(zero))
skip_value = graph.add(graph.step()(skip))
graph.edge_from(graph.start).to(outer_value)
graph.edge_from(outer.case(Work)).to(inner_value)
graph.edge_from(outer.case(Skip)).to(skip_value)
graph.edge_from(inner.case(Positive)).to(items)
graph.edge_from(mapped).to(total)
graph.edge_from(inner.case(Zero)).to(zero_value)
inner_result = inner.select(int, positive=total, zero=zero_value)
outer_result = outer.select(
    int, **{"work'{{arbitrary}}": inner_result, "skip": skip_value}
)
graph.edge_from(outer_result).to(graph.end)
