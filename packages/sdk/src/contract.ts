import { compareCodeUnits } from "./stable.ts";

const immutableImage = /^[^@\s]+@sha256:[0-9a-f]{64}$/;
const containerPlatform = /^[a-z0-9][a-z0-9._-]*\/[a-z0-9][a-z0-9._-]*$/;

export type PackageManager = "npm" | "pnpm" | "yarn";

export type EnvironmentSpec = ContainerEnvironmentSpec | NodeEnvironmentSpec;

export interface ContainerEnvironmentSpec {
  readonly kind: "container";
  readonly image: string;
  readonly platform?: string;
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

export interface RetrySpec {
  readonly maxAttempts: number;
  readonly delaySeconds: number;
  readonly backoffFactor: number;
  readonly maxDelaySeconds: number;
}

export interface ContractSpec {
  readonly env?: EnvironmentSpec;
  readonly resources?: ResourceSpec;
  readonly secrets?: readonly SecretRef[];
  readonly network?: NetworkSpec;
  readonly retry?: RetrySpec;
  readonly timeoutSeconds?: number;
}

export class ExecutionContract {
  constructor(readonly spec: ContractSpec) {
    validateExecutionPolicy(spec);
  }

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
    const platform = spec.platform ?? "linux/amd64";
    if (!immutableImage.test(spec.image)) {
      throw new Error(
        "env.container() image must be an immutable sha256 digest reference",
      );
    }
    if (!containerPlatform.test(platform)) {
      throw new Error("env.container() platform must be an os/architecture pair");
    }
    if (spec.command?.some((value) => value.length === 0)) {
      throw new Error("env.container() command values must be non-empty");
    }
    if (spec.workingDirectory === "") {
      throw new Error("env.container() workingDirectory must not be empty");
    }
    return {
      kind: "container",
      image: spec.image,
      platform,
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

// Attempt n >= 2 waits min(delaySeconds * backoffFactor^(n - 2),
// maxDelaySeconds). attempts: 1 disables retries.
export function retry(options: {
  readonly attempts: number;
  readonly delaySeconds?: number;
  readonly backoffFactor?: number;
  readonly maxDelaySeconds?: number;
}): RetrySpec {
  const spec = {
    maxAttempts: options.attempts,
    delaySeconds: options.delaySeconds ?? 10,
    backoffFactor: options.backoffFactor ?? 2,
    maxDelaySeconds: options.maxDelaySeconds ?? 600,
  };
  validateRetrySpec(spec, "retry()");
  return spec;
}

export function contract(spec: ContractSpec): ExecutionContract {
  return new ExecutionContract(spec);
}

// Plain ContractSpec objects bypass the factories, so emission re-checks the
// effective policy against the WorkflowSpec bounds.
export function validateExecutionPolicy(spec: ContractSpec): void {
  if (spec.retry !== undefined) {
    validateRetrySpec(spec.retry, "contract retry");
  }
  if (spec.timeoutSeconds !== undefined) {
    requireIntegerInRange(
      "contract timeoutSeconds",
      spec.timeoutSeconds,
      1,
      604_800,
    );
  }
}

function validateRetrySpec(spec: RetrySpec, label: string): void {
  requireIntegerInRange(`${label} attempts`, spec.maxAttempts, 1, 100);
  requireIntegerInRange(`${label} delaySeconds`, spec.delaySeconds, 0, 86_400);
  requireIntegerInRange(`${label} backoffFactor`, spec.backoffFactor, 1, 10);
  requireIntegerInRange(
    `${label} maxDelaySeconds`,
    spec.maxDelaySeconds,
    0,
    86_400,
  );
  if (spec.maxDelaySeconds < spec.delaySeconds) {
    throw new Error(
      `${label} maxDelaySeconds (${spec.maxDelaySeconds}) must be at least delaySeconds (${spec.delaySeconds})`,
    );
  }
}

function requireIntegerInRange(
  label: string,
  value: number,
  minimum: number,
  maximum: number,
): void {
  if (!Number.isInteger(value) || value < minimum || value > maximum) {
    throw new Error(
      `${label} must be an integer from ${minimum} to ${maximum}, got ${value}`,
    );
  }
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
  // A retry policy is one unit: overriding it never inherits base fields.
  const retryPolicy = overrides.retry ?? base.retry;
  const timeoutSeconds = overrides.timeoutSeconds ?? base.timeoutSeconds;
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
    ...(retryPolicy === undefined ? {} : { retry: retryPolicy }),
    ...(timeoutSeconds === undefined ? {} : { timeoutSeconds }),
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
