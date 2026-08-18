from __future__ import annotations

from pydantic import BaseModel

from massive import GraphBuilder, NodeHandle, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


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
