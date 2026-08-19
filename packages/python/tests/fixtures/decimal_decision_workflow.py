from __future__ import annotations

from decimal import Decimal
from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    amount: Decimal


class Accepted(BaseModel):
    kind: Literal["accepted"] = "accepted"
    amount: Decimal


class Rejected(BaseModel):
    kind: Literal["rejected"] = "rejected"
    reason: str


Route = Annotated[Accepted | Rejected, Field(discriminator="kind")]


class Result(BaseModel):
    message: str


graph = GraphBuilder(
    name="decimal-decision",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)


@graph.step()
def classify(context: StepContext[None, Request]) -> Route:
    if context.inputs.amount >= 0:
        return Accepted(amount=context.inputs.amount)
    return Rejected(reason="negative amount")


@graph.step()
def accept(context: StepContext[None, Accepted]) -> Result:
    return Result(message=str(context.inputs.amount))


@graph.step()
def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(message=context.inputs.reason)


classified = graph.add(classify)
accepted = graph.add(accept)
rejected = graph.add(reject)
route = graph.decision(classified, on="kind", id="route")
accepted_input = route.case(Accepted)
rejected_input = route.case(Rejected)
selected = route.select(Result, accepted=accepted, rejected=rejected)
graph.edge_from(graph.start).to(classified)
graph.edge_from(accepted_input).to(accepted)
graph.edge_from(rejected_input).to(rejected)
graph.edge_from(selected).to(graph.end)
