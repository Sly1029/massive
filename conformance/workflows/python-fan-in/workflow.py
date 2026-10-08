"""Static fan-in: an ordered merge, a discriminated gather into a call, and a call source."""

from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution

DEFAULTS = execution(
    environment=container(
        "example.invalid/python-runner@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)


class Request(BaseModel):
    value: int


class Doubled(BaseModel):
    kind: Literal["doubled"]
    value: int


class Squared(BaseModel):
    kind: Literal["squared"]
    value: int


Measurement = Annotated[Doubled | Squared, Field(discriminator="kind")]


class Report(BaseModel):
    """Records each received value's decoded model, so order and decoding are visible."""

    parts: list[str]


def prepare(context: StepContext[Request]) -> Request:
    return context.inputs


def double(context: StepContext[Request]) -> Doubled:
    return Doubled(kind="doubled", value=context.inputs.value * 2)


def square(context: StepContext[Request]) -> Squared:
    return Squared(kind="squared", value=context.inputs.value**2)


def pair(context: StepContext[tuple[Doubled, Squared]]) -> Report:
    return Report(
        parts=[f"{type(item).__name__}={item.value}" for item in context.inputs]
    )


def summarize(context: StepContext[list[Measurement]]) -> Report:
    return Report(
        parts=[f"{type(item).__name__}={item.value}" for item in context.inputs]
    )


def combine(context: StepContext[tuple[Report, Report]]) -> Report:
    paired, summarized = context.inputs
    return Report(parts=[*paired.parts, "|", *summarized.parts])


summaries = GraphBuilder(
    name="summaries",
    input_type=list[Measurement],
    output_type=Report,
    defaults=DEFAULTS,
)
summaries.edge_from(summaries.start).transform(summarize).to_end(summaries.end)

graph = GraphBuilder(
    name="python-fan-in", input_type=Request, output_type=Report, defaults=DEFAULTS
)
prepared = graph.add(prepare)
doubled = graph.add(double)
squared = graph.add(square)
graph.edge_from(graph.start).to(prepared).to(doubled)
graph.edge_from(prepared).to(squared)
paired = graph.add(pair)
graph.merge(doubled, squared).to(paired)
summarized = graph.call(summaries, id="summaries")
graph.gather(doubled, squared).to(summarized)
graph.merge(paired, summarized).transform(combine).to_end(graph.end)
