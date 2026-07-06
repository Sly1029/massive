import type { EnvironmentSpec } from "./contract.ts";

export type WorkflowSpecTarget =
  | { readonly kind: "local" }
  | {
    readonly kind: "argo";
    readonly namespace: string;
    readonly serviceAccountName: string;
    readonly workflowTemplateName?: string;
  };

export interface WorkflowPackageConfig {
  readonly projectId?: string;
  readonly include: readonly string[];
  readonly entrypoint: string;
  readonly environment?: EnvironmentSpec;
  readonly targets?: readonly WorkflowSpecTarget[];
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
    ...(config.targets === undefined ? {} : { targets: [...config.targets] }),
  };
}

export const target = {
  local(): WorkflowSpecTarget {
    return { kind: "local" };
  },

  argo(
    spec: Omit<Extract<WorkflowSpecTarget, { readonly kind: "argo" }>, "kind">,
  ): WorkflowSpecTarget {
    return {
      kind: "argo",
      namespace: spec.namespace,
      serviceAccountName: spec.serviceAccountName,
      ...(spec.workflowTemplateName === undefined
        ? {}
        : { workflowTemplateName: spec.workflowTemplateName }),
    };
  },
};
