export {
  defineWorkflowPackage,
  target,
  type WorkflowPackageConfig,
  type WorkflowSpecTarget,
} from "./config.ts";
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
  emitWorkflowSpec,
  type EmitWorkflowSpecOptions,
  type WorkflowSpec,
} from "./emit.ts";
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
