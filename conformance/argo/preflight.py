from __future__ import annotations

from massive import GraphBuilder, StepContext, container, execution, retry
from pydantic import BaseModel

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"
NAME = "argo-preflight"


class Request(BaseModel):
    value: int


graph = GraphBuilder(
    name=NAME,
    input_type=Request,
    output_type=int,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM), retry=retry(3)),
)


def double(ctx: StepContext[Request]) -> int:
    return ctx.inputs.value * 2


graph.edge_from(graph.start).to(graph.add(double)).to_end(graph.end)
