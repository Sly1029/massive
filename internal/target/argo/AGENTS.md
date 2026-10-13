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

Placement defaults reach every pod; a node override reaches only the pods
running that step or map node's author code, never control pods. Resolve
placement per template instead of using Argo's workflow-level pod defaults, and
keep the plan's platform node selector keys owned by the plan.

The exit hook runs from `onExit` through a DAG task with `continueOn`, so its
failure cannot become the workflow's status. `ExitStatus` recomputes pod
template names to find the failed node; keep `templateNodes` aligned with the
names `workflowTemplate` generates.

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
`activeDeadlineSeconds` on ordinary task templates. The plan's run deadline is
on the workflow spec. Exit-handler pods are exempt from that deadline, so the
hook template must carry its own `activeDeadlineSeconds` equal to the required
finite hook timeout.

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

Exit outcomes use terminal task boundaries: retry parents and outer map DAGs,
identified by the generated task name and template. Failed attempt pods alone
do not prove a terminal failure; return an unknown failed node for them.

A collecting map loops over per-item DAGs that continue past the item pod.
A missing result carries the terminal pod's Argo node ID. Absence alone never
establishes process death: the collector reads terminal Workflow/Pod status,
matches the node-id annotation (not a label), requires main-container OOMKilled
or signal evidence plus schema-valid last phase `author`, and counts the Retry
node's actual children. Eviction, missing pods, fatal termination markers,
and ordinary/executor exits fail the run. Preserve pods through collection.
Only the collecting-map control pod mounts a Kubernetes API token; its service
account needs Workflow get and Pod list in the deployment namespace. Application
pods retain no API token. Runtime errors report an unindexed fatal envelope
from before datastore opening and decoding; failed result writes also report
the fatal marker before further result I/O. Phase/fatal records follow
`conformance/schema/execution-evidence.schema.json`; record hydration before
setup and bracket the Python function call with author/publish transitions.
Use File termination policy and fixed-size, single-write updates of the
kubelet-mounted inode; rename cannot replace that mount. Missing or non-author
phase evidence fails the run. Preserve the reconciliation diagnostic so live
OOM conformance proves it exercised a missing envelope.
