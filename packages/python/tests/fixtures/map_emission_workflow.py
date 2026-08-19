from __future__ import annotations

from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Detail(BaseModel):
    value: int


class Item(BaseModel):
    detail: Detail


class Result(BaseModel):
    label: str


def load_items(context: StepContext[None, Request]) -> list[Item]:
    return [Item(detail=Detail(value=context.inputs.value))]


def render_item(context: StepContext[None, Item]) -> Result:
    return Result(label=str(context.inputs.detail.value))


graph = GraphBuilder(
    name="python-model-map",
    input_type=Request,
    output_type=list[Result],
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
        )
    ),
)
items = graph.add(graph.step()(load_items))
results = graph.map(items, graph.step()(render_item), id="render-items", concurrency=3)
graph.edge_from(graph.start).to(items)
graph.edge_from(results).to(graph.end)
