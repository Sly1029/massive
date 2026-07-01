@0xa1aa2f4efcb2e4db;

# Canonical backend-compiled plan boundary for Massive v0.
# Frontend SDKs emit WorkflowSpec JSON; the Go compiler emits this WorkflowPlan.

struct HashRef {
  algorithm @0 :Text;
  digestHex @1 :Text;
}

struct ArtifactRef {
  key @0 :Text;
  hash @1 :HashRef;
  contentType @2 :Text;
}

struct WorkflowPlan {
  schemaVersion @0 :UInt16;
  planHash @1 :HashRef;
  specHash @2 :HashRef;
  graph @3 :GraphIR;
  schemas @4 :List(SchemaEntry);
  symbols @5 :List(SymbolEntry);
  sourcePackages @6 :List(SourcePackageRef);
  environments @7 :List(MaterializedEnvironment);
  contracts @8 :List(ExecutionContract);
  targets @9 :List(TargetPlan);
  datastoreManifests @10 :List(ArtifactRef);
  provenance @11 :CompilerProvenance;
}

struct GraphIR {
  workflowName @0 :Text;
  inputSchema @1 :HashRef;
  outputSchema @2 :HashRef;
  startNode @3 :Text;
  endNode @4 :Text;
  nodes @5 :List(GraphNode);
  edges @6 :List(GraphEdge);
}

struct GraphNode {
  id @0 :Text;

  union {
    start @1 :Void;
    end @2 :Void;
    step @3 :StepNode;
  }
}

struct StepNode {
  inputSchema @0 :HashRef;
  outputSchema @1 :HashRef;
  symbolRef @2 :Text;
  contractRef @3 :HashRef;
  mergeInputs @4 :List(Text);
}

struct GraphEdge {
  from @0 :Text;
  to @1 :Text;
}

struct SchemaEntry {
  hash @0 :HashRef;
  canonicalJson @1 :Text;
}

struct SymbolEntry {
  symbolRef @0 :Text;
  packageId @1 :Text;
  language @2 :Language;
  module @3 :Text;
  export @4 :Text;
}

enum Language {
  unknown @0;
  typescript @1;
  python @2;
}

struct SourcePackageRef {
  packageId @0 :Text;
  language @1 :Language;
  packageHash @2 :HashRef;
  manifest @3 :ArtifactRef;
  sourceArchive @4 :ArtifactRef;
}

struct MaterializedEnvironment {
  envRef @0 :HashRef;
  specHash @1 :HashRef;

  union {
    skipped @2 :Void;
    local @3 :ArtifactRef;
    container @4 :ContainerRuntime;
  }
}

struct ContainerRuntime {
  image @0 :Text;
  sourceFetch @1 :ArtifactRef;
}

struct ExecutionContract {
  contractRef @0 :HashRef;
  environmentRef @1 :HashRef;
  resources @2 :ResourceRequirements;
  secrets @3 :List(SecretRef);
  network @4 :NetworkPolicy;
}

struct ResourceRequirements {
  cpu @0 :Text;
  memory @1 :Text;
}

struct SecretRef {
  name @0 :Text;
  ref @1 :Text;
}

struct NetworkPolicy {
  egress @0 :EgressMode;
}

enum EgressMode {
  none @0;
  declared @1;
  any @2;
}

struct TargetPlan {
  kind @0 :TargetKind;
  targetHash @1 :HashRef;
  bundleManifest @2 :ArtifactRef;
}

enum TargetKind {
  unknown @0;
  local @1;
  argo @2;
}

struct CompilerProvenance {
  compilerName @0 :Text;
  compilerVersion @1 :Text;
  sourceSpecHash @2 :HashRef;
}
