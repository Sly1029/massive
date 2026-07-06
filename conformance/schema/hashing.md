# Canonical Hashing

This document defines the v0 canonical hashing rules for `specHash`,
`planHash`, `sourcePackageHash`, environment keys, and runtime-artifact hashes.
The normative implementation baseline is `stableStringify` plus SHA-256 in
`packages/sdk/src/stable.ts`.

The canonicalization is **RFC 8785 (JSON Canonicalization Scheme, JCS)**, with
one additional v0 restriction: numbers are limited to canonical safe integers
(see [Canonicalization details](#canonicalization-details)). Implementations
in other languages should target RFC 8785 conformance rather than
reverse-engineering ECMAScript behavior; the rules spelled out below are the
JCS behaviors most often gotten wrong, and the golden vector exercises them.

## Canonical Field Tree

Hash inputs are JSON-compatible field trees: `null`, booleans, finite JSON
numbers, strings, arrays, and objects with string keys. Values outside that
field tree, such as `undefined`, functions, symbols, non-finite numbers, maps,
sets, dates, and binary values, must be lowered to JSON-compatible values before
hashing.

Canonicalization is:

1. Recursively sort every object by key using the same ordering as
   ECMAScript `Object.keys(value).sort()`.
2. Preserve array order exactly.
3. Serialize the sorted field tree with JSON syntax equivalent to
   `JSON.stringify`, with no insignificant whitespace.
4. Hash the UTF-8 bytes of that canonical JSON string with SHA-256.

The stored key form is `sha256:<hex>`, where `<hex>` is the full lowercase
64-character SHA-256 digest. Manifests must record the algorithm. UI and CLI
surfaces may display shortened prefixes, but datastore keys, manifest
references, and conformance fixtures use the full key.

### Canonicalization details

The following rules pin the exact ECMAScript semantics that "equivalent to
`JSON.stringify`" implies. They match RFC 8785; non-JavaScript implementations
must reproduce them byte for byte, and the golden vector exercises each rule.

- **Key ordering is UTF-16 code-unit order** (ECMAScript
  `Object.keys(value).sort()`), not UTF-8 byte order or code-point order. The
  orders diverge for keys containing supplementary-plane characters: a key
  beginning with U+1F600 (surrogate pair `D83D DE00`) sorts *before* a key
  beginning with U+E000 in UTF-16, but after it in UTF-8 byte order.
- **String escaping is exactly `JSON.stringify`'s:** `\"`, `\\`, `\b`, `\t`,
  `\n`, `\f`, `\r`, and `\u00XX` for other control characters below U+0020.
  Everything else is emitted raw — including `<`, `>`, `&`, U+2028, and
  U+2029. Implementations must not apply HTML-safety or JS-source-safety
  escaping. Strings must be well-formed Unicode (no lone surrogates).
- **Numbers in v0 canonical field trees are restricted to canonical safe
  integers:** base-10, no leading zeros, no sign on zero, no fraction or
  exponent, absolute value at most 2^53 − 1. This is a strict subset of RFC
  8785's number serialization (ECMAScript `Number::toString`). Producers must
  normalize or reject anything else. Full RFC 8785 number serialization is
  required of any implementation before non-integer numbers may appear in
  hashed field trees (owed by the Go compiler workstream, WS-2).

Hashes are over canonical field trees, never over JSON source whitespace and
never over Cap'n Proto wire bytes. Canonical compiled artifacts, bundle
manifests, JSON projections, and hash coverage must not include wall-clock
timestamps. If audit timing is needed, it belongs in side metadata outside the
hashed artifact.

## Hash Coverage

### `specHash`

`specHash` covers the full `WorkflowSpec` field tree emitted by the frontend:

- graph IR, including workflow name, schemas referenced by graph boundaries,
  start/end node IDs, node definitions, edges, and merge-input ordering;
- schema table;
- symbol table;
- source package table, including package IDs and referenced package content
  hashes;
- environment table;
- effective execution-contract table;
- per-node execution-contract references;
- target requests and target-specific authoring inputs present in the spec.

`specHash` does not cover source JSON whitespace, storage path names, datastore
write time, compile time, run IDs, or any other wall-clock timestamp.

### `planHash`

`planHash` covers the canonical compiled plan field tree:

- `specHash`;
- GraphIR;
- ExecutionContract entries;
- symbol table;
- target config;
- patches;
- compiler identity and version;
- environment materialization references;
- datastore manifest references;
- mediation provider identity.

The hash is computed from the canonical field tree used for the plan's JSON
projection, not from Cap'n Proto segment bytes. Generated deploy artifacts must
carry the `planHash`; backends must reject runs when the annotation is missing
or mismatched.

### `sourcePackageHash`

`sourcePackageHash` covers executable source content through an exact source
package manifest:

- exact included file list;
- each included file's normalized relative path;
- each included file's content hash;

Broad implicit packaging is out of scope for v0. A changed included file changes
the package hash. A changed excluded file does not. Package IDs, local absolute
root paths, symbols, target requests, and datastore write locations are outside
the source package content hash.

### Environment Key

The environment key covers only dependency-environment inputs:

- environment kind;
- runtime version, such as Node version;
- package manager and package-manager version;
- lockfile hash;
- manifest hash;
- workspace package hashes;
- platform;
- architecture;
- materializer identity and version;
- materializer build arguments.

The environment key must not include resources, secrets, network policy,
scheduling, retry policy, run IDs, target queue placement, or wall-clock
timestamps. Those fields may affect execution contracts or target scheduling,
but they do not define the dependency environment.

### Runtime-Artifact Hash

A runtime-artifact hash covers the exact bytes of the persisted artifact payload.
The hash does not cover datastore object keys, content type, schema reference,
producer step ID, run ID, validation status, write time, or other side metadata.
Those fields are stored in artifact manifests or run records outside the payload
digest.

## Golden Vector

The shared conformance vector is:

- input: `conformance/fixtures/hashing/canonical-input.json`
- expected key: `conformance/fixtures/hashing/canonical-input.sha256`

Implementations must parse the input JSON into a field tree, canonicalize it
with the rules above, hash the canonical UTF-8 bytes, prefix the lowercase digest
with `sha256:`, and compare it to the expected key.
