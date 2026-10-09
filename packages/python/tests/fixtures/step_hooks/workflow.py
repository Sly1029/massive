from __future__ import annotations

from pydantic import BaseModel
from redaction import redacted

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    name: str


class Report(BaseModel):
    text: str


graph = GraphBuilder(
    name="python-step-hooks",
    input_type=Request,
    output_type=Report,
    defaults=execution(
        environment=container(
            "example.invalid/python@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
            platform="linux/amd64",
            command=("python", "-m", "massive"),
            working_directory="app",
        )
    ),
)


@redacted
def report(context: StepContext[Request]) -> Report:
    return Report(text=f"{context.inputs.name} holds the secret")


graph.edge_from(graph.start).to(graph.add(report)).to_end(graph.end)
