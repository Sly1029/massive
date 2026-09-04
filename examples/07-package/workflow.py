from packaged_steps.formatters import format_value
from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    values: list[int]


class Result(BaseModel):
    labels: list[str]


graph = GraphBuilder(
    name="packaged-example",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)


@graph.step()
def unpack(context: StepContext[None, Request]) -> list[int]:
    return context.inputs.values


format_item = graph.step()(format_value)


@graph.step()
def collect(context: StepContext[None, list[str]]) -> Result:
    return Result(labels=context.inputs)


items = graph.add(unpack)
labels = graph.map(items, format_item, concurrency=4, id="labels")
result = graph.add(collect)
graph.edge_from(graph.start).to(items)
graph.edge_from(labels).to(result).to(graph.end)
