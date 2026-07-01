@0xc1521df4fcee1afa;

using Plan = import "workflow-plan.capnp";

# Target bundle manifest emitted by the Go compiler for local, Argo, and future
# backends. The bundle is target output; it is not the canonical WorkflowPlan.

struct TargetBundleManifest {
  schemaVersion @0 :UInt16;
  target @1 :Plan.TargetKind;
  planHash @2 :Plan.HashRef;
  bundleHash @3 :Plan.HashRef;
  files @4 :List(EmittedFile);
  validations @5 :List(ValidationResult);
  provenance @6 :BundleProvenance;
}

struct EmittedFile {
  path @0 :Text;
  artifact @1 :Plan.ArtifactRef;
  role @2 :FileRole;
}

enum FileRole {
  unknown @0;
  manifest @1;
  workflowTemplate @2;
  localRunManifest @3;
  descriptor @4;
}

struct ValidationResult {
  name @0 :Text;
  passed @1 :Bool;
  diagnostic @2 :Text;
}

struct BundleProvenance {
  compilerName @0 :Text;
  compilerVersion @1 :Text;
}
