"""A fatal author signal stops a collecting loop and preserves its exit hook."""

from __future__ import annotations

import time

from botocore.exceptions import ClientError
from botocore.session import get_session
from massive import (
    FatalRunError,
    GraphBuilder,
    MapItemOutcome,
    RunOutcome,
    StepContext,
    container,
    execution,
    retry,
)

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"
defaults = execution(environment=container(IMAGE, platform=PLATFORM), retry=retry(3))


def guarded(ctx: StepContext[int]) -> int:
    store = get_session().create_client(
        "s3", endpoint_url="http://minio:9000", region_name="us-east-1"
    )
    key = f"fatal-conformance/{ctx.invocation.run_id}/ready"
    if ctx.inputs == 0:
        deadline = time.monotonic() + 60
        while True:
            try:
                body = store.get_object(Bucket="my-bucket", Key=key)["Body"]
                body.close()
                break
            except ClientError as error:
                if error.response["ResponseMetadata"]["HTTPStatusCode"] != 404:
                    raise
                if time.monotonic() > deadline:
                    raise RuntimeError(
                        "sibling did not reach the functional gate"
                    ) from None
                time.sleep(0.1)
        raise FatalRunError("Input is invalid", "input.invalid") from RuntimeError(
            "SECRET exception chain"
        )
    print(f"sibling {ctx.inputs} started", flush=True)
    store.put_object(Bucket="my-bucket", Key=key, Body=b"ready")
    time.sleep(120)
    print(f"sibling {ctx.inputs} completed", flush=True)
    return ctx.inputs


def refuse(ctx: StepContext[int]) -> int:
    raise FatalRunError(
        "Upstream source unavailable", "source.unavailable"
    ) from RuntimeError("SECRET exception chain")


def after(ctx: StepContext[list[MapItemOutcome[int]]]) -> int:
    raise RuntimeError("a fatal map must never reach this step")


def notify(ctx: StepContext[RunOutcome]) -> None:
    assert ctx.inputs.fatal is not None
    assert ctx.inputs.fatal.type in {"input.invalid", "source.unavailable"}
    assert ctx.inputs.fatal.message in {
        "Input is invalid",
        "Upstream source unavailable",
    }


graph = GraphBuilder(
    name="argo-fatal-map", input_type=list[int], output_type=int, defaults=defaults
)
outcomes = graph.map(
    graph.start, guarded, id="guarded", item_failures="collect", concurrency=2
)
graph.edge_from(outcomes).to(graph.add(after)).to_end(graph.end)
graph.on_exit(notify)

plain = GraphBuilder(
    name="argo-fatal-step", input_type=int, output_type=int, defaults=defaults
)
plain.edge_from(plain.start).to(plain.add(refuse)).to_end(plain.end)
plain.on_exit(notify)
