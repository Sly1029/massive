from __future__ import annotations

from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution

from helper import increment


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


graph = GraphBuilder(
    name="python-linear",
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
def add_one(context: StepContext[None, Request]) -> Result:
    return Result(value=increment(context.inputs.value))


step = graph.add(add_one)
graph.edge_from(graph.start).to(step).to(graph.end)
