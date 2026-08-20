# Datastore Layout

Status: v0 conformance contract

Massive v0 stores compiled artifacts and run outputs in an object-store-first layout. Local filesystem and S3-compatible implementations must use the same relative object keys. The default local root is `~/.massive/store`; tests should use explicit temporary roots.

This document freezes path templates, digest encoding, and project-key normalization. Hash *field coverage* for `specHash`, `planHash`, source-package hash, and env key is owned by [`hashing.md`](hashing.md) (WS-0.6). This document defines how those digests appear in datastore keys and how run-scoped objects are namespaced.

Runners and adapters must treat keys recorded in proto-typed JSON manifests (`ArtifactRef.key`) as authoritative. They must not hardcode alternate layouts.

## Key syntax

All datastore keys are relative object paths:

- forward slashes only (`/`),
- no leading `/`,
- no empty, `.`, or `..` segments,
- no backslashes,
- no absolute paths.

Implementations must reject keys that escape the configured datastore root after resolution.

For the CLI's local filesystem store, an operator may configure one validated
relative prefix ahead of every key. `--store-prefix <key>` takes precedence over
`MASSIVE_STORE_PREFIX`; neither setting enters `specHash`, `planHash`, or any
artifact body hash. For example, prefix `tenants/acme` maps logical key
`blobs/sha256/<digest>` to physical path
`tenants/acme/blobs/sha256/<digest>`. The prefix obeys the same key syntax and
the CLI additionally rejects control and leading/trailing whitespace. It is
configuration, never part of a manifest reference. An empty environment value
means no prefix; an explicitly supplied empty flag is invalid.

## Digest references and path segments

Digests have exactly one representation per artifact family:

- **Proto-typed JSON manifests** use only the digest string `sha256:<hex>`, per [`hashing.md`](hashing.md) and the `.proto` schemas. Digest object forms are not valid in JSON artifacts.
- **JSON artifacts** (workflow specs, step invocation descriptors, plans, provenance records, and manifests) use the same digest string form.

UI and CLI output may display shortened prefixes; **datastore keys and manifest references must use the full 64-character lowercase hex digest**, never a truncated prefix.

Content-addressed **path segments** encode the same digest in a filesystem-safe form:

```text
sha256-<digestHex>
```

Example: digest `bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb` becomes path segment `sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`.

Pure content blobs use a separate layout (below): algorithm as a directory name and raw `digestHex` as the leaf segment.

### Mapping rules

| Role | Path segment or key field | Example |
|------|---------------------------|---------|
| JSON digest string | `sha256:<digestHex>` | `sha256:bbbb…bbbb` |
| `<spec-key>`, `<plan-key>`, `<env-key>`, `<package-key>`, `<project-key>` | `sha256-<digestHex>` | `sha256-bbbb…bbbb` |
| Blob leaf name | `<digestHex>` under `blobs/sha256/` | `blobs/sha256/bbbb…bbbb` |

`<spec-key>` is the path segment for `specHash`. `<plan-key>` is the path segment for `planHash`. `<package-key>` is the path segment for the source-package content hash. `<env-key>` is the path segment for the environment materialization key (environment-relevant inputs only; resource, secret, and network differences do not change it).

## Globally content-addressed artifacts

These objects are deduplicated across projects inside the configured datastore.

### Blob

Template:

```text
blobs/sha256/<digestHex>
```

Stores opaque content addressed by raw bytes. `<digestHex>` is the SHA-256 of the object body (64 lowercase hex characters).

Example:

```text
blobs/sha256/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

### Workflow spec

Template:

```text
specs/<spec-key>/workflow-spec.json
```

Example:

```text
specs/sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/workflow-spec.json
```

### Environment materialization

Templates:

```text
envs/<env-key>/manifest.json
envs/<env-key>/runtime.tar.zst
```

Examples:

```text
envs/sha256-7777777777777777777777777777777777777777777777777777777777777777/manifest.json
envs/sha256-7777777777777777777777777777777777777777777777777777777777777777/runtime.tar.zst
```

### Source package

Templates:

```text
packages/<package-key>/source-manifest.json
packages/<package-key>/source.tar
```

Examples:

```text
packages/sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/source-manifest.json
packages/sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/source.tar
```

### Compiled plan

Templates:

```text
plans/<plan-key>/workflow.json
plans/<plan-key>/provenance.json
```

Examples:

```text
plans/sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/workflow.json
plans/sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/provenance.json
```

### Target bundle

Template:

```text
targets/<plan-key>/<target>/bundle-manifest.json
```

`<target>` is the target backend id (`local`, `argo`, or a future backend name).

Example:

```text
targets/sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/argo/bundle-manifest.json
```

Target compilers may emit additional bundle files under the same `targets/<plan-key>/<target>/` prefix. Those files are recorded in `bundle-manifest.json`; their keys are bundle-relative paths joined under that prefix.

Example (companion artifact referenced from a bundle manifest):

```text
targets/sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/argo/workflow-template.yaml
```

## Project-scoped run artifacts

Run metadata and runtime JSON values are namespaced by **project key** so local run history stays organized. They are not content-addressed globally.

Compiled artifacts (specs, packages, plans, envs, blobs) remain global inside the datastore.

### Project identity

Project identity is resolved before computing `<project-key>`:

1. **Explicit config.** `massive.config.ts` may set `projectId`, for example `acme/security-workflows`.
2. **Git `origin` fallback.** If no project ID is configured, derive identity from the `origin` remote as `owner/repo`. V0 supports common GitHub- and GitLab-style SSH and HTTPS remotes.
3. **Failure.** If identity cannot be resolved, commands that write or read project-scoped run keys must fail with a configuration error asking for an explicit project ID.

Git remote normalization examples:

| `origin` URL | Normalized project identity |
|--------------|----------------------------|
| `https://github.com/acme/security-workflows.git` | `acme/security-workflows` |
| `git@github.com:acme/security-workflows.git` | `acme/security-workflows` |
| `https://gitlab.com/acme/security-workflows` | `acme/security-workflows` |

### Project-key normalization

The datastore never uses raw project identity strings as path segments.

Normalization pipeline:

1. Start from the resolved project identity string (`projectId` or normalized `owner/repo`).
2. Trim leading and trailing ASCII whitespace.
3. Lowercase ASCII letters (`A`–`Z` → `a`–`z`). Non-ASCII characters are unchanged.
4. Compute `digestHex = SHA-256(UTF-8(normalized identity))`.
5. Set `<project-key> = sha256-<digestHex>`.

Example:

```text
normalized identity: acme/security-workflows
project-key:         sha256-7a3f8c2e1b904d5a6e8f0c1d2b3a4e5f60718293a4b5c6d7e8f9012345678ab
```

(The example digest is illustrative; implementations must compute the real SHA-256 of the normalized identity.)

Project keys must not embed run IDs, timestamps, branch names, or other transient run data.

### Run object templates

`<run-id>` is an opaque run identifier chosen by the orchestrator (for example a UUID string). `<step-id>` is the graph node id. `<attempt>` is a positive integer attempt number (the local orchestrator currently uses only `1`).

Templates:

```text
projects/<project-key>/runs/<run-id>/run-manifest.json
projects/<project-key>/runs/<run-id>/inputs/<step-id>.json
projects/<project-key>/runs/<run-id>/steps/<step-id>/<attempt>/output-manifest.json
projects/<project-key>/runs/<run-id>/result.json
```

Mapped invocations preserve their static `<step-id>` and add every ordered
outer-to-inner scope frame. `mapId` is a safe segment; `index` is a canonical
base-10, nonnegative JSON-safe integer with no leading zeroes (except `0`).
The same frame path is used for item input and its descriptor filename, so
concurrent map items cannot overwrite each other:

```text
projects/<project-key>/runs/<run-id>/inputs/<step-id>/scopes/maps/<map-id>/items/<index>.json
projects/<project-key>/runs/<run-id>/steps/<step-id>/scopes/maps/<map-id>/items/<index>[/maps/<nested-map-id>/items/<nested-index>]/<attempt>/output-manifest.json
```

Static invocations omit `scope` and retain the original static paths.

`run-manifest.json` is the v3 (`schemaVersion: 3`, `encoding: "json-v3"`) run
manifest the orchestrator records when it creates a run: plan hash, run status,
per-step attempt/artifact records, finite-map item records, and durable
decision outcomes. A selected
decision record has `nodeId`, `status: "selected"`, and `selectedCase`. A failed
record has `status: "failed"` and a safe diagnostic; an inactive nested decision
has `status: "skipped"` and the same structured `skipReason` used by steps. The
orchestrator persists a successful selection before it schedules downstream
nodes, and a future resume or replay must use the journaled selection rather
than run the classifier again.

An unselected branch step has status `"skipped"`, no attempts, and a
`skipReason` with `kind: "decision-not-selected"`, the `decisionId`, and the
step's `case`. Selected and non-branch steps retain the ordinary attempt record.
An attempt output contains the immutable output-manifest reference and its
content-addressed body reference.

A map step owns a dense, source-ordered `items` array. Each item remains tied
to its static map node by an ordered execution scope and records its own
attempt. Successful collection is source-index ordered, never completion
ordered. An item that was not dispatched because the map failed has terminal
status `"not-started"`, zero attempts, and a safe diagnostic. A successful map
requires every item to succeed. A failed map has no pending or running items
and never publishes the static collection manifest. An empty map succeeds with
an empty item array and publishes canonical `[]` at the map node's static
output slot.

`result.json` is the final run result artifact the CLI surfaces as the run's
output location. Both are canonical JSON. The local orchestrator currently
records only attempt `1` for each node; retry scheduling is intentionally not
implemented by this transport slice.

The v3 run-manifest protocol has no compatibility reader or dual-write mode.
An implementation must reject an earlier run manifest instead of guessing or
silently upgrading its routing state. The step invocation descriptor is
independently versioned and remains `schemaVersion: 2`, `encoding: "json-v2"`.

Examples:

```text
projects/sha256-7a3f8c2e1b904d5a6e8f0c1d2b3a4e5f60718293a4b5c6d7e8f9012345678ab/runs/550e8400-e29b-41d4-a716-446655440000/run-manifest.json
projects/sha256-7a3f8c2e1b904d5a6e8f0c1d2b3a4e5f60718293a4b5c6d7e8f9012345678ab/runs/550e8400-e29b-41d4-a716-446655440000/inputs/double.json
projects/sha256-7a3f8c2e1b904d5a6e8f0c1d2b3a4e5f60718293a4b5c6d7e8f9012345678ab/runs/550e8400-e29b-41d4-a716-446655440000/steps/double/1/output-manifest.json
projects/sha256-7a3f8c2e1b904d5a6e8f0c1d2b3a4e5f60718293a4b5c6d7e8f9012345678ab/runs/550e8400-e29b-41d4-a716-446655440000/result.json
```

Step outputs are committed by an immutable canonical JSON manifest at the
attempt key; the manifest references a canonical JSON body under
`blobs/sha256/<digestHex>`. Inputs and final result locations retain their
current run-scoped JSON layout. Each artifact record carries its schema hash,
content hash, content type, datastore key, and producing run/node/attempt when
applicable.

This is a breaking v0 step-output rename from `output.json` to
`output-manifest.json`. There is no compatibility read or dual write: a
workflow adopts the v2 SDK and deployment namespace as one migration.

## Layout overview

```text
blobs/sha256/<digestHex>
specs/<spec-key>/workflow-spec.json
envs/<env-key>/manifest.json
envs/<env-key>/runtime.tar.zst
packages/<package-key>/source-manifest.json
packages/<package-key>/source.tar
plans/<plan-key>/workflow.json
plans/<plan-key>/provenance.json
targets/<plan-key>/<target>/bundle-manifest.json
projects/<project-key>/runs/<run-id>/run-manifest.json
projects/<project-key>/runs/<run-id>/inputs/<step-id>.json
projects/<project-key>/runs/<run-id>/steps/<step-id>/<attempt>/output-manifest.json
projects/<project-key>/runs/<run-id>/result.json
```

Shared datastore contract tests (WS-3.1) and cross-language clients (Go + TypeScript) must read and write these keys identically.
