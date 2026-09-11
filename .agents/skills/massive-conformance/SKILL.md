---
name: massive-conformance
description: Validate Massive graph/IR, artifact transport, CLI distribution, or Argo execution changes using its functional conformance gates. Use when changing these boundaries or preparing a Massive PR for merge.
---

Read the checkout's root and affected nested AGENTS.md, then `docs/roadmap.md`.
Use that checkout's scripts and schemas as the source of truth; resolve paths
from its repository root, not a remembered worktree.

Choose evidence for the changed boundary:

- Graph parsing or control flow: preserve a failing generated case as a small
  regression; run `packages/python/tests/test_graph_properties.py` and
  `scripts/fuzz.sh`. Parsing success
  alone does not prove decision/select or map execution.
- CLI or language adapters: run `scripts/test-python-distribution.sh` as well as
  affected unit tests. The wheel launcher sets Python-related environment variables;
  verify TypeScript dispatch still chooses its own adapter.
- Storage or Argo lowering: run `scripts/test-argo.sh` and inspect its workflow
  results. Generated manifest validation alone cannot prove cross-pod hydration,
  inactive branches, empty maps, or item failure behavior.
- Finish code changes with the required mock ban check and the repository's
  `pnpm check`. Re-run further gates when subsequent changes affect their evidence.

For a manual kind investigation, identify the kubeconfig, context, and owned
cluster explicitly. Load the actual immutable image manifest digest matching the
node architecture. Keep diagnostic resources until the failure is understood;
remove only resources owned by the test. On a host using snap Docker, put build
contexts and the TMPDIR used by `kind load` under the home directory: snap cannot
read the shell's `/tmp` paths reliably.

Before an authorized merge, request the review required by the current session,
resolve concrete findings, and check CI against the final pushed commit. Report
skipped or unavailable infrastructure gates as limits on the evidence. A local
kind/MinIO pass does not establish cloud workload identity or production readiness.
Store durable module invariants in the nearest AGENTS.md as they are learned;
keep run IDs, temporary credentials, and one-off logs out of those instructions.
