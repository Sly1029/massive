from __future__ import annotations

from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


graph = GraphBuilder(
    name="python-emission",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
            runtime=("python", "3.12.3"),
            packages={"pydantic": "2.10.6"},
            build_args={"UV_COMPILE_BYTECODE": "1"},
        )
    ),
)


@graph.step()
def increment(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


increment_node = graph.add(increment)
graph.edge_from(graph.start).to(increment_node).to(graph.end)
