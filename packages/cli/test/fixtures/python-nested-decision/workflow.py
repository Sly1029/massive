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


OuterRoute = Annotated[Approved | Rejected, Field(discriminator="kind")]


class FastTrack(BaseModel):
    kind: Literal["fast"] = "fast"
    score: int


class ManualReview(BaseModel):
    kind: Literal["manual"] = "manual"
    score: int


InnerRoute = Annotated[FastTrack | ManualReview, Field(discriminator="kind")]


class Result(BaseModel):
    message: str


graph = GraphBuilder(
    name="python-nested-decision",
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
def classify_outer(context: StepContext[None, Request]) -> OuterRoute:
    if context.inputs.score >= 70:
        return Approved(score=context.inputs.score)
    return Rejected(reason="score below threshold")


@graph.step()
def review(context: StepContext[None, Approved]) -> InnerRoute:
    if context.inputs.score >= 90:
        return FastTrack(score=context.inputs.score)
    return ManualReview(score=context.inputs.score)


@graph.step()
def fast(context: StepContext[None, FastTrack]) -> Result:
    return Result(message=f"fast:{context.inputs.score}")


@graph.step()
def manual(context: StepContext[None, ManualReview]) -> Result:
    return Result(message=f"manual:{context.inputs.score}")


@graph.step()
def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(message=f"rejected:{context.inputs.reason}")


classified_outer = graph.add(classify_outer)
reviewed = graph.add(review)
fast_result = graph.add(fast)
manual_result = graph.add(manual)
rejected_result = graph.add(reject)

outer = graph.decision(classified_outer, on="kind", id="outer-route")
outer_approved = outer.case(Approved)
outer_rejected = outer.case(Rejected)

graph.edge_from(graph.start).to(classified_outer)
graph.edge_from(outer_approved).to(reviewed)
graph.edge_from(outer_rejected).to(rejected_result)

inner = graph.decision(reviewed, on="kind", id="inner-route")
inner_fast = inner.case(FastTrack)
inner_manual = inner.case(ManualReview)
graph.edge_from(inner_fast).to(fast_result)
graph.edge_from(inner_manual).to(manual_result)
inner_selected = inner.select(Result, fast=fast_result, manual=manual_result)

selected = outer.select(Result, approved=inner_selected, rejected=rejected_result)
graph.edge_from(selected).to(graph.end)
