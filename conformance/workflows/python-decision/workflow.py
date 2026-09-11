from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    score: int


class Approved(BaseModel):
    kind: Literal["approved"] = "approved"
    score: int


class Rejected(BaseModel):
    kind: Literal["rejected"] = "rejected"
    reason: str


Classification = Annotated[Approved | Rejected, Field(discriminator="kind")]


class Result(BaseModel):
    message: str


graph = GraphBuilder(
    name="python-decision",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python-runner@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        )
    ),
)


@graph.step()
def classify(context: StepContext[None, Request]) -> Classification:
    if context.inputs.score >= 70:
        return Approved(score=context.inputs.score)
    return Rejected(reason="score below threshold")


@graph.step()
def approve(context: StepContext[None, Approved]) -> Result:
    return Result(message=f"approved:{context.inputs.score}")


@graph.step()
def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(message=f"rejected:{context.inputs.reason}")


classified = graph.add(classify)
approved = graph.add(approve)
rejected = graph.add(reject)

route = graph.decision(classified, on="kind", id="route")
approved_input = route.case(Approved)
rejected_input = route.case(Rejected)
selected = route.select(Result, approved=approved, rejected=rejected)

graph.edge_from(graph.start).to(classified)
graph.edge_from(approved_input).to(approved)
graph.edge_from(rejected_input).to(rejected)
graph.edge_from(selected).to(graph.end)
