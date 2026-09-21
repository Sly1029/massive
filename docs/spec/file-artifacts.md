# File artifacts

Status: implemented Python runtime contract, version 1.

`Blob` and `Tree` extend typed JSON dataflow without adding graph primitives or
embedding storage credentials in values. Both are frozen Pydantic models that
may appear at any depth in supported inputs and outputs. A reference contains
exactly `kind`, `hash` (a SHA-256 reference), and `size` (a nonnegative safe
integer). Unknown versions or fields fail validation. Local source paths,
hydrated paths, and runtime bindings are never serialized.

## Storage and identity

A blob's hash covers its exact bytes and its size counts those bytes. Its key is
`file-artifacts/blobs/sha256/<digest>` and content type is
`application/octet-stream`.

A tree's hash and size cover a canonical-json-v0 manifest:

```json
{"directories":["empty"],"files":[],"version":1}
```

A file entry contains `path`, `blob` (a Blob reference), and `executable` (a
boolean). All directories, including nonempty directories, appear explicitly.
Directory paths and file entries are separately sorted by Unicode code point.
Paths are relative POSIX strings with no empty, dot, parent, backslash, NUL, or
absolute components. Paths are unique across both collections and every parent
must be declared as a directory. Readers validate the whole manifest before
creating directories. No archive extraction is involved.

Tree manifest keys are `file-artifacts/trees/sha256/<digest>`, with content type
`application/vnd.massive.tree+json`. Separating file kinds from each other and
from JSON artifact bodies avoids content-type conflicts for identical bytes.
File content is deduplicated across trees within the configured datastore.
The empty tree hashes canonical `{"directories":[],"files":[],"version":1}`.

Executability is one boolean, restored as 0755 or 0644. Directory modes, mtimes,
ownership, ACLs, and extended attributes are excluded. Symlinks and special
files are rejected. Hardlinked source files are read as ordinary file contents;
hydration always creates independent inodes. V1 targets case-sensitive POSIX
filesystems. This flat manifest shares file bodies, not subtree manifests.

## Publication and working copies

`from_path()` captures identity; source files must remain available and unchanged
until publication. The runner serializes typed output with a datastore binding:

1. Verify captured file bytes and immutably publish each blob.
2. Publish tree manifests after their complete file closure exists.
3. Publish the ordinary schema-validated JSON output body and task manifest.
4. Record task success through the existing runner/orchestrator protocol.

Missing, changed, or corrupt bodies fail publication. Failed publication may
leave unreachable bodies but cannot commit a successful task output. This is
not an atomic filesystem snapshot of concurrently changing application files.

Input validation binds handles to the invocation's datastore and scratch area.
`path()` verifies hashes, sizes, and content types before exposing a working
copy. Each distinct handle hydration creates a separate copy. Repeated `path()`
calls on one handle reuse its copy; rebinding to another invocation discards
the prior scratch association. Equality and hashing depend only on the reference,
not the binding or local mutations.

Forwarding an existing reference verifies its original content closure and
preserves identity. Only an explicit new snapshot publishes working-copy edits.
Scratch is deleted when the runner exits. Workload credentials come from the
existing datastore adapter and never from the reference. A datastore root/prefix
is the isolation boundary; hashes are identities, not access-control tokens.

## Operational limits and release gates

The implementation buffers a file while hashing, uploading, or downloading it.
Trees hydrate lazily as a whole on first access. Streaming, scratch quotas,
resumable transfer, per-file lazy access, and cross-platform filename translation
are not implemented. Argo still rejects large source packages; this feature
only removes file payloads from graph parameters.

Any future retention collector must follow typed JSON references into tree
manifests and their file blobs, including references nested in lists and models.
Deleting solely by a task manifest's immediate JSON body is unsafe. No collector
or retention policy is introduced here.

Functional gates cover filesystem publication, real S3-compatible storage with
independent reader processes, the Python-to-Go-to-subprocess map path, and clean
wheel installation. A live Argo cluster run remains a separate release gate.

## Author workspace

Python `StepContext.workspace` is a runner-owned writable directory for temporary
outputs. Each invocation gets a distinct directory, separate from source archives
and hydrated input files. Its lifetime includes output serialization and file
publication. Success, author failure, and serialization failure all remove it.
Use Blob/Tree handles to carry files across invocations; workspace paths have no
cross-task meaning. Process-tree cancellation and scratch quotas remain separate
execution-policy work. Abrupt process termination can leave local scratch behind.
