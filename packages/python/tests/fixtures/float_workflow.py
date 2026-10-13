from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution


class Measurement(BaseModel):
    value: float
    scores: dict[str, float]
    padding: str


class Request(BaseModel):
    accepted: bool
    confidence: float = Field(ge=0.0, le=1.0, default=0.5)
    measurements: list[Measurement]


class Accepted(BaseModel):
    kind: Literal["accepted"] = "accepted"
    confidence: float
    measurements: list[Measurement]


class Rejected(BaseModel):
    kind: Literal["rejected"] = "rejected"
    confidence: float
    measurements: list[Measurement]


Route = Annotated[Accepted | Rejected, Field(discriminator="kind")]


def classify(context: StepContext[Request]) -> Route:
    result = Accepted if context.inputs.accepted else Rejected
    return result(confidence=context.inputs.confidence, measurements=context.inputs.measurements)


def accept(context: StepContext[Accepted]) -> list[Measurement]:
    assert isinstance(context.inputs.confidence, float)
    return context.inputs.measurements


def reject(context: StepContext[Rejected]) -> list[Measurement]:
    assert isinstance(context.inputs.confidence, float)
    return context.inputs.measurements


def echo(context: StepContext[Measurement]) -> Measurement:
    assert isinstance(context.inputs.value, float)
    assert all(isinstance(value, float) for value in context.inputs.scores.values())
    return context.inputs


graph = GraphBuilder(
    name="finite-floats",
    input_type=Request,
    output_type=list[Measurement],
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)
classified = graph.add(classify)
route = graph.decision(classified, on="kind", id="route")
accepted_input = route.case(Accepted)
rejected_input = route.case(Rejected)
accepted = graph.add(accept)
rejected = graph.add(reject)
mapped = graph.map(accepted, echo, id="measurements", concurrency=2)
selected = route.select(list[Measurement], accepted=mapped, rejected=rejected)
graph.edge_from(graph.start).to(classified)
graph.edge_from(accepted_input).to(accepted)
graph.edge_from(rejected_input).to(rejected)
graph.edge_from(selected).to_end(graph.end)
