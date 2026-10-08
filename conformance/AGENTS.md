# Shared contracts and conformance

Schema sources in `schema/` define shared wire contracts. Regenerate Protobuf
bindings with `../scripts/generate-proto.sh`. For spec/plan fixtures, emit from
the language frontend and compile through Go, then replace the affected fixture
bytes. There is no golden-update script; do not hand-edit hashes to make a
failing fixture pass.

Contract migrations must update both language emitters and Go readers together.
Keep only the current transport/version; obsolete inputs should explain that
they need rebuilding. Test rehashed but semantically invalid plans as well as
malformed bytes. Graph fuzzing should cover topology and exhaustive decisions,
not only the parser's tolerance of random JSON. The plan body is
invariant apart from spec-derived hashes: declaration order and spec-local
schema, contract, or environment references must not change it. `specHash`
covers the spec bytes, so it, `provenance.sourceSpecHash`, and the `planHash`
over them still change with authoring order.

Fixtures describe real executable requirements. Change a fixture at its source
when its environment or network intent changes; tests must not silently rewrite
those requirements just to get through a target compiler.

S3 tests share the source-pinned image reference in `minio/image-reference`.
Build it with `../scripts/build-minio-test-image.sh` before running Docker-backed
fixtures. The Argo gate loads that image into its owned cluster and binds the
imported manifest digest, so it exercises the same server as the language tests.

Invocation descriptors are current-only v3/json-v3. Channel fields are absent,
including empty arrays; validate and reject them before user code runs. The
materialization container selection is a plain field: preserve its proto-JSON
projection and identity when changing generated implementation types.
