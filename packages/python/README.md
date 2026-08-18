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
