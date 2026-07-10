import { compareCodeUnits } from "./stable.ts";

export type PackageManager = "npm" | "pnpm" | "yarn";

export type EnvironmentSpec = ContainerEnvironmentSpec | NodeEnvironmentSpec;

export interface ContainerEnvironmentSpec {
  readonly kind: "container";
  readonly image: string;
  readonly command?: readonly string[];
  readonly workingDirectory?: string;
}

export interface NodeEnvironmentSpec {
  readonly kind: "node";
  readonly version: string;
  readonly packageManager: PackageManager;
  readonly lockfile: string;
}

export interface ResourceSpec {
  readonly cpu?: string;
  readonly memory?: string;
}

export interface SecretRef {
  readonly name: string;
  readonly ref: string;
}

export type NetworkSpec =
  | { readonly egress: "none" }
  | { readonly egress: "declared"; readonly hosts: readonly string[] }
  | { readonly egress: "any" };

export interface ContractSpec {
  readonly env?: EnvironmentSpec;
  readonly resources?: ResourceSpec;
  readonly secrets?: readonly SecretRef[];
  readonly network?: NetworkSpec;
}

export class ExecutionContract {
  constructor(readonly spec: ContractSpec) {}

  extend(overrides: ContractSpec | ExecutionContract): ExecutionContract {
    return new ExecutionContract(
      mergeContractSpecs(this.spec, contractSpecOf(overrides)),
    );
  }
}

export const env = {
  container(
    spec: Omit<ContainerEnvironmentSpec, "kind">,
  ): ContainerEnvironmentSpec {
    return {
      kind: "container",
      image: spec.image,
      ...(spec.command === undefined ? {} : { command: [...spec.command] }),
      ...(spec.workingDirectory === undefined
        ? {}
        : { workingDirectory: spec.workingDirectory }),
    };
  },

  node(spec: Omit<NodeEnvironmentSpec, "kind">): NodeEnvironmentSpec {
    return {
      kind: "node",
      version: spec.version,
      packageManager: spec.packageManager,
      lockfile: spec.lockfile,
    };
  },
};

export const net = {
  denyAll(): NetworkSpec {
    return { egress: "none" };
  },

  allow(host: string | readonly string[]): NetworkSpec {
    const hosts = [...new Set(typeof host === "string" ? [host] : host)]
      .sort(compareCodeUnits);
    if (hosts.length === 0) {
      throw new Error(
        "net.allow() requires at least one host; use net.denyAll() for no egress",
      );
    }
    return { egress: "declared", hosts };
  },

  allowAny(): NetworkSpec {
    return { egress: "any" };
  },
};

export const secret = {
  ref(name: string, ref: string = name): SecretRef {
    return { name, ref };
  },
};

export function contract(spec: ContractSpec): ExecutionContract {
  return new ExecutionContract(spec);
}

export function contractSpecOf(
  spec: ContractSpec | ExecutionContract | undefined,
): ContractSpec {
  if (spec === undefined) {
    return {};
  }
  return spec instanceof ExecutionContract ? spec.spec : spec;
}

export function mergeContractSpecs(
  base: ContractSpec,
  overrides: ContractSpec,
): ContractSpec {
  const environment = overrides.env ?? base.env;
  const network = overrides.network ?? base.network;
  const resources =
    base.resources === undefined && overrides.resources === undefined
      ? undefined
      : { ...(base.resources ?? {}), ...(overrides.resources ?? {}) };
  const secrets = mergeSecrets(base.secrets ?? [], overrides.secrets ?? []);

  return {
    ...(environment === undefined ? {} : { env: environment }),
    ...(resources === undefined || Object.keys(resources).length === 0
      ? {}
      : { resources }),
    ...(secrets.length === 0 ? {} : { secrets }),
    ...(network === undefined ? {} : { network }),
  };
}

function mergeSecrets(
  base: readonly SecretRef[],
  overrides: readonly SecretRef[],
): SecretRef[] {
  const byName = new Map<string, SecretRef>();
  for (const entry of base) {
    byName.set(entry.name, entry);
  }
  for (const entry of overrides) {
    byName.set(entry.name, entry);
  }
  return [...byName.values()].sort((left, right) =>
    compareCodeUnits(`${left.name}\0${left.ref}`, `${right.name}\0${right.ref}`)
  );
}
