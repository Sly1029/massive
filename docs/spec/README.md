# Spec Index

Specification documents for Massive. The active prioritization lives in
[../roadmap.md](../roadmap.md); background research lives in
[../research/](../research/).

## Normative

- [Workflow Platform v2 Direction](workflow-platform-v2.md) — the accepted
  product and architecture direction: Python-first SDK, immutable explicit
  dataflow, the next graph model.
- [Overview](overview.md) — the implemented v0 substrate and compile pipeline.

## Design drafts

- [Authoring Model](authoring-model.md) — portable authoring semantics; retains
  the TypeScript forms. Channel/state surface is removed in favor of explicit dataflow.
- [IR And Datastore](ir-and-datastore.md)
- [Workflow Packaging and Environment Materialization](environment-materialization.md) —
  implemented source packaging and the next dependency realization contract.
- [Runtime Environment Bindings](runtime-environment.md) — workflow requirements,
  deployment-owned credentials, and explicit enforcement limits; no provider framework.
- [Artifact Runtime](artifact-runtime.md)
- [Argo Backend](argo-backend.md)
- [Testing Strategy](testing-strategy.md)
- [Open Questions](open-questions.md)

## Archived

- [Implementation Roadmap](archive/implementation-roadmap.md) — the original
  TypeScript-first delivery plan; preserved for task numbering and history.
  Superseded by [../roadmap.md](../roadmap.md) and
  [Workflow Platform v2 Direction](workflow-platform-v2.md).
