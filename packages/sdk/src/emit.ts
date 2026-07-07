import {
  contract,
  type ContractSpec,
  contractSpecOf,
  env,
  type EnvironmentSpec,
  mergeContractSpecs,
  net,
  type NetworkSpec,
  type ResourceSpec,
  type SecretRef,
} from "./contract.ts";
import type { WorkflowPackageConfig, WorkflowSpecTarget } from "./config.ts";
import { GraphValidationError } from "./errors.ts";
import { validateGraphShape } from "./graph-validate.ts";
import { lowerPortableSchema } from "./schema.ts";
import { hashSourcePackage, type SourceSpec } from "./source-package.ts";
import {
  compareCodeUnits,
  type JsonValue,
  sha256RefText,
  stableStringify,
} from "./stable.ts";
import {
  END_NODE,
  START_NODE,
  type StepNode,
  type WorkflowBuilder,
} from "./workflow.ts";

export interface EmitSourceSpec extends SourceSpec {
  readonly packageId?: string;
  readonly module?: string;
}

export interface EmitWorkflowSpecOptions {
  readonly source?: EmitSourceSpec;
  readonly package?: WorkflowPackageConfig;
  readonly packageRoot?: string;
  readonly targets?: readonly WorkflowSpecTarget[];
}

export interface WorkflowSpec {
  readonly kind: "WorkflowSpec";
  readonly schemaVersion: 0;
  readonly encoding: "json-v0";
  readonly specHash: string;
  readonly workflow: {
    readonly name: string;
    readonly inputSchema: string;
    readonly outputSchema: string;
  };
  readonly graph: {
    readonly start: string;
    readonly end: string;
    readonly nodes: readonly WorkflowSpecNode[];
    readonly edges: readonly WorkflowSpecEdge[];
  };
  readonly schemas: Readonly<Record<string, JsonValue>>;
  readonly symbols: Readonly<Record<string, WorkflowSpecSymbol>>;
  readonly sourcePackages: Readonly<Record<string, WorkflowSpecSourcePackage>>;
  readonly environments: Readonly<Record<string, WorkflowSpecEnvironment>>;
  readonly contracts: Readonly<Record<string, WorkflowSpecExecutionContract>>;
  readonly targets: readonly WorkflowSpecTarget[];
}

export type WorkflowSpecNode =
  | { readonly id: string; readonly kind: "start" }
  | { readonly id: string; readonly kind: "end" }
  | {
    readonly id: string;
    readonly kind: "step";
    readonly inputSchema: string;
    readonly outputSchema: string;
    readonly symbolRef: string;
    readonly contractRef: string;
    readonly mergeInputs?: readonly string[];
  };

export interface WorkflowSpecEdge {
  readonly from: string;
  readonly to: string;
}

export interface WorkflowSpecSymbol {
  readonly packageId: string;
  readonly language: "typescript";
  readonly module: string;
  readonly export: string;
}

export interface WorkflowSpecSourcePackage {
  readonly packageId: string;
  readonly language: "typescript";
  readonly packageHash: string;
  readonly root: string;
  readonly include: readonly string[];
  readonly files: readonly { readonly path: string; readonly hash: string }[];
  readonly artifact: string;
}

export type WorkflowSpecEnvironment = EnvironmentSpec;

export interface WorkflowSpecExecutionContract {
  readonly environmentRef: string;
  readonly resources?: ResourceSpec;
  readonly secrets?: readonly SecretRef[];
  readonly network?: NetworkSpec;
}

const DEFAULT_CONTRACT = contract({
  env: env.container({ image: "ghcr.io/massive-dev/typescript-runner:v0" }),
  network: net.denyAll(),
});

export async function emitWorkflowSpec<Input, Output>(
  builder: WorkflowBuilder<Input, Output>,
  options: EmitWorkflowSpecOptions,
): Promise<WorkflowSpec> {
  builder.freeze();
  validateGraphShape(builder as WorkflowBuilder<unknown, unknown>);

  const sourceOptions = emitSourceSpec(options);
  const packageId = sourceOptions.packageId ?? "ts-main";
  const module = sourceOptions.module ??
    moduleFromEntrypoint(options.package?.entrypoint ?? "./workflow.ts");
  const source = await hashSourcePackage(sourceOptions);
  const schemas = new Map<string, JsonValue>();

  const workflowInput = registerSchema(
    schemas,
    builder.input,
    `${builder.name}.input`,
  );
  const workflowOutput = registerSchema(
    schemas,
    builder.output,
    `${builder.name}.output`,
  );
  const stepSchemas = new Map<string, { input: string; output: string }>();
  for (const step of sortedSteps(builder)) {
    stepSchemas.set(step.id, {
      input: registerSchema(
        schemas,
        step.input,
        `${builder.name}.${step.id}.input`,
      ),
      output: registerSchema(
        schemas,
        step.output,
        `${builder.name}.${step.id}.output`,
      ),
    });
  }

  const environments = new Map<string, WorkflowSpecEnvironment>();
  const contracts = new Map<string, WorkflowSpecExecutionContract>();
  const workflowDefault = mergeContractSpecs(
    mergeContractSpecs(
      DEFAULT_CONTRACT.spec,
      options.package?.environment === undefined
        ? {}
        : { env: options.package.environment },
    ),
    contractSpecOf(builder.defaults),
  );
  registerContract(environments, contracts, workflowDefault);
  const stepContractRefs = new Map<string, string>();
  for (const step of sortedSteps(builder)) {
    stepContractRefs.set(
      step.id,
      registerContract(
        environments,
        contracts,
        mergeContractSpecs(workflowDefault, contractSpecOf(step.contract)),
      ),
    );
  }

  const symbols = new Map<string, WorkflowSpecSymbol>();
  for (const step of sortedSteps(builder)) {
    const symbolId = symbolRef(packageId, module, step.id);
    symbols.set(symbolId, {
      packageId,
      language: "typescript",
      module,
      export: step.id,
    });
  }

  const specWithoutHash = {
    kind: "WorkflowSpec" as const,
    schemaVersion: 0 as const,
    encoding: "json-v0" as const,
    workflow: {
      name: builder.name,
      inputSchema: workflowInput,
      outputSchema: workflowOutput,
    },
    graph: {
      start: START_NODE,
      end: END_NODE,
      nodes: lowerNodes(
        builder,
        stepSchemas,
        stepContractRefs,
        packageId,
        module,
      ),
      edges: lowerEdges(builder),
    },
    schemas: sortedRecord(schemas),
    symbols: sortedRecord(symbols),
    sourcePackages: {
      [packageId]: {
        packageId,
        language: "typescript" as const,
        packageHash: source.sourcePackageHash,
        root: source.root,
        include: source.include,
        files: source.files,
        artifact: `packages/${
          hashPathSegment(source.sourcePackageHash)
        }/source.tar.zst`,
      },
    },
    environments: sortedRecord(environments),
    contracts: sortedRecord(contracts),
    targets: [
      ...(options.targets ?? options.package?.targets ?? [{
        kind: "local" as const,
      }]),
    ],
  };

  return {
    ...specWithoutHash,
    specHash: sha256RefText(stableStringify(specWithoutHash)),
  };
}

function emitSourceSpec(options: EmitWorkflowSpecOptions): EmitSourceSpec {
  if (options.source !== undefined) {
    return options.source;
  }
  if (options.package === undefined) {
    throw new GraphValidationError(
      "emitWorkflowSpec requires either source or package options",
    );
  }

  return {
    root: options.packageRoot ?? process.cwd(),
    include: options.package.include,
    module: moduleFromEntrypoint(options.package.entrypoint),
  };
}

function moduleFromEntrypoint(entrypoint: string): string {
  const hashIndex = entrypoint.lastIndexOf("#");
  const modulePath = hashIndex === -1
    ? entrypoint
    : entrypoint.slice(0, hashIndex);
  return modulePath.startsWith(".") ? modulePath : `./${modulePath}`;
}

function registerSchema(
  schemas: Map<string, JsonValue>,
  schema: Parameters<typeof lowerPortableSchema>[0],
  role: string,
): string {
  const lowered = lowerPortableSchema(schema, role);
  const schemaRef = `sha256:${lowered.hash}`;
  schemas.set(schemaRef, lowered.jsonSchema);
  return schemaRef;
}

function registerContract(
  environments: Map<string, WorkflowSpecEnvironment>,
  contracts: Map<string, WorkflowSpecExecutionContract>,
  spec: ContractSpec,
): string {
  if (spec.env === undefined) {
    throw new GraphValidationError(
      "Execution contract is missing an environment",
    );
  }

  const environment = lowerEnvironment(spec.env);
  const environmentRef = sha256RefText(stableStringify(environment));
  environments.set(environmentRef, environment);

  const contractSpec: WorkflowSpecExecutionContract = {
    environmentRef,
    ...(spec.resources === undefined || Object.keys(spec.resources).length === 0
      ? {}
      : { resources: spec.resources }),
    ...(spec.secrets === undefined || spec.secrets.length === 0
      ? {}
      : { secrets: [...spec.secrets] }),
    ...(spec.network === undefined ? {} : { network: spec.network }),
  };
  const contractRef = sha256RefText(stableStringify(contractSpec));
  contracts.set(contractRef, contractSpec);
  return contractRef;
}

function lowerEnvironment(
  environment: EnvironmentSpec,
): WorkflowSpecEnvironment {
  if (environment.kind === "container") {
    return {
      kind: "container",
      image: environment.image,
      ...(environment.command === undefined
        ? {}
        : { command: [...environment.command] }),
      ...(environment.workingDirectory === undefined
        ? {}
        : { workingDirectory: environment.workingDirectory }),
    };
  }

  return {
    kind: "node",
    version: environment.version,
    packageManager: environment.packageManager,
    lockfile: environment.lockfile,
  };
}

function sortedSteps(builder: WorkflowBuilder<unknown, unknown>): StepNode[] {
  return [...builder.stepNodes.values()].sort((left, right) =>
    compareCodeUnits(left.id, right.id)
  );
}

function lowerNodes(
  builder: WorkflowBuilder<unknown, unknown>,
  stepSchemas: ReadonlyMap<
    string,
    { readonly input: string; readonly output: string }
  >,
  stepContractRefs: ReadonlyMap<string, string>,
  packageId: string,
  module: string,
): WorkflowSpec["graph"]["nodes"] {
  const steps = sortedSteps(builder).map((step) => {
    const schemas = stepSchemas.get(step.id);
    if (schemas === undefined) {
      throw new GraphValidationError(
        `Missing schema refs for step "${step.id}"`,
      );
    }
    const contractRef = stepContractRefs.get(step.id);
    if (contractRef === undefined) {
      throw new GraphValidationError(
        `Missing contract ref for step "${step.id}"`,
      );
    }

    return {
      id: step.id,
      kind: "step" as const,
      inputSchema: schemas.input,
      outputSchema: schemas.output,
      symbolRef: symbolRef(packageId, module, step.id),
      contractRef,
      ...(step.mergeInputs === undefined
        ? {}
        : { mergeInputs: step.mergeInputs }),
    };
  });

  return [{ id: START_NODE, kind: "start" }, ...steps, {
    id: END_NODE,
    kind: "end",
  }];
}

function lowerEdges(
  builder: WorkflowBuilder<unknown, unknown>,
): WorkflowSpec["graph"]["edges"] {
  const edges: WorkflowSpecEdge[] = [];
  builder.graph.forEachDirectedEdge((_edge, _attributes, source, target) => {
    edges.push({ from: source, to: target });
  });
  return edges.sort((left, right) =>
    compareCodeUnits(`${left.from}\0${left.to}`, `${right.from}\0${right.to}`)
  );
}

function sortedRecord<Value>(
  map: ReadonlyMap<string, Value>,
): Record<string, Value> {
  return Object.fromEntries(
    [...map.entries()].sort(([left], [right]) => compareCodeUnits(left, right)),
  );
}

function symbolRef(
  packageId: string,
  module: string,
  exportName: string,
): string {
  return `${packageId}:${module}#${exportName}`;
}

function hashPathSegment(hash: string): string {
  return hash.replace(":", "-");
}
