from __future__ import annotations

from massive import GraphBuilder, RunOutcome, StepContext, container, execution

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"

graph = GraphBuilder(
    name="argo-exit-hook",
    input_type=int,
    output_type=int,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM)),
)


def double(ctx: StepContext[int]) -> int:
    return ctx.inputs * 2


def notify(ctx: StepContext[RunOutcome]) -> None:
    print("MASSIVE-OUTCOME " + ctx.inputs.model_dump_json(), flush=True)
    raise RuntimeError("notification service unavailable")


graph.edge_from(graph.start).to(graph.add(double)).to_end(graph.end)
graph.on_exit(notify)
