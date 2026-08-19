# Massive Python SDK

Massive lets a Python author describe a typed, static workflow graph. Pydantic
models are the source of the JSON Schemas carried in the emitted workflow
specification; a step receives one typed input through `StepContext` and
returns one typed output. Python is the primary Massive authoring surface;
TypeScript uses the same portable artifact protocol.

```python
from pydantic import BaseModel

from massive import GraphBuilder, StepContext, container, execution


class Request(BaseModel):
    value: int


class Result(BaseModel):
    value: int


graph = GraphBuilder(
    name="double",
    input_type=Request,
    output_type=Result,
    defaults=execution(
        environment=container(
            "registry.example/massive-python@sha256:"
            "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
        )
    ),
)


@graph.step()
async def double(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value * 2)


node = graph.add(double)
graph.edge_from(graph.start).to(node).to(graph.end)
```

`GraphBuilder.step` accepts synchronous and asynchronous top-level functions.
The type of `node` is `NodeHandle[Result]` in either form, so the graph's edge
types still follow the value a step produces rather than its coroutine.

## Exhaustive decisions

Use a Pydantic discriminated union when a step chooses one of a finite set of
routes. Each alternative is a `BaseModel` with exactly one string `Literal`
tag, and the union is annotated with `Field(discriminator=...)`:

```python
from typing import Annotated, Literal

from pydantic import BaseModel, Field


class Approved(BaseModel):
    kind: Literal["approved"] = "approved"
    value: int


class Rejected(BaseModel):
    kind: Literal["rejected"] = "rejected"
    reason: str


Review = Annotated[Approved | Rejected, Field(discriminator="kind")]

decision_graph: GraphBuilder[None, Request, Result] = GraphBuilder(
    name="review",
    input_type=Request,
    output_type=Result,
    defaults=graph.defaults,
)


@decision_graph.step()
def classify(context: StepContext[None, Request]) -> Review:
    if context.inputs.value >= 0:
        return Approved(value=context.inputs.value)
    return Rejected(reason="negative value")


@decision_graph.step()
async def approve(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value)


@decision_graph.step()
def reject(context: StepContext[None, Rejected]) -> Result:
    return Result(value=0)


classified = decision_graph.add(classify)
approved = decision_graph.add(approve)
rejected = decision_graph.add(reject)

route = decision_graph.decision(classified, on="kind", id="review-route")
approved_input = route.case(Approved)  # CaseHandle[Approved]
rejected_input = route.case(Rejected)  # CaseHandle[Rejected]

decision_graph.edge_from(decision_graph.start).to(classified)
decision_graph.edge_from(approved_input).to(approved)
decision_graph.edge_from(rejected_input).to(rejected)

selected = route.select(Result, approved=approved, rejected=rejected)
decision_graph.edge_from(selected).to(decision_graph.end)
```

`case()` narrows the routed value for Pyright and for the receiving step's
Pydantic input validation. Every union case must be claimed exactly once, and
`select()` keyword names must exactly equal the discriminator tags. Every
selected branch must return the declared output type. A model with multiple
tags such as `Literal["approved", "manual-review"]` is rejected; define one
model per tag so `case(Model)` remains unambiguous.

Only the selected branch runs. Inactive branch steps are recorded as skipped,
not invoked. `select()` then exposes the selected branch result as an ordinary
`NodeHandle[Result]`; at runtime it aliases the already crystallized artifact
without launching another step, copying its body, or uploading it again.
Declaration order does not affect the emitted graph: authors may create the
select before or after adding the case edges, and `emit()` validates the final
topology. Decisions may be nested inside a case; if the outer case is inactive,
the nested classifier, decision, branches, and select are all skipped as one
control region. Sync and async classification or branch steps may be mixed
freely.

Exhaustive decisions execute through `massive run`'s local compiled path today.
The Argo target deliberately rejects decision/select plans with an explicit
unsupported-graph-semantic diagnostic; it does not emit a template with altered
branch semantics. Argo lowering will be enabled only when it can preserve these
same exhaustive, skip, and select guarantees.

## Artifact handling

Authors do not read or write object-store keys in normal step code. The runner
validates the Pydantic-derived input and output schemas, canonicalizes the JSON
output, writes its content-addressed body, then commits the deterministic step
output manifest. A retry with the identical value converges on the same
objects; a different value for an already committed attempt fails rather than
overwriting it. Downstream runtime code resolves and verifies the manifest,
body digest, size, content type, producer slot, and schema before decoding the
value.

The important runtime categories are:

- `ArtifactValidationError`: invalid slot identity, non-canonical JSON, or a
  value/schema problem before publication.
- `ArtifactNotFoundError`: the requested output manifest has not been
  committed.
- `ArtifactIntegrityError`: a committed manifest references missing, tampered,
  or incompatible data.
- `ArtifactBodyConflictError` and `ArtifactManifestConflictError`: an
  immutable object already exists with different bytes or metadata.

At the command boundary, descriptor errors exit 64, schema/artifact failures
exit 65, and a user-step exception exits 66. Datastore outages are deliberately
not rewritten as artifact conflicts so retry policy can recognize them as
transient infrastructure failures.

The runtime derives the artifact namespace from a normalized project identity
of the form `sha256-<64 lowercase hex characters>`; callers do not put a human
repository name into an object-store path.

## Current scope

The graph is static: define all nodes and edges during authoring rather than
creating graph structure from step results. Step inputs and outputs must be
canonical JSON values representable by their Pydantic schemas. Dependency
providers (`deps_type`) are intentionally not part of the invocation protocol
yet, so use explicit JSON inputs until that surface is designed. Python is the
primary SDK; the artifact manifest protocol is shared with the other Massive
runtimes.

`canonical-json-v0` deliberately supports integers, but not floating-point
numbers. Massive emits Pydantic's **validation schema** for workflow and step
inputs, and its **serialization schema** for workflow and step outputs. This
matches the runtime boundary: inputs are parsed into Python values with
`TypeAdapter.validate_python`, while outputs are validated and then converted
with `dump_python(mode="json")` before publication.

`emit()` still checks the serialization shape for both directions before it
accepts an input validation schema. That prevents a bare `float` input from
appearing portable merely because Pydantic can validate it: its serialized
shape is JSON `number`, which v0 cannot carry. Decimal passes this check because
its serialized shape is a string.

That distinction makes `Decimal` useful without inventing a second wire
format. Pydantic accepts a Decimal input in its validation schema and serializes
the output as a JSON string, so an output such as `Decimal("10.5")` is stored as
`{"value":"10.5"}`. A following Decimal-typed step can validate that string
again. The published validation schema deliberately retains Pydantic's
number-or-string input shape; canonical-json-v0 framing is an additional,
authoritative constraint that rejects a fractional JSON number before schema
validation. Actual float/`number` output schemas are rejected at `emit()` because
their serialized values cannot be represented by v0; use `Decimal`, a string,
or a scaled integer instead.

`Any` and unconstrained mappings are also rejected at `emit()` in v0. They can
admit floats that no runtime can encode consistently. Prefer a concrete model,
`list[int]`, `dict[str, int]`, or another explicit JSON shape.
Models configured with Pydantic `extra="allow"` are rejected for the same
reason; prefer the default behavior or `extra="forbid"` for workflow payloads.

These author-time checks intentionally cover the Pydantic-generated schema
forms Massive emits; they do not attempt to solve arbitrary JSON Schema
satisfiability. Runtime canonicalization remains the authoritative final
boundary before an artifact is published.

## Frontend emitter

Run a zero-config Python workflow through the same compiler, orchestrator, and
artifact protocol used by every Massive frontend:

```sh
massive run path/to/workflow.py --input '{"value": 21}'
```

When a module exports more than one graph, select it explicitly with
`workflow.py#graph_export`. The CLI invokes the Python frontend as an isolated
process, validates its canonical `WorkflowSpec`, compiles it with the Go
compiler, and invokes each Python step in a separate runner process. Local
execution does not call the in-memory `GraphBuilder` directly.

The Python frontend emits a portable `WorkflowSpec` from a Python workflow
file:

```sh
massive-python-frontend emit path/to/workflow.py[#graph_export]
```

The command writes only canonical `WorkflowSpec` JSON to stdout. Diagnostics
are written to stderr and return exit status 2. The `massive run` CLI uses this
process seam internally; workflow authors normally invoke `massive run`
instead.

This first frontend slice is zero-config. Its source package root is the entry
file's parent directory, uses package ID `python-main`, and includes every
root-level `*.py` file. Nested packages and recursive source layouts are not
included yet, so keep the workflow and direct helper modules together until an
explicit Python package configuration surface is added.
