import fg from "fast-glob";
import { readFile } from "node:fs/promises";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { GraphValidationError, MassiveError } from "./errors.ts";
import type { Datastore } from "./datastore.ts";
import { WorkflowPlanJsonV0Schema, type WorkflowPlanJsonV0 } from "./plan.ts";
import { lowerPortableSchema, type AnySchema, type LoweredSchema } from "./schema.ts";
import { sha256Bytes, sha256Text, stableStringify, type JsonValue } from "./stable.ts";
import { END_NODE, START_NODE, type StepNode, type WorkflowBuilder } from "./workflow.ts";

export interface SourceSpec {
  readonly root: string;
  readonly include: readonly string[];
}

export interface CompileOptions {
  readonly target: "local";
  readonly datastore: Datastore;
  readonly source: SourceSpec;
}

export interface CompiledWorkflow<Output = unknown> {
  readonly planHash: string;
  readonly plan: WorkflowPlanJsonV0;
  readonly datastore: Datastore;
  readonly runtimeRegistry: ReadonlyMap<string, StepNode["run"]>;
  readonly runtimeSchemas: ReadonlyMap<string, AnySchema>;
  readonly __output?: Output;
}

interface SourcePackage {
  readonly root: string;
  readonly include: string[];
  readonly files: { readonly path: string; readonly hash: string }[];
  readonly sourcePackageHash: string;
}

export async function compile<Input, Output>(
  builder: WorkflowBuilder<Input, Output>,
  options: CompileOptions
): Promise<CompiledWorkflow<Output>> {
  builder.freeze();
  validateGraphShape(builder);

  const source = await hashSourcePackage(options.source);
  const schemas = new Map<string, JsonValue>();
  const runtimeSchemas = new Map<string, AnySchema>();

  const workflowInput = registerSchema(schemas, runtimeSchemas, builder.input, `${builder.name}.input`);
  const workflowOutput = registerSchema(schemas, runtimeSchemas, builder.output, `${builder.name}.output`);

  const stepSchemas = new Map<string, { input: LoweredSchema; output: LoweredSchema }>();
  for (const step of sortedSteps(builder)) {
    stepSchemas.set(step.id, {
      input: registerSchema(schemas, runtimeSchemas, step.input, `${builder.name}.${step.id}.input`),
      output: registerSchema(schemas, runtimeSchemas, step.output, `${builder.name}.${step.id}.output`),
    });
  }

  const channelSchemas = new Map<string, LoweredSchema>();
  for (const [name, definition] of Object.entries(builder.channels).sort(([left], [right]) => left.localeCompare(right))) {
    channelSchemas.set(name, registerSchema(schemas, runtimeSchemas, definition.schema, `${builder.name}.channel.${name}`));
  }

  const symbolSteps = sortedSteps(builder).map((step) => ({
    stepId: step.id,
    name: step.symbolRef,
    sourcePackageHash: source.sourcePackageHash,
  }));
  const symbolManifestHash = sha256Text(stableStringify(symbolSteps));

  const planWithoutHash: WorkflowPlanJsonV0 = {
    schemaVersion: 0,
    encoding: "json-v0",
    target: options.target,
    workflow: {
      name: builder.name,
      inputSchema: workflowInput.hash,
      outputSchema: workflowOutput.hash,
    },
    source,
    symbols: {
      symbolManifestHash,
      steps: symbolSteps,
    },
    schemas: Object.fromEntries([...schemas.entries()].sort(([left], [right]) => left.localeCompare(right))),
    graph: {
      start: START_NODE,
      end: END_NODE,
      nodes: lowerNodes(builder, stepSchemas),
      edges: lowerEdges(builder),
    },
    channels: Object.fromEntries(
      [...channelSchemas.entries()].map(([name, schema]) => [name, { schema: schema.hash, reducer: "last" as const }])
    ),
  };

  WorkflowPlanJsonV0Schema.parse(planWithoutHash);

  const planHash = sha256Text(stableStringify(planWithoutHash));
  const plan = { ...planWithoutHash, planHash };
  WorkflowPlanJsonV0Schema.parse(plan);

  await options.datastore.put(`packages/${source.sourcePackageHash}/manifest.json`, stableStringify(source));
  await options.datastore.put(`plans/${planHash}/workflow.json`, stableStringify(plan));
  await options.datastore.put(
    `plans/${planHash}/manifest.json`,
    stableStringify({
      planHash,
      encoding: "json-v0",
      workflow: builder.name,
      sourcePackageHash: source.sourcePackageHash,
      symbolManifestHash,
    })
  );

  return {
    planHash,
    plan,
    datastore: options.datastore,
    runtimeRegistry: new Map(builder.runtimeRegistry),
    runtimeSchemas,
  };
}

function registerSchema(
  schemas: Map<string, JsonValue>,
  runtimeSchemas: Map<string, AnySchema>,
  schema: AnySchema,
  role: string
): LoweredSchema {
  const lowered = lowerPortableSchema(schema, role);
  schemas.set(lowered.hash, lowered.jsonSchema);
  runtimeSchemas.set(lowered.hash, schema);
  return lowered;
}

async function hashSourcePackage(source: SourceSpec): Promise<SourcePackage> {
  if (source.include.length === 0) {
    throw new MassiveError("compile source.include must contain at least one pattern");
  }

  const root = resolve(source.root);
  const files = await fg([...source.include], {
    cwd: root,
    dot: true,
    followSymbolicLinks: false,
    onlyFiles: true,
    unique: true,
  });

  const entries: { path: string; hash: string }[] = [];
  for (const file of files.sort()) {
    const absolute = resolve(root, file);
    const backToRoot = relative(root, absolute);
    if (backToRoot === "" || backToRoot.startsWith(`..${sep}`) || isAbsolute(backToRoot)) {
      throw new MassiveError(`compile source include resolved outside root: ${file}`);
    }

    entries.push({
      path: normalizeObjectPath(backToRoot),
      hash: sha256Bytes(await readFile(absolute)),
    });
  }

  const sourcePackageHash = sha256Text(stableStringify(entries));
  return {
    root,
    include: [...source.include],
    files: entries,
    sourcePackageHash,
  };
}

function validateGraphShape(builder: WorkflowBuilder<unknown, unknown>): void {
  const graph = builder.graph;

  if (!graph.hasNode(START_NODE) || !graph.hasNode(END_NODE)) {
    throw new GraphValidationError("Workflow graph must contain exactly one start and one end");
  }

  assertAcyclic(builder);

  const reachableFromStart = traverse(builder, START_NODE, "outbound");
  for (const step of builder.stepNodes.keys()) {
    if (!reachableFromStart.has(step)) {
      throw new GraphValidationError(`Step "${step}" is not reachable from start`);
    }
  }

  const canReachEnd = traverse(builder, END_NODE, "inbound");
  for (const step of builder.stepNodes.keys()) {
    if (!canReachEnd.has(step)) {
      throw new GraphValidationError(`Step "${step}" cannot reach end`);
    }
  }

  if (!reachableFromStart.has(END_NODE)) {
    throw new GraphValidationError("End is not reachable from start");
  }
}

function assertAcyclic(builder: WorkflowBuilder<unknown, unknown>): void {
  const graph = builder.graph;
  const visiting = new Set<string>();
  const visited = new Set<string>();

  const visit = (node: string): void => {
    if (visited.has(node)) return;
    if (visiting.has(node)) {
      throw new GraphValidationError(`Workflow graph contains a cycle at "${node}"`);
    }

    visiting.add(node);
    for (const neighbor of graph.outboundNeighbors(node) as string[]) {
      visit(neighbor);
    }
    visiting.delete(node);
    visited.add(node);
  };

  for (const node of graph.nodes() as string[]) {
    visit(node);
  }
}

function traverse(builder: WorkflowBuilder<unknown, unknown>, start: string, direction: "inbound" | "outbound"): Set<string> {
  const graph = builder.graph;
  const seen = new Set<string>();
  const queue = [start];

  while (queue.length > 0) {
    const node = queue.shift();
    if (node === undefined || seen.has(node)) continue;
    seen.add(node);

    const next = direction === "outbound" ? graph.outboundNeighbors(node) : graph.inboundNeighbors(node);
    queue.push(...(next as string[]));
  }

  return seen;
}

function sortedSteps(builder: WorkflowBuilder<unknown, unknown>): StepNode[] {
  return [...builder.stepNodes.values()].sort((left, right) => left.id.localeCompare(right.id));
}

function lowerNodes(
  builder: WorkflowBuilder<unknown, unknown>,
  stepSchemas: Map<string, { input: LoweredSchema; output: LoweredSchema }>
): WorkflowPlanJsonV0["graph"]["nodes"] {
  const steps = sortedSteps(builder).map((step) => {
    const schemas = stepSchemas.get(step.id);
    if (schemas === undefined) {
      throw new GraphValidationError(`Missing schema refs for step "${step.id}"`);
    }

    return {
      id: step.id,
      kind: "step" as const,
      inputSchema: schemas.input.hash,
      outputSchema: schemas.output.hash,
      symbolRef: step.symbolRef,
      ...(step.channel === undefined ? {} : { channel: step.channel }),
      ...(step.mergeInputs === undefined ? {} : { mergeInputs: step.mergeInputs }),
      ...(step.publish === undefined ? {} : { publish: step.publish }),
    };
  });

  return [{ id: START_NODE, kind: "start" as const }, ...steps, { id: END_NODE, kind: "end" as const }];
}

function lowerEdges(builder: WorkflowBuilder<unknown, unknown>): WorkflowPlanJsonV0["graph"]["edges"] {
  const edges: WorkflowPlanJsonV0["graph"]["edges"] = [];
  builder.graph.forEachDirectedEdge((edge, _attributes, source, target) => {
    edges.push({ from: String(source), to: String(target) });
  });
  return edges.sort((left, right) => `${left.from}\0${left.to}`.localeCompare(`${right.from}\0${right.to}`));
}

function normalizeObjectPath(path: string): string {
  return path.split(sep).join("/");
}
