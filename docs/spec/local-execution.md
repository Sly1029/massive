# Local execution bindings and workers

Local tasks use the existing target-neutral `ExecutionContract.secrets` name/ref
pairs. Local binding sources belong to the executor, just as Kubernetes Secret
name/key bindings belong to Argo. No provider framework or graph schema change
is needed. Binding values never enter plans, descriptors, environment identities,
journals, or lifecycle events.

`massive run --secret-bindings bindings.json` maps logical refs to one local
source per ref:

```json
{
  "catalog-api": {"env": "CI_CATALOG_TOKEN"},
  "signing-key": {"file": "secrets/signing-key"}
}
```

Relative files resolve beside the bindings file. Their entire contents become
the value, without trimming: `echo token > file` includes the trailing `\n`.
Symlinks are followed. Each value is limited to 32 KiB; the complete task
environment is limited to 128 KiB (24 KiB on Windows), including the base and
pass-through values. Secret names must not collide with the base or explicit
pass-through names. Only declared refs are resolved; required sources
are checked before task dispatch. There is no implicit `.env` loading or secret
discovery. Argo keeps its existing `{name,key}` sources and lowering.

The task environment is constructed before the interpreter starts. Its base
contains `PATH` for executables, `HOME` for user tool configuration, `TMPDIR`,
`TEMP`, and `TMP` for scratch, `LANG` and standard `LC_*` locale categories,
`TZ` for timezone, and `SSL_CERT_FILE`/`SSL_CERT_DIR` for TLS trust configuration.
Windows also includes `SYSTEMROOT`, `COMSPEC`, `USERPROFILE`, `SYSTEMDRIVE`,
`WINDIR`, `PATHEXT`, `LOCALAPPDATA`, `APPDATA`, and `USERNAME` for native
process startup and home lookup. Massive's runner selectors are explicit runtime
configuration. Other launching variables require repeated `--pass-env NAME`
options for non-secret configuration, or declared secret bindings. Passing a
variable does not satisfy a declared secret requirement.

Python tasks use the absolute interpreter checked by dependency preflight,
with `-I`; activation variables such as `VIRTUAL_ENV`, `PYTHONPATH`, and
`PYTHONHOME` are unnecessary. Preflight checks metadata before graph import and
pins uv's offline check to that interpreter and prefix. The metadata probe
uses this reduced base; uv retains the control plane's allowlisted `UV_*`
settings for cache, index, and resolution inputs. Secrets and scheduling
stay outside dependency realization identity. Graph import remains trusted code
in the launching environment; selective task environments are not a sandbox.

For a corporate network, explicitly pass the configured proxy and extra CA
variables, for example:

```sh
massive run workflow.py --pass-env HTTPS_PROXY --pass-env HTTP_PROXY \
  --pass-env NO_PROXY --pass-env REQUESTS_CA_BUNDLE --pass-env NODE_EXTRA_CA_CERTS
```

Only name variables that are set; proxy variables are outside the base allowlist.

`massive run --max-workers N` defaults to `runtime.GOMAXPROCS(0)`
(the Go runtime CPU budget, respecting container CPU limits) and bounds
live local task processes across every ready branch, map, retry, and exit hook.
Each map's declared concurrency remains an additional upper bound. Independent
ready nodes execute concurrently. Readiness is re-scanned after each node settles,
so a fast branch can start its dependents while a slow sibling is still running.
Network-bound maps may benefit from a higher explicit worker count.
Graph resolution and durable journal writes
are coordinated. Worker waits observe cancellation and do not consume attempt
timeouts. Results retain declared merge order and source-indexed map order.

`RunHooks.AfterStepInvocation` may be called concurrently by independent node
workers; hook implementations must be goroutine-safe.
