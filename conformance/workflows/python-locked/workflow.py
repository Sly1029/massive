from __future__ import annotations

from pydantic import BaseModel
from tabulate import tabulate

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int
    table: str


graph = GraphBuilder(
    name="python-locked",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python-runner@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        )
    ),
)


def double(context: StepContext[Request]) -> Result:
    value = context.inputs.value * 2
    return Result(value=value, table=tabulate([["value", value]], tablefmt="plain"))


step = graph.add(double)
graph.edge_from(graph.start).to(step).to(graph.end)
