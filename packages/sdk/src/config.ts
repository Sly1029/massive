import type { EnvironmentSpec } from "./contract.ts";
import type { DeploymentProfile } from "./deployment.ts";

export interface WorkflowPackageConfig {
  readonly projectId?: string;
  readonly include: readonly string[];
  readonly entrypoint: string;
  readonly environment?: EnvironmentSpec;
  // Deployment profiles are authored with a package for convenience, but are
  // intentionally lowered as DeploymentSpec artifacts after plan compilation.
  readonly deploymentProfiles?: readonly DeploymentProfile[];
}

export function defineWorkflowPackage(
  config: WorkflowPackageConfig,
): WorkflowPackageConfig {
  return {
    ...(config.projectId === undefined ? {} : { projectId: config.projectId }),
    include: [...config.include],
    entrypoint: config.entrypoint,
    ...(config.environment === undefined
      ? {}
      : { environment: config.environment }),
    ...(config.deploymentProfiles === undefined
      ? {}
      : { deploymentProfiles: [...config.deploymentProfiles] }),
  };
}
