# Artifact Runtime

Status: v2 canonical-JSON output protocol

The Artifact Runtime is the deep module that turns one schema-valid canonical
JSON value into an immutable workflow output. Step code continues to return a
typed value. Language runners and orchestrators use the runtime; authors do not
handle object-store keys or credentials.

This first slice covers one JSON output per step. Blob, Tree, channel, streaming,
and multi-entry materializations remain outside the interface until they have a
concrete producer and consumer.

## Interface

The cross-language behavior has two operations:

```text
PublishJSON(destination, producer, canonicalBody) -> PublishedJSON
ResolveJSON(destination, expectedProducer) -> PublishedJSON + canonicalBody
```

`destination` contains the pinned schema reference and deterministic manifest
key. The key must match the producer's project, run, node, ordered execution
scope, and attempt slot. Static producers omit `scope`; a mapped producer
carries non-empty outer-to-inner `scope.frames`, each
`{kind:"map-item",mapId,index}`.
Callers cannot choose a body key: the runtime derives it from the exact bytes.
`producer.projectKey` is the normalized datastore namespace key, exactly
`sha256-` followed by 64 lowercase hexadecimal characters. It is the digest of
the normalized project identity, not the raw identity, repository path, or a
hierarchical path segment.

The versioned manifest shape is frozen by
`conformance/schema/data-artifact-manifest.schema.json`. Its encoding is
`canonical-json-v1`, the integer-only canonical field-tree format defined in
[`hashing.md`](../../conformance/schema/hashing.md). A future incompatible
canonicalizer requires a new encoding value and consumer support range.

## Publication

Publication is ordered:

1. Resolve the schema by its `sha256:<digest>` blob reference and verify its
   exact canonical bytes and digest.
2. Require the output to be canonical JSON and validate it against that schema.
3. Hash the exact output bytes and conditionally create
   `blobs/sha256/<digest>` with content type `application/json`.
4. If the body already exists, read it and require exact bytes and content type.
   Never repair or overwrite it.
5. Construct and validate the canonical `DataArtifactManifest`.
6. Conditionally create the deterministic attempt manifest with content type
   `application/vnd.massive.data-artifact-manifest+json`.
7. If the manifest exists, read it and require byte equality. Equality is an
   idempotent retry; inequality is a permanent correctness failure.
8. Journal attempt success only after resolving the published manifest again.

This is atomic visibility, not a multi-object transaction. A crash before the
manifest may leave an unreachable body. A crash after the manifest is
recoverable from the deterministic attempt key. Consumers never discover
outputs by listing body keys.

## Breaking Output Migration

This is a breaking v1 output migration: a step output is now published at
`.../output-manifest.json`, rather than the legacy `.../output.json` value
location. The runtime provides neither a compatibility reader nor dual
publication. Workflows move to the v2 SDK and its separate deployment namespace
as a complete unit.

## Integrity And Trust

Manifest `producer` fields are provenance assertions checked against the
invocation descriptor. They are not signatures and do not authenticate the
writer. The configured artifact-store binding is the security-realm seam:

- workload credentials must be scoped to the intended realm;
- step writers need conditional-create access to content bodies and their own
  run prefix, not unrestricted manifest overwrite access;
- credentials never appear in WorkflowSpec, WorkflowPlan, DeploymentSpec,
  invocation descriptors, or manifests;
- a deployment needing mutually untrusted writers requires a broker, scoped
  credentials, or signed attestations before it is a supported target.

Body content type is a fixed protocol constant, not producer-selected parsing
metadata. Schema references are content hashes, so mutable schema names cannot
change the meaning of an existing publication.

## Recovery, Retention, And Argo

Conditional-create conflict is followed only by a stable read and comparison.
The runtime never performs `Exists` followed by an unconditional write and
never rewrites a conflicting key.

Artifact immutability is enforced at this module boundary: publication uses
`Datastore.Put(..., IfAbsent: true)` for every body and output manifest, then
requires the existing object to match exactly. `Datastore` itself deliberately
also supports ordinary replacement writes for mutable control records such as
run journals; it is not an immutable-store abstraction. The local adapter
uses link-based conditional installation to publish an `IfAbsent` object's
complete content-type sidecar before its body becomes visible to concurrent
processes. It does not fsync object, metadata, or directory entries, so this
is process-atomic behavior—not a power-loss durability guarantee. An
interrupted process can leave metadata without a body, which a matching retry
completes.

Content bodies are shared within one configured security realm. Retention must
eventually mark reachable bodies from immutable manifests, apply a grace period
for interrupted publications, and then sweep unreachable bodies. Deleting from
an individual failed attempt is unsafe because another manifest may reference
the same body.

An Argo step driver must consume the same invocation descriptor and Artifact
Runtime. Pod success is insufficient by itself: the driver or controller must
resolve the deterministic output manifest before recording success or making
the output available downstream.
