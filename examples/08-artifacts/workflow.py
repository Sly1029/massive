from __future__ import annotations

from pathlib import Path

from pydantic import BaseModel

from massive import Blob, GraphBuilder, StepContext, Tree, container, execution


class Request(BaseModel):
    copies: int


class Snapshot(BaseModel):
    checkout: Tree
    index: int


class Inspection(BaseModel):
    original: Tree
    report: Blob


class Summary(BaseModel):
    reports: list[str]
    original: str


graph = GraphBuilder(
    name="file-artifacts",
    input_type=Request,
    output_type=Summary,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
        )
    ),
)


def capture(ctx: StepContext[None, Request]) -> list[Snapshot]:
    root = Path(__file__).parent / "checkout"
    root.mkdir()
    (root / "source.txt").write_text("original")
    (root / "empty").mkdir()
    (root / "tool").write_text("#!/bin/sh\necho ready\n")
    (root / "tool").chmod(0o755)
    tree = Tree.from_path(root)
    return [Snapshot(checkout=tree, index=i) for i in range(ctx.inputs.copies)]


def inspect(ctx: StepContext[None, Snapshot]) -> Inspection:
    root = ctx.inputs.checkout.path()
    assert (root / "empty").is_dir()
    assert (root / "tool").stat().st_mode & 0o111
    assert (root / "source.txt").read_text() == "original"
    (root / "source.txt").write_text(f"copy-{ctx.inputs.index}")
    report = root / "report.txt"
    report.write_text(f"report-{ctx.inputs.index}")
    return Inspection(original=ctx.inputs.checkout, report=Blob.from_path(report))


def summarize(ctx: StepContext[None, list[Inspection]]) -> Summary:
    return Summary(
        reports=[item.report.path().read_text() for item in ctx.inputs],
        original=(ctx.inputs[0].original.path() / "source.txt").read_text()
        if ctx.inputs
        else "empty",
    )


# Registration works without decorators, so reusable ordinary functions compose cleanly.
source = graph.add(graph.step()(capture))
items = graph.map(source, graph.step()(inspect), id="inspect-files", concurrency=2)
result = graph.add(graph.step()(summarize))
graph.edge_from(graph.start).to(source)
graph.edge_from(items).to(result).to(graph.end)
