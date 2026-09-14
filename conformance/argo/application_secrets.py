from __future__ import annotations

import hashlib
import os

from massive import GraphBuilder, StepContext, container, execution
from pydantic import BaseModel

IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


class Request(BaseModel):
    digest: str
    values: list[int]


class Item(BaseModel):
    digest: str
    value: int


environment = container(IMAGE, platform=PLATFORM)
graph = GraphBuilder(
    name="argo-secrets",
    input_type=Request,
    output_type=int,
    defaults=execution(environment=environment),
)


def prepare(ctx: StepContext[Request]) -> list[Item]:
    assert "APP_TOKEN" not in os.environ
    return [Item(digest=ctx.inputs.digest, value=value) for value in ctx.inputs.values]


def authenticate(ctx: StepContext[Item]) -> int:
    actual = hashlib.sha256(os.environ["APP_TOKEN"].encode()).hexdigest()
    if actual != ctx.inputs.digest:
        raise ValueError("Application credential did not match the deployed Secret")
    return ctx.inputs.value * 2


def collect(ctx: StepContext[list[int]]) -> int:
    assert "APP_TOKEN" not in os.environ
    return sum(ctx.inputs)


items = graph.add(prepare)
results = graph.map(
    items,
    authenticate,
    contract=execution(environment=environment, secrets={"APP_TOKEN": "service-token"}),
    id="authenticated",
    concurrency=2,
)
summary = graph.add(collect)
graph.edge_from(graph.start).to(items)
graph.edge_from(results).to(summary).to(graph.end)
