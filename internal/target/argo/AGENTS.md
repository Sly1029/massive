# Argo lowering

Read `../../../docs/spec/argo-backend.md` when changing pod templates, control
flow, or runtime transport. Deployment artifacts contain bindings, never values
of credentials; execution requirements remain separate from deployment binding.

Only user invocations need shared datastore access and application execution
resources. Decision/select, map expansion, and collection control pods execute
no author code; keep user credentials and resources off these pods. Bind each
logical application-secret ref through DeploymentSpec and reject unbound refs
before emitting a bundle. Reserve storage/runtime environment names even when
no explicit storage credential Secret is configured.

Inactive branches must not read nonexistent outputs. Select waits for inactive
or terminal alternatives, requires a successful chosen source, and resolves
only that source. Map item order follows source indices, including empty maps.

Reject unsupported network/storage requirements during compilation. Pod-local
storage cannot carry artifacts between tasks. The embedded source size limit is
an explicit error, not permission to drop files.

For lowering or transport changes, exercise `../../../scripts/test-argo.sh`.
Schema-valid YAML and isolated runtime tests do not prove live controller behavior.
