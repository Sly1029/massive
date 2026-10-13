from __future__ import annotations

from massive import (
    GraphBuilder,
    NonRetryableError,
    RunOutcome,
    StepContext,
    container,
    execution,
    retry,
)

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


def recovered(ctx: StepContext[int]) -> int:
    if ctx.invocation.attempt == 1:
        raise RuntimeError("first attempt fails, then recovers")
    return ctx.inputs


def terminal(ctx: StepContext[int]) -> int:
    raise NonRetryableError("the terminal task ends the run")


def notify(ctx: StepContext[RunOutcome]) -> None:
    assert ctx.inputs.status == "failed"
    assert ctx.inputs.failed_node == "terminal"


graph = GraphBuilder(
    name="argo-exit-retry",
    input_type=int,
    output_type=int,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM), retry=retry(2)),
)
graph.edge_from(graph.start).to(graph.add(recovered)).to(graph.add(terminal)).to_end(graph.end)
graph.on_exit(notify)
