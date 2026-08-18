# Massive Python SDK

Massive lets a Python author describe a typed, static workflow graph. Pydantic
models are the source of the JSON Schemas carried in the emitted workflow
specification; a step receives one typed input through `StepContext` and
returns one typed output.

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
numbers. `emit()` rejects Pydantic input or output schemas containing JSON
Schema `type: "number"` before a workflow is submitted, including nested
models and collection items. Model a fractional value as a string (or scale it
to an integer) until a future protocol version defines a cross-runtime decimal
representation.
