# Argo lowering

Read `../../../docs/spec/argo-backend.md` when changing pod templates, control
flow, or runtime transport. Deployment artifacts contain bindings, never values
of credentials; execution requirements remain separate from deployment binding.

Only user invocations need application secrets and execution resources.
Decision/select, map expansion, and collection control pods execute no author
code; keep application secrets and resources off them. They do mount the
shared datastore, with any bound storage credentials, because value parameters
may reference bodies they must validate, expand, or collect. Bind each
logical application-secret ref through DeploymentSpec and reject unbound refs
before emitting a bundle. Reserve storage/runtime environment names even when
no explicit storage credential Secret is configured.

Inactive branches must not read nonexistent outputs. Select waits for inactive
or terminal alternatives, requires a successful chosen source, and resolves
only that source. Map item order follows source indices, including empty maps.

Reject unsupported network/storage requirements during compilation. Pod-local
storage cannot carry artifacts between tasks. The embedded source size limit is
an explicit error, not permission to drop files.

`runtimeTransport` is a DeploymentSpec binding: changing it must change the
deployment hash and runtime ConfigMap name, never the plan hash. Selection is
explicit; never switch transports by size. `object-store-v0` ConfigMaps hold
only the plan, and runner templates pin each archive digest from the verified
materialization manifest. Runtime pods trust only those pinned digests, not
whatever object is present at a key.

Source archive keys include the archive digest, not only the package identity:
different tar encodings of one package must never contend for a key. A missing,
oversized, or mismatched published archive must stop the retry expression:
runner exit 64, or 68 when Python pod preflight fetches and verifies it first.
Do not add a Go-side precheck that turns it into a retryable infrastructure
error, and never preflight a missing body as an empty project.

Retry strategies belong only on runner templates. Keep the non-retryable exit
list aligned with the runner exit codes and the runtime's preflight exit (68), pass `{{retries}}` so the runtime
derives the attempt, and enforce timeouts inside the runtime instead of with
`activeDeadlineSeconds`.

For lowering or transport changes, exercise `../../../scripts/test-argo.sh`.
Schema-valid YAML and isolated runtime tests do not prove live controller behavior.

Value parameters are canonical JSON up to 4 KiB, otherwise `@` plus a value
reference, with exactly one valid spelling between tasks; mapper results inline
at most 100 bytes. Never concatenate parameters into JSON text (merge sources
are separate parameters). Workflow inputs are inline JSON only: steps take them
with `--workflow-input`, and a decision or map reads them through the
`workflow-entry` task. Keep decisions, selects, and map expansion read-only:
forward received references and slice referenced lists in item pods. Only map
collection and the entry task publish values. Bound reads by the reference's
declared size, and keep maps within the collector's parameter budget.

A map that collects item failures loops over per-item DAGs that continue past
the item pod and default their result to `{"lost":true}`: Argo aggregates only
successful loop children, and an OOM-killed container cannot report for itself.
Keep every non-retried exit path of the item runtime writing an envelope (an
outcome or the fatal marker), so "lost" can only mean the pod died after its
last attempt. Only the item runtime classifies failures, through the local
orchestrator's function, so both targets build identical outcomes.
