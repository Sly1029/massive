export { defineWorkflowPackage, type WorkflowPackageConfig } from "./config.ts";
export {
  type HashingSpec,
  SOURCE_PACKAGE_HASHING,
  WORKFLOW_SPEC_HASHING,
} from "./hashing.ts";
export {
  computeDeploymentHash,
  deployment,
  type DeploymentProfile,
  type DeploymentSpec,
  type DeploymentTarget,
  emitDeploymentSpec,
} from "./deployment.ts";
export {
  contract,
  type ContractSpec,
  env,
  type EnvironmentSpec,
  type ExecutionContract,
  net,
  secret,
} from "./contract.ts";
// Import the datastore facade directly (not ./datastore/index.ts) so that a
// workflow module importing "@massive/sdk" — and therefore the step runner that
// imports that module — never pulls the S3 client's @aws-sdk module graph,
// which reads environment variables at load and would crash under the runner's
// scoped (no --allow-env) permissions. S3 access remains a deep import.
export { type Datastore, datastore } from "./datastore/facade.ts";
export {
  computeSpecHash,
  emitWorkflowSpec,
  type EmitWorkflowSpecOptions,
  type WorkflowSpec,
  type WorkflowSpecLanguage,
} from "./emit.ts";
export {
  parseWorkflowSpec,
  parseWorkflowSpecText,
  WorkflowSpecError,
} from "./workflow-spec.ts";
export {
  channel,
  type ChannelDefinition,
  type EndHandle,
  type MergeBuilder,
  type PathBuilder,
  type StateSchema,
  stateSchema,
  type StepHandle,
  type StepNode,
  type StepRun,
  type StepSpec,
  workflow,
  type WorkflowBuilder,
  type WorkflowConfig,
} from "./workflow.ts";
export {
  parseWorkflowPackageConfig,
  type ResolvedWorkflowEntrypoint,
  resolveWorkflowEntrypoint,
  type ResolveWorkflowEntrypointOptions,
} from "./resolve.ts";
export {
  DatastoreKeyError,
  GraphValidationError,
  MassiveError,
  SchemaPortabilityError,
  SourcePackagePathError,
} from "./errors.ts";
