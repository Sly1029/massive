from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, NodeHandle, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class Approved(BaseModel):
    kind: Literal["approved"]
    value: int


class Rejected(BaseModel):
    kind: Literal["rejected"]
    reason: str


Route = Annotated[Approved | Rejected, Field(discriminator="kind")]


graph: GraphBuilder[None, Request, Result] = GraphBuilder(
    name="typed-authoring",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/typed@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
        )
    ),
)


@graph.step()
def increment(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


@graph.step()
async def increment_async(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


sync_node: NodeHandle[Result] = graph.add(increment)
async_node: NodeHandle[Result] = graph.add(increment_async)
graph.edge_from(graph.start).to(sync_node).to(async_node).to(graph.end)


decision_graph: GraphBuilder[None, Request, Result] = GraphBuilder(
    name="typed-decisions",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/typed-decisions@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        )
    ),
)


@decision_graph.step()
def classify(context: StepContext[None, Request]) -> Route:
    return Approved(kind="approved", value=context.inputs.value)


@decision_graph.step()
def approve(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value)


@decision_graph.step()
def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(value=0)


classified = decision_graph.add(classify)
approved_result = decision_graph.add(approve)
rejected_result = decision_graph.add(reject)
route = decision_graph.decision(classified, on="kind", id="route")
approved_input: NodeHandle[Approved] = route.case(Approved)
rejected_input: NodeHandle[Rejected] = route.case(Rejected)
decision_graph.edge_from(decision_graph.start).to(classified)
decision_graph.edge_from(approved_input).to(approved_result)
decision_graph.edge_from(rejected_input).to(rejected_result)
selected: NodeHandle[Result] = route.select(
    Result, approved=approved_result, rejected=rejected_result
)
decision_graph.edge_from(selected).to(decision_graph.end)
