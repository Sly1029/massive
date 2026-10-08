"""Conformance workflow whose source package exceeds the embedded transport.

Run `generate.py <this directory>` first. Every step reads resource files
from the extracted source package, locally and in remote pods alike.
"""

from __future__ import annotations

import hashlib
from pathlib import Path

from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution

# The conformance driver replaces these declarations before source packaging.
IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"

RESOURCES = Path(__file__).parent / "resources"


class Request(BaseModel):
    pass


class Inventory(BaseModel):
    area: str
    files: int
    bytes: int
    digest: str


class Summary(BaseModel):
    first_prompt: str
    files: int
    bytes: int
    areas: list[Inventory]


def areas(ctx: StepContext[Request]) -> list[str]:
    return sorted(path.name for path in RESOURCES.iterdir())


def inventory(ctx: StepContext[str]) -> Inventory:
    digest = hashlib.sha256()
    files = sorted(path for path in (RESOURCES / ctx.inputs).rglob("*") if path.is_file())
    total = 0
    for path in files:
        body = path.read_bytes()
        total += len(body)
        digest.update(path.relative_to(RESOURCES).as_posix().encode() + b"\0" + body)
    return Inventory(area=ctx.inputs, files=len(files), bytes=total, digest=digest.hexdigest())


def summarize(ctx: StepContext[list[Inventory]]) -> Summary:
    first_prompt = (RESOURCES / "prompts" / "group-00" / "prompt-0000.md").read_text()
    return Summary(
        first_prompt=first_prompt.splitlines()[0],
        files=sum(item.files for item in ctx.inputs),
        bytes=sum(item.bytes for item in ctx.inputs),
        areas=ctx.inputs,
    )


graph = GraphBuilder(
    name="large-source",
    input_type=Request,
    output_type=Summary,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM)),
)
listed = graph.add(areas)
counted = graph.map(listed, inventory, id="inventory", concurrency=3)
summary = graph.add(summarize)
graph.edge_from(graph.start).to(listed)
graph.edge_from(counted).to(summary).to_end(graph.end)
