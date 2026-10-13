"""Real-cluster fixture: a map that collects item failures, including an OOM kill."""

from __future__ import annotations

import ctypes
import time
from datetime import timedelta
from typing import Literal

from massive import (
    GraphBuilder,
    MapItemOutcome,
    NonRetryableError,
    StepContext,
    container,
    execution,
    retry,
)
from pydantic import BaseModel

# The conformance driver replaces these declarations before source packaging.
IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


class Task(BaseModel):
    name: str
    behavior: Literal["ok", "flaky", "raise", "refuse", "segfault", "hang", "oom"]


class Scanned(BaseModel):
    name: str
    attempt: int


graph = GraphBuilder(
    name="argo-map-outcomes",
    input_type=list[Task],
    output_type=list[MapItemOutcome[Scanned]],
    # The limit lets the kernel kill the "oom" item; the others stay far below it.
    defaults=execution(environment=container(IMAGE, platform=PLATFORM), memory="256Mi"),
)


def scan(context: StepContext[Task]) -> Scanned:
    task, attempt = context.inputs, context.invocation.attempt
    if task.behavior == "flaky" and attempt == 1:
        raise RuntimeError(f"{task.name} is flaky")
    if task.behavior == "raise":
        raise ValueError(f"{task.name} cannot be parsed")
    if task.behavior == "refuse":
        raise NonRetryableError(f"{task.name} is not supported")
    if task.behavior == "segfault":
        ctypes.string_at(0)
    if task.behavior == "hang":
        time.sleep(60)
    if task.behavior == "oom":
        blocks = []
        while True:
            blocks.append(b"x" * (32 << 20))
    return Scanned(name=task.name, attempt=attempt)


outcomes = graph.map(
    graph.start,
    scan,
    id="scan",
    concurrency=4,
    item_failures="collect",
    retry=retry(2, delay=timedelta(0)),
    timeout=timedelta(seconds=5),
)
graph.edge_from(outcomes).to_end(graph.end)
