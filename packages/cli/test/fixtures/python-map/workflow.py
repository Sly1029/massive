from __future__ import annotations

import asyncio

from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    values: list[int]


class Item(BaseModel):
    value: int


class Finding(BaseModel):
    source: int
    doubled: int


graph = GraphBuilder(
    name="python-map",
    input_type=Request,
    output_type=list[Finding],
    defaults=execution(
        environment=container(
            "example.invalid/python-runner@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        )
    ),
)


@graph.step()
def enumerate_items(context: StepContext[None, Request]) -> list[Item]:
    return [Item(value=value) for value in context.inputs.values]


@graph.step()
async def inspect_item(context: StepContext[None, Item]) -> Finding:
    # Finish in a different order than the source collection. Collection must
    # still be deterministic by source index.
    await asyncio.sleep((4 - context.inputs.value) * 0.02)
    if context.inputs.value < 0:
        raise ValueError("negative values cannot be inspected")
    return Finding(source=context.inputs.value, doubled=context.inputs.value * 2)


items = graph.add(enumerate_items)
findings = graph.map(items, inspect_item, id="inspect-items", concurrency=2)
graph.edge_from(graph.start).to(items)
graph.edge_from(findings).to(graph.end)
