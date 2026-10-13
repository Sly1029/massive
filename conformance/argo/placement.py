from __future__ import annotations

from massive import GraphBuilder, StepContext, container, execution

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"

graph = GraphBuilder(
    name="argo-placement",
    input_type=int,
    output_type=int,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM)),
)


def items(ctx: StepContext[int]) -> list[int]:
    return list(range(ctx.inputs))


def square(ctx: StepContext[int]) -> int:
    return ctx.inputs * ctx.inputs


def total(ctx: StepContext[list[int]]) -> int:
    return sum(ctx.inputs)


source = graph.add(items)
squares = graph.map(source, square, id="square", concurrency=2)
graph.edge_from(graph.start).to(source)
graph.edge_from(squares).to(graph.add(total)).to_end(graph.end)
