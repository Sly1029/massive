# Massive

Massive is a typed workflow SDK with a Go control plane. Python authors define
a static graph with Pydantic models; Massive compiles it to a protobuf-owned
plan and executes that same plan locally or lowers it to Argo Workflows.
The local runner also runs inside ordinary CI jobs without a hosted control plane.

The `massive-workflows` platform wheel contains both the Python authoring/runtime
package and the matching native Go CLI:

```sh
uv add massive-workflows
uv run massive run workflow.py --input '{"value": 21}'
```

Each workflow directory can own its dependencies and source/resource allowlist in
`pyproject.toml`. See [workflow packaging](packages/python/README.md#workflow-packages-and-resources).
The local runner uses the launching Python environment; it does not install
dependencies, enforce the declared container, or provide a sandbox.

For Argo, use an immutable runner image containing the same
`massive-workflows` version, then build and apply the generated runtime assets
and `WorkflowTemplate`:

```sh
uv run massive build workflow.py \
  --output .massive/argo \
  --namespace workflows \
  --service-account massive-runner

kubectl apply -f .massive/argo/runtime-configmap.json
kubectl apply -f .massive/argo/workflow-template.yaml
argo submit -n workflows --from workflowtemplate/my-workflow \
  -p 'input={"value":21}' --watch
```

Builds also emit portable materialization inputs and a manifest bound to the
deployment. Compilation can consume those inputs without the original checkout.
Source archives are verified; image references are pinned declarations, not
registry or dependency attestations. See the
[materialization contract](docs/spec/materialization-contract.md).

Version 0.1 supports static graphs and finite maps on Argo, and static graphs,
exhaustive decisions, and finite maps locally. Argo source transport is
intentionally small and self-contained for the first release; larger source
bundles and values will move to the existing artifact-store seam without
changing the SDK or compiled plan.

See [the Python guide](packages/python/README.md) for the complete authoring and
deployment walkthrough, or [the graph examples](examples/README.md) for graph
shapes from passthrough through decisions and maps. Direction and
prioritization live in [the roadmap](docs/roadmap.md) and
[the spec index](docs/spec/README.md).
