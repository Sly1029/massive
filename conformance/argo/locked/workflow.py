from __future__ import annotations

from massive import GraphBuilder, StepContext, container, execution, retry
from pydantic import BaseModel

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


class Request(BaseModel):
    value: int


graph = GraphBuilder(
    name="argo-locked",
    input_type=Request,
    output_type=str,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM), retry=retry(2)),
)


def render(ctx: StepContext[Request]) -> str:
    # Only the image provides tabulate; the build host emits without it.
    from tabulate import tabulate

    return tabulate([["value", ctx.inputs.value * 2]], tablefmt="plain")


graph.edge_from(graph.start).to(graph.add(render)).to(graph.end)
