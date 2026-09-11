from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    mode: Literal["mapped", "bypass"]
    values: list[int]


class Mapped(BaseModel):
    kind: Literal["mapped"] = "mapped"
    values: list[int]


class Bypass(BaseModel):
    kind: Literal["bypass"] = "bypass"
    values: list[int]


Route = Annotated[Mapped | Bypass, Field(discriminator="kind")]


class Item(BaseModel):
    value: int


class Result(BaseModel):
    value: int


graph = GraphBuilder(
    name="python-map-branches",
    input_type=Request,
    output_type=list[Result],
    defaults=execution(
        environment=container(
            "example.invalid/python-runner@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        )
    ),
)


@graph.step()
def classify(context: StepContext[None, Request]) -> Route:
    if context.inputs.mode == "mapped":
        return Mapped(values=context.inputs.values)
    return Bypass(values=context.inputs.values)


@graph.step()
def mapped_items(context: StepContext[None, Mapped]) -> list[Item]:
    return [Item(value=value) for value in context.inputs.values]


@graph.step()
def double_item(context: StepContext[None, Item]) -> Item:
    return Item(value=context.inputs.value * 2)


@graph.step()
def increment_item(context: StepContext[None, Item]) -> Result:
    return Result(value=context.inputs.value + 1)


@graph.step()
def bypass_items(context: StepContext[None, Bypass]) -> list[Result]:
    return [Result(value=value) for value in context.inputs.values]


classified = graph.add(classify)
mapped_source = graph.add(mapped_items)
bypassed = graph.add(bypass_items)
route = graph.decision(classified, on="kind", id="route")
mapped_input = route.case(Mapped)
bypass_input = route.case(Bypass)
doubled = graph.map(mapped_source, double_item, id="double-items", concurrency=2)
incremented = graph.map(doubled, increment_item, id="increment-items", concurrency=3)
selected = route.select(list[Result], mapped=incremented, bypass=bypassed)

graph.edge_from(graph.start).to(classified)
graph.edge_from(mapped_input).to(mapped_source)
graph.edge_from(bypass_input).to(bypassed)
graph.edge_from(selected).to(graph.end)
