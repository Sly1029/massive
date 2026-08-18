from __future__ import annotations

from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


graph: GraphBuilder[None, Request, Result] = GraphBuilder(
    name="typed-authoring",
    input_type=Request,
    output_type=Result,
    defaults=execution(environment=container("example.invalid/typed:latest")),
)


@graph.step()
def increment(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


node = graph.add(increment)
graph.edge_from(graph.start).to(node).to(graph.end)
