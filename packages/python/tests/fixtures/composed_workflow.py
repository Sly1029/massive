from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Approved(BaseModel):
    kind: Literal["approved"]
    value: int


class Rejected(BaseModel):
    kind: Literal["rejected"]
    value: int


Route = Annotated[Approved | Rejected, Field(discriminator="kind")]


def classify(context: StepContext[Request]) -> Route:
    if context.inputs.value >= 0:
        return Approved(kind="approved", value=context.inputs.value)
    return Rejected(kind="rejected", value=context.inputs.value)


def increment(context: StepContext[Approved]) -> Approved:
    return Approved(kind="approved", value=context.inputs.value + 1)


def accept(context: StepContext[Approved]) -> Request:
    return Request(value=context.inputs.value)


def reject(context: StepContext[Rejected]) -> Request:
    return Request(value=0)


defaults = execution(
    environment=container(
        "example.invalid/python@sha256:" + "0" * 64,
        platform="linux/amd64",
    )
)

increment_graph = GraphBuilder(
    name="increment-step", input_type=Approved, output_type=Approved, defaults=defaults
)
increment_graph.edge_from(increment_graph.start).to(increment_graph.add(increment)).to(
    increment_graph.end
)
child = GraphBuilder(
    name="increment-child", input_type=Approved, output_type=Approved, defaults=defaults
)
child.edge_from(child.start).to(child.call(increment_graph, id="nested")).to(child.end)

graph = GraphBuilder(name="composed", input_type=Request, output_type=Request, defaults=defaults)
classified = graph.add(classify)
graph.edge_from(graph.start).to(classified)
decision = graph.decision(classified, on="kind", id="route")
first = graph.call(child, id="first")
second = graph.call(child, id="second")
graph.edge_from(decision.case(Approved)).to(first).to(second)
accepted = graph.add(accept)
graph.edge_from(second).to(accepted)
rejected = graph.add(reject)
graph.edge_from(decision.case(Rejected)).to(rejected)
selected = decision.select(Request, approved=accepted, rejected=rejected)
graph.edge_from(selected).to(graph.end)
