from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, JsonValue, NodeHandle, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


class BatchRequest(BaseModel):
    values: list[Request]


class Metadata(BaseModel):
    values: dict[str, JsonValue]


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


map_graph: GraphBuilder[None, BatchRequest, list[Result]] = GraphBuilder(
    name="typed-map",
    input_type=BatchRequest,
    output_type=list[Result],
    defaults=graph.defaults,
)


@map_graph.step()
def unpack(context: StepContext[None, BatchRequest]) -> list[Request]:
    return context.inputs.values


@map_graph.step()
def increment_item(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


requests: NodeHandle[list[Request]] = map_graph.add(unpack)
mapped: NodeHandle[list[Result]] = map_graph.map(requests, increment_item, id="increment-items")
map_graph.edge_from(map_graph.start).to(requests)
map_graph.edge_from(mapped).to(map_graph.end)


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


map_select_graph: GraphBuilder[None, Request, list[Result]] = GraphBuilder(
    name="typed-map-select",
    input_type=Request,
    output_type=list[Result],
    defaults=graph.defaults,
)


@map_select_graph.step()
def classify_for_map(context: StepContext[None, Request]) -> Route:
    return Approved(kind="approved", value=context.inputs.value)


@map_select_graph.step()
def approved_items(context: StepContext[None, Approved]) -> list[Approved]:
    return [context.inputs]


@map_select_graph.step()
def increment_approved(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value + 1)


@map_select_graph.step()
def rejected_items(context: StepContext[None, Rejected]) -> list[Result]:
    return [Result(value=0)]


classified_for_map = map_select_graph.add(classify_for_map)
approved_source = map_select_graph.add(approved_items)
rejected_source = map_select_graph.add(rejected_items)
map_route = map_select_graph.decision(classified_for_map, on="kind", id="map-route")
approved_input = map_route.case(Approved)
rejected_input = map_route.case(Rejected)
approved_mapped: NodeHandle[list[Result]] = map_select_graph.map(
    approved_source, increment_approved, id="increment-approved"
)
selected_map: NodeHandle[list[Result]] = map_route.select(
    list[Result], approved=approved_mapped, rejected=rejected_source
)
map_select_graph.edge_from(map_select_graph.start).to(classified_for_map)
map_select_graph.edge_from(approved_input).to(approved_source)
map_select_graph.edge_from(rejected_input).to(rejected_source)
map_select_graph.edge_from(selected_map).to(map_select_graph.end)
