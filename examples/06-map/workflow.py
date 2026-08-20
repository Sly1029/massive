from __future__ import annotations

from massive import GraphBuilder, StepContext, container, execution
from pydantic import BaseModel


class Batch(BaseModel):
    values: list[int]


class Item(BaseModel):
    value: int


class Result(BaseModel):
    source: int
    squared: int


graph: GraphBuilder[None, Batch, list[Result]] = GraphBuilder(
    name="map-example",
    input_type=Batch,
    output_type=list[Result],
    defaults=execution(
        environment=container(
            "example.invalid/python-runner@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)


@graph.step()
def unpack(context: StepContext[None, Batch]) -> list[Item]:
    return [Item(value=value) for value in context.inputs.values]


@graph.step()
def square(context: StepContext[None, Item]) -> Result:
    return Result(source=context.inputs.value, squared=context.inputs.value**2)


items = graph.add(unpack)
results = graph.map(items, square, id="square-items", concurrency=4)

graph.edge_from(graph.start).to(items)
graph.edge_from(results).to(graph.end)
