"""Real process and filesystem handshakes for coordinator crash/replay tests."""

from __future__ import annotations

import asyncio
import json
import os
import sys
import time
from datetime import timedelta
from pathlib import Path
from typing import Literal

from pydantic import BaseModel

from massive import FatalRunError, StepContext, StepFailed, Workflow, container, execution, retry

coordinator_defaults = execution(
    environment=container("example.invalid/coordinator@sha256:" + "0" * 64),
    retry=retry(3, delay=timedelta(0)),
)
coordinator_deadline_seconds = int(os.environ.get("COORDINATOR_DEADLINE", "0"))
VARIANT = os.environ.get("COORDINATOR_VARIANT", "normal")


class Job(BaseModel):
    name: str
    control: str
    value: int
    behavior: Literal["ok", "fail", "fatal", "hang", "orphan", "flaky", "invalid"] = "ok"


def _activity(context: StepContext[Job], stage: str) -> int:
    job = context.inputs
    root = Path(job.control)
    assert context.invocation.item is None
    record = {
        "pid": os.getpid(),
        "pgid": os.getpgrp(),
        "attempt": context.invocation.attempt,
        "idempotencyKey": context.invocation.idempotency_key,
        "startedAt": time.time(),
    }
    (root / f"started-{stage}-{job.name}-{context.invocation.attempt}.json").write_text(
        json.dumps(record)
    )
    settings = json.loads((root / "settings.json").read_text())
    delay = settings.get("delays", {}).get(f"{stage}-{job.name}", 0)
    time.sleep(delay)
    while (
        f"{stage}-{job.name}" in settings.get("blocked", [])
        and not (root / f"release-{stage}-{job.name}").exists()
    ):
        time.sleep(0.02)
    if stage == "a":
        if job.behavior == "fatal":
            raise FatalRunError("safe coordinator test fatal", "FixtureFatal")
        if job.behavior == "hang":
            time.sleep(60)
        if job.behavior == "fail" or job.behavior == "flaky" and context.invocation.attempt == 1:
            raise ValueError("bounded fixture failure")
        if job.behavior == "orphan" and context.invocation.attempt == 1:
            from massive.artifact import ArtifactRuntime
            from massive.runner import _datastore, _publication_identity

            descriptor = json.loads(Path(sys.argv[1]).read_text())
            destination, producer = _publication_identity(descriptor)
            ArtifactRuntime(_datastore(descriptor["datastore"])).publish_json(
                destination, producer, b"999"
            )
            os._exit(66)
    (root / f"effect-{stage}-{job.name}-{context.invocation.attempt}").write_text(str(job.value))
    return job.value + (1 if stage == "a" else 10)


def stage_a(context: StepContext[Job]) -> int:
    print('author stdout {"kind":"InvokeCommand"}', flush=True)
    value = _activity(context, "a")
    if context.inputs.behavior == "invalid":
        return "wrong output"  # type: ignore[return-value]
    return value


def stage_b(context: StepContext[Job]) -> int:
    return _activity(context, "b")


async def coordinator(wf: Workflow, inputs: list[Job]) -> list[int]:
    async def item(job: Job) -> int:
        try:
            value = await wf.run(stage_a, job, key=f"a-{job.name}")
        except StepFailed:
            value = job.value
        return await wf.run(stage_b, job.model_copy(update={"value": value}), key=f"b-{job.name}")

    ordered = inputs
    if VARIANT == "changed":
        ordered = [job.model_copy(update={"value": job.value + 1}) for job in inputs]
    if VARIANT == "reordered":
        ordered = list(reversed(inputs))
    if VARIANT == "new":
        ordered = [inputs[0].model_copy(update={"name": "new"}), *inputs]
    if VARIANT == "early":
        return []
    if VARIANT == "as_completed":
        tasks = [asyncio.create_task(item(job)) for job in ordered]
        return [await value for value in asyncio.as_completed(tasks)]
    return list(await asyncio.gather(*(item(job) for job in ordered)))
