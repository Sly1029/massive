# Massive Python SDK

Massive lets a Python author describe a typed, static workflow graph. Pydantic
models are the source of the JSON Schemas carried in the emitted workflow
specification; a step receives one typed input through `StepContext` and
returns one typed output. Python is the primary Massive authoring surface;
TypeScript uses the same portable artifact protocol.

## Install and run

The PyPI distribution is named `massive-workflows`; it installs the `massive`
Python module and command. The platform wheel contains the matching Go control
plane, while the command passes the active Python interpreter to the frontend
and step runner automatically:

```sh
uv add massive-workflows
uv run massive run workflow.py --input '{"value": 21}'
```

Build a static, container-target workflow for Argo with the same compiled plan:

```sh
uv run massive build workflow.py \
  --target argo \
  --output .massive/argo \
  --namespace workflows \
  --service-account massive-runner
```

The bundle contains the canonical workflow and deployment specifications, the
compiled plan, a bundle manifest, a runtime `ConfigMap`, and an
offline-schema-validated Argo `WorkflowTemplate` in both YAML and canonical
JSON. Verified source packages are
also exposed under `runtime-assets/` as deterministic, content-addressed tar
files. Their hashes and media types are recorded in `bundle-manifest.json`, so
the same pack can be inspected or uploaded without decoding the ConfigMap.
Apply and submit it with the workflow input as one JSON parameter:

```sh
kubectl apply -f .massive/argo/runtime-configmap.json
test ! -f .massive/argo/runtime-network-policy.json || \
  kubectl apply -f .massive/argo/runtime-network-policy.json
kubectl apply -f .massive/argo/workflow-template.yaml

argo submit -n workflows --from workflowtemplate/double \
  -p 'input={"value":21}' --watch
```

Every container image selected with `container(...)` must contain Python 3.12+
and the same `massive-workflows` release, so its default `massive` command can
run the isolated step adapter and Python runner. Source files are verified at
build time and mounted from the generated `ConfigMap`; they do not need to be
baked into the runner image. If a container recipe supplies `command=`, that
command must launch the Massive CLI and accept the generated runtime arguments.

Local execution supports static steps, exhaustive decisions, and finite maps.
Argo 0.1 lowering is intentionally limited to static graphs, immutable
container environments, small JSON parameter values, and at most 700 KiB of
embedded plan plus source archives. `network="none"` emits a matching
`NetworkPolicy`; secret declarations and other egress policies fail during the
build until their target lowering exists. Unsupported semantics fail rather
than producing a behaviorally different workflow. The deployment profile keeps
an artifact-store binding identity so larger source/value transport can move to
S3-compatible storage later without changing authoring or plan identity.

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

## Finite maps

Map a concrete list produced by an earlier node with one decorated step. The
returned handle is the ordered `list[Result]`, including the empty-list case;
there is no separate gather or collect call.

```python
class Batch(BaseModel):
    values: list[Request]


map_graph = GraphBuilder(
    name="increment-batch",
    input_type=Batch,
    output_type=list[Result],
    defaults=graph.defaults,
)


@map_graph.step()
def unpack(context: StepContext[None, Batch]) -> list[Request]:
    return context.inputs.values


@map_graph.step()
def increment_item(context: StepContext[None, Request]) -> Result:
    return Result(value=context.inputs.value + 1)


requests = map_graph.add(unpack)
results = map_graph.map(requests, increment_item, id="increment-items", concurrency=20)
map_graph.edge_from(map_graph.start).to(requests)
map_graph.edge_from(results).to(map_graph.end)
```

`map()` accepts only a direct, concrete `list[T]` source and requires the mapper
input to be exactly `T`. Its mapper is registered by `map()`, rather than with
`add()`. The result preserves source order and represents an empty input as an
empty `list[Result]`; it may feed ordinary downstream nodes, including a later
map as sequential composition. `concurrency` defaults to 20 and must be a
strict integer from 1 through 4,294,967,295. It is an upper bound: a target may
apply a lower executor capacity. The local process executor currently caps a
map at 32 simultaneous child processes.

Emission produces one Graph IR 0.3 `map` node with its input, item-input,
item-output, and collected-output schemas, mapper symbol and contract, and
`maxConcurrency`. The emitted map contract is the execution boundary; no local
in-memory map behavior is part of the authoring API.

Finite maps execute through both `massive run` and the Argo target. Argo lowers
each map to a bounded nested DAG: an indexed envelope crystallizes the input,
`withParam` invokes the mapper under the declared concurrency limit, and a
collector restores source order. Duplicate values retain distinct indexes and
an empty input publishes canonical `[]` without invoking mapper code.

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

## Building the 0.1 release

From a clean checkout with Go 1.25 and `uv` installed, build the source archive
and Linux, macOS, and Windows wheels for amd64 and arm64:

```sh
./scripts/build-python-release.sh dist/python-release
uv publish dist/python-release/*
```

The release builder cross-compiles CGO-disabled Go control-plane binaries,
assigns Python-ABI-independent platform tags, checks every wheel contains its
native executable, and checks the source distribution can rebuild the Go
binary. Run `pnpm check` before publishing; its distribution acceptance test
installs a wheel in a clean environment and exercises local, Argo build, and
isolated Argo-step paths.

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

Use Massive's recursive `JsonValue` type when a result deliberately carries
open-ended metadata while retaining the portable wire constraint:

```python
from massive import JsonValue


class Result(BaseModel):
    metadata: dict[str, JsonValue]
```

This permits nested strings, safe integers, booleans, nulls, arrays, and
objects, but keeps floating-point values out of the schema and runtime payload.

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

## Workflow packages and resources

The source package root is the entry file's parent directory, with package ID
`python-main`. Without configuration, it includes root-level `*.py` files.
For nested modules and non-Python assets, put a `pyproject.toml` beside the
entrypoint:

```toml
[project]
name = "my-analysis"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = ["massive-workflows==0.1.0", "httpx>=0.28,<1"]

[tool.massive.source]
include = ["workflow.py", "analysis/**/*.py", "analysis/prompts/*.txt", "rules/*.yaml"]
```

Includes are directory-relative `pathlib` glob patterns and replace the default
`["*.py"]`. Include the entrypoint and all local modules/resources it needs.
Unknown Massive configuration fields, selected symlinks, and patterns escaping
the directory are rejected. Parent project configuration is not inherited;
workflows in separate directories can carry different dependencies and assets.
Multiple exports in one directory share that directory's package configuration.

`pyproject.toml` and `uv.lock`, when present, are always included in the source
manifest. Changes to their bytes or any included resource change package identity.
This records intended dependency inputs; it does **not** verify installed packages.

Use `importlib.resources.files("analysis")` or paths relative to `__file__` to
load packaged resources. The runner imports from a verified extracted archive,
not from the original checkout. Avoid `**/*`: do not package `.env`, credentials,
virtual environments, large datasets, or caches.

Third-party packages belong in `[project].dependencies`, not source includes.
Create and commit a lock with `uv lock`. Then, from the workflow directory:

```sh
uv sync --locked
uv run --locked massive run workflow.py --project example/analysis --input '{}'
```

The launching Python environment runs both the frontend and each step. Massive
does not install dependencies automatically. For Argo, build an immutable runner
image with the same locked dependencies and Massive version. Local execution
currently uses the active interpreter, not the container declared in the contract.

See the [packaged map example](../../examples/07-package/workflow.py) for a
nested module, a text resource, and ordered collection into a typed result.

### Secrets

CI or the platform owns obtaining and rotating credentials. Never place their
values in project metadata, lockfiles, source includes, or execution contracts.
Local Python processes currently inherit the launching environment; secret
preflight and selective task binding are not implemented. Argo rejects declared
secrets until secret-reference lowering exists.

The intended contract is logical requirements in the workflow and concrete
bindings in deployment configuration. See the [binding design](../../docs/spec/runtime-environment.md)
for the distinction between declarations, bindings, and enforcement.
