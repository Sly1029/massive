from __future__ import annotations

import time
from datetime import timedelta

from massive import GraphBuilder, StepContext, container, execution

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"

graph = GraphBuilder(
    name="argo-deadline",
    input_type=int,
    output_type=int,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM)),
    deadline=timedelta(seconds=30),
)


def stall(ctx: StepContext[int]) -> int:
    time.sleep(600)
    return ctx.inputs


graph.edge_from(graph.start).to(graph.add(stall)).to_end(graph.end)
