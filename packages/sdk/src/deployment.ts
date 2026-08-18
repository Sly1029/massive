import { sha256RefText, stableStringify } from "./stable.ts";

export type DeploymentTarget =
  | { readonly kind: "local" }
  | {
    readonly kind: "argo";
    readonly namespace: string;
    readonly serviceAccountName: string;
    readonly workflowTemplateName?: string;
  };

export interface DeploymentProfile {
  readonly name: string;
  readonly artifactStoreBinding: string;
  readonly target: DeploymentTarget;
}

export interface DeploymentSpec {
  readonly kind: "DeploymentSpec";
  readonly schemaVersion: 0;
  readonly encoding: "json-v0";
  readonly deploymentHash: string;
  readonly planHash: string;
  readonly profile: DeploymentProfile;
}

export const deployment = {
  local(
    profile: Partial<Omit<DeploymentProfile, "target">> = {},
  ): DeploymentProfile {
    return {
      name: profile.name ?? "local",
      artifactStoreBinding: profile.artifactStoreBinding ?? "local-artifacts",
      target: { kind: "local" },
    };
  },

  argo(
    profile:
      & Omit<
        DeploymentProfile,
        "target"
      >
      & Omit<Extract<DeploymentTarget, { readonly kind: "argo" }>, "kind">,
  ): DeploymentProfile {
    return {
      name: profile.name,
      artifactStoreBinding: profile.artifactStoreBinding,
      target: {
        kind: "argo",
        namespace: profile.namespace,
        serviceAccountName: profile.serviceAccountName,
        ...(profile.workflowTemplateName === undefined
          ? {}
          : { workflowTemplateName: profile.workflowTemplateName }),
      },
    };
  },
};

export function emitDeploymentSpec(
  planHash: string,
  profile: DeploymentProfile,
): DeploymentSpec {
  const specWithoutHash = {
    kind: "DeploymentSpec" as const,
    schemaVersion: 0 as const,
    encoding: "json-v0" as const,
    planHash,
    profile,
  };
  return {
    ...specWithoutHash,
    deploymentHash: computeDeploymentHash(specWithoutHash),
  };
}

export function computeDeploymentHash(
  spec: Omit<DeploymentSpec, "deploymentHash"> & {
    readonly deploymentHash?: string;
  },
): string {
  const { deploymentHash: _omit, ...rest } = spec;
  return sha256RefText(stableStringify(rest));
}
