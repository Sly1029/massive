"""Local code-first fan-in, branching, exhausted outcomes, and recovery."""
from __future__ import annotations

import asyncio
from datetime import timedelta

from fan_in import DEFAULTS, Doubled, Report, Request, double, pair, prepare, square
from map_outcomes import Scanned, Task, scan
from massive import MapItemFailed, MapItemOutcome, MapItemSucceeded, StepFailed, Workflow, execution, retry
from pydantic import BaseModel

coordinator_defaults = DEFAULTS
SCAN = execution(environment=DEFAULTS.environment, retry=retry(2, delay=timedelta(0)))


class Result(BaseModel):
    report: Report
    outcomes: list[MapItemOutcome[Scanned]]
    recovery: Doubled | None


async def coordinator(wf: Workflow, inputs: Request) -> Result:
    doubled, squared = await asyncio.gather(
        wf.run(double, inputs, key="double"), wf.run(square, inputs, key="square")
    )
    report = await wf.run(pair, (doubled, squared))

    async def item(task: Task, index: int) -> MapItemOutcome[Scanned]:
        try:
            return MapItemSucceeded(status="succeeded", value=await wf.run(
                scan, task, contract=SCAN, key=f"item-{index}"))
        except StepFailed as error:
            return MapItemFailed(status="failed", failure=error.failure)

    outcomes = await asyncio.gather(
        item(Task(name="ok", behavior="ok"), 0),
        item(Task(name="exhausted", behavior="raise"), 1),
    )
    recovery = None
    if any(outcome.status == "failed" for outcome in outcomes):
        prepared = await wf.run(prepare, inputs, key="recovery-input")
        recovery = await wf.run(double, prepared, key="recovery")
    return Result(report=report, outcomes=outcomes, recovery=recovery)
