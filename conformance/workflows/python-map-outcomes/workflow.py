"""A map that collects item failures: real exceptions, signals, and timeouts.

Each item fails in its own way after its retries are exhausted. The map still
succeeds with one outcome per item, and a downstream step reports them beside
the requests it merges back in.
"""

from __future__ import annotations

import ctypes
import os
import signal
import time
from datetime import timedelta
from typing import Literal

from massive import (
    GraphBuilder,
    MapItemFailed,
    MapItemOutcome,
    MapItemSucceeded,
    NonRetryableError,
    StepContext,
    container,
    execution,
    retry,
)
from pydantic import BaseModel

DEFAULTS = execution(
    environment=container(
        "example.invalid/python-runner@sha256:"
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    )
)

Behavior = Literal["ok", "flaky", "raise", "refuse", "segfault", "sigkill", "hang"]


class Batch(BaseModel):
    tasks: list[Task]


class Task(BaseModel):
    name: str
    behavior: Behavior


class Scanned(BaseModel):
    name: str
    attempt: int


class Failure(BaseModel):
    name: str
    kind: str
    attempts: int
    diagnostic: str


class Report(BaseModel):
    scanned: list[Scanned]
    failed: list[Failure]


graph = GraphBuilder(
    name="python-map-outcomes", input_type=Batch, output_type=Report, defaults=DEFAULTS
)


def plan(context: StepContext[Batch]) -> list[Task]:
    return context.inputs.tasks


def scan(context: StepContext[Task]) -> Scanned:
    task, attempt = context.inputs, context.invocation.attempt
    if task.behavior == "flaky" and attempt == 1:
        raise RuntimeError(f"{task.name} is flaky")
    if task.behavior == "raise":
        raise ValueError(f"{task.name} cannot be parsed")
    if task.behavior == "refuse":
        raise NonRetryableError(f"{task.name} is not supported")
    if task.behavior == "segfault":
        ctypes.string_at(0)  # a native crash, as in a faulting extension or tool
    if task.behavior == "sigkill":
        os.kill(os.getpid(), signal.SIGKILL)  # the signal an out-of-memory kill sends
    if task.behavior == "hang":
        time.sleep(60)
    return Scanned(name=task.name, attempt=attempt)


def report(
    context: StepContext[tuple[list[Task], list[MapItemOutcome[Scanned]]]],
) -> Report:
    tasks, outcomes = context.inputs
    scanned: list[Scanned] = []
    failed: list[Failure] = []
    for task, outcome in zip(tasks, outcomes, strict=True):
        match outcome:
            case MapItemSucceeded(value=value):
                scanned.append(value)
            case MapItemFailed(failure=failure):
                failed.append(
                    Failure(
                        name=task.name,
                        kind=failure.kind,
                        attempts=failure.attempts,
                        diagnostic=failure.diagnostic,
                    )
                )
    return Report(scanned=scanned, failed=failed)


tasks = graph.add(plan)
outcomes = graph.map(
    tasks,
    scan,
    id="scan",
    concurrency=8,
    item_failures="collect",
    retry=retry(2, delay=timedelta(0)),
    timeout=timedelta(seconds=2),
)
summary = graph.add(report)
graph.edge_from(graph.start).to(tasks)
graph.merge(tasks, outcomes).to(summary).to_end(graph.end)
