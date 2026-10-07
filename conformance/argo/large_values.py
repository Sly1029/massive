"""Real-cluster fixture: multi-MB JSON values through every Argo value boundary.

About 3 MB of records cross a decision, a select, a map's expansion, items and
collection, and ordinary step edges. On Argo each of those values travels as a
datastore reference; locally the same values are ordinary JSON artifacts.
"""

import hashlib
import json
from typing import Annotated, Literal

from massive import GraphBuilder, StepContext, container, execution
from pydantic import BaseModel, Field

# The conformance driver replaces these declarations before source packaging.
IMAGE = "example.invalid/runner@sha256:" + "0" * 64
PLATFORM = "linux/amd64"


class Request(BaseModel):
    count: int
    chunks: int = 4


class Record(BaseModel):
    id: int
    path: str
    severity: Literal["low", "high"]
    message: str


class Many(BaseModel):
    kind: Literal["many"] = "many"
    chunks: int
    records: list[Record]


class Few(BaseModel):
    kind: Literal["few"] = "few"
    records: list[Record]


Batch = Annotated[Many | Few, Field(discriminator="kind")]


class Chunk(BaseModel):
    records: list[Record]


class Summary(BaseModel):
    count: int
    high: int
    annotated: int
    digest: str


def generate(ctx: StepContext[Request]) -> Batch:
    records = [
        Record(
            id=index,
            path=f"rules/pack-{index % 17}/rule-{index:05d}.yaml",
            severity="high" if index % 7 == 0 else "low",
            message=hashlib.sha256(str(index).encode()).hexdigest() * 2,
        )
        for index in range(ctx.inputs.count)
    ]
    if len(records) > 1000:
        return Many(chunks=ctx.inputs.chunks, records=records)
    return Few(records=records)


def split(ctx: StepContext[Many]) -> list[Chunk]:
    records = ctx.inputs.records
    size = -(-len(records) // ctx.inputs.chunks)
    return [
        Chunk(records=records[start : start + size])
        for start in range(0, len(records), size)
    ]


def annotate(ctx: StepContext[Chunk]) -> Chunk:
    return Chunk(
        records=[
            record.model_copy(update={"message": record.message + ":annotated"})
            for record in ctx.inputs.records
        ]
    )


def flatten(ctx: StepContext[list[Chunk]]) -> list[Record]:
    return [record for chunk in ctx.inputs for record in chunk.records]


def unchanged(ctx: StepContext[Few]) -> list[Record]:
    return ctx.inputs.records


def summarize(ctx: StepContext[list[Record]]) -> Summary:
    body = json.dumps([record.model_dump() for record in ctx.inputs], sort_keys=True)
    return Summary(
        count=len(ctx.inputs),
        high=sum(record.severity == "high" for record in ctx.inputs),
        annotated=sum(record.message.endswith(":annotated") for record in ctx.inputs),
        digest=hashlib.sha256(body.encode()).hexdigest(),
    )


graph = GraphBuilder(
    name="large-values",
    input_type=Request,
    output_type=Summary,
    defaults=execution(environment=container(IMAGE, platform=PLATFORM)),
)
batch = graph.add(generate)
route = graph.decision(batch, on="kind", id="route")
chunks = graph.add(split)
annotated = graph.map(chunks, annotate, id="annotate", concurrency=2)
joined = graph.add(flatten)
small = graph.add(unchanged)
graph.edge_from(graph.start).to(batch)
graph.edge_from(route.case(Many)).to(chunks)
graph.edge_from(annotated).to(joined)
graph.edge_from(route.case(Few)).to(small)
records = route.select(list[Record], many=joined, few=small)
summary = graph.add(summarize)
graph.edge_from(records).to(summary).to(graph.end)
