import { DirectedGraph } from "graphology";
import type { z } from "zod";
import type { ContractSpec, ExecutionContract } from "./contract.ts";
import { GraphValidationError } from "./errors.ts";
import type { AnySchema } from "./schema.ts";

export const START_NODE = "__start";
export const END_NODE = "__end";
// Graph IR node ids (conformance/schema/workflow-spec.schema.json).
export const NODE_ID_PATTERN = /^[A-Za-z0-9_.@:#-]{1,128}$/;

export type StepRun<Input, Output> = (context: {
  readonly input: Input;
  readonly context: {
    readonly runId: string;
    readonly stepId: string;
    readonly attempt: number;
    readonly maxAttempts: number;
    readonly idempotencyKey: string;
  };
}) => Output | Promise<Output>;

export interface StepSpec<InputSchema extends AnySchema, OutputSchema extends AnySchema> {
  readonly input: InputSchema;
  readonly output: OutputSchema;
  readonly run: StepRun<z.infer<InputSchema>, z.infer<OutputSchema>>;
  readonly contract?: ContractSpec | ExecutionContract;
  // The entrypoint export holding `run`; defaults to the step id. Set it to give
  // one exported function several node ids.
  readonly export?: string;
}

// A transform's input schema is the schema of the value on its path.
export type TransformSpec<Input, OutputSchema extends AnySchema> =
  & Omit<StepSpec<AnySchema, OutputSchema>, "input" | "run">
  & { readonly run: StepRun<Input, z.infer<OutputSchema>> };

export interface WorkflowConfig<InputSchema extends AnySchema, OutputSchema extends AnySchema> {
  readonly name: string;
  readonly input: InputSchema;
  readonly output: OutputSchema;
  readonly defaults?: ContractSpec | ExecutionContract;
}

export interface StepNode {
  readonly id: string;
  readonly kind: "step";
  readonly exportName: string;
  readonly input: AnySchema;
  readonly output: AnySchema;
  // Only resolved by export and compared by identity, so any step run fits.
  readonly run: StepRun<never, unknown>;
  readonly symbolRef: string;
  readonly contract?: ContractSpec | ExecutionContract;
  mergeInputs?: string[];
}

// A child workflow expanded into scoped steps when the parent is emitted.
export interface CallNode {
  readonly id: string;
  readonly kind: "call";
  readonly child: WorkflowBuilder<unknown, never>;
  mergeInputs?: string[];
}

// Handles consume values, so their input slot is contravariant: an edge accepts
// a producer whose output is assignable to the consumer's input, never a wider one.
export class StepHandle<Input, Output> {
  readonly __input?: (value: Input) => void;
  readonly __output?: Output;

  constructor(readonly nodeId: string) {}
}

export class EndHandle<Output> {
  readonly __input?: (value: Output) => void;

  constructor(readonly nodeId: string) {}
}

export class PathBuilder<Current> {
  constructor(
    private readonly builder: WorkflowBuilder<unknown, never>,
    private readonly currentNodeId: string
  ) {}

  to<Next>(next: StepHandle<Current, Next>): PathBuilder<Next>;
  to(next: EndHandle<Current>): void;
  to<Next>(next: StepHandle<Current, Next> | EndHandle<Current>): PathBuilder<Next> | void {
    this.builder.addEdge(this.currentNodeId, next.nodeId);

    if (next instanceof EndHandle) {
      return;
    }

    return new PathBuilder<Next>(this.builder, next.nodeId);
  }

  // Sugar for an ordinary named step that consumes this path's value: the step's
  // input schema is the producer's output schema, and `run` must still be the
  // entrypoint export named by `spec.export ?? id`.
  transform<OutputSchema extends AnySchema>(
    id: string,
    spec: TransformSpec<Current, OutputSchema>
  ): PathBuilder<z.infer<OutputSchema>> {
    this.builder.addStep(id, {
      ...spec,
      input: this.builder.valueSchema(this.currentNodeId),
    });
    this.builder.addEdge(this.currentNodeId, id);
    return new PathBuilder<z.infer<OutputSchema>>(this.builder, id);
  }
}

export class MergeBuilder<Current> {
  constructor(
    private readonly builder: WorkflowBuilder<unknown, never>,
    private readonly sourceNodeIds: readonly string[]
  ) {}

  to<Next>(next: StepHandle<Current[], Next>): PathBuilder<Next> {
    this.builder.addMergeEdges(this.sourceNodeIds, next.nodeId);
    return new PathBuilder<Next>(this.builder, next.nodeId);
  }
}

export class WorkflowBuilder<Input, Output> {
  readonly graph = new DirectedGraph();
  readonly stepNodes = new Map<string, StepNode>();
  readonly callNodes = new Map<string, CallNode>();
  readonly runtimeRegistry = new Map<string, StepRun<never, unknown>>();

  constructor(
    readonly name: string,
    readonly input: AnySchema,
    readonly output: AnySchema,
    readonly defaults: ContractSpec | ExecutionContract | undefined
  ) {
    this.graph.addNode(START_NODE, { kind: "start" });
    this.graph.addNode(END_NODE, { kind: "end" });
  }

  step<InputSchema extends AnySchema, OutputSchema extends AnySchema>(
    id: string,
    spec: StepSpec<InputSchema, OutputSchema>
  ): StepHandle<z.infer<InputSchema>, z.infer<OutputSchema>> {
    this.addStep(id, spec);
    return new StepHandle<z.infer<InputSchema>, z.infer<OutputSchema>>(id);
  }

  addStep(
    id: string,
    spec: Omit<StepSpec<AnySchema, AnySchema>, "run"> & { readonly run: StepRun<never, unknown> }
  ): void {
    if (id === START_NODE || id === END_NODE || this.graph.hasNode(id)) {
      throw new GraphValidationError(`Duplicate or reserved step id "${id}"`);
    }

    const symbolRef = `${this.name}/${id}`;
    const node: StepNode = {
      id,
      kind: "step",
      exportName: spec.export ?? id,
      input: spec.input,
      output: spec.output,
      run: spec.run,
      symbolRef,
      ...(spec.contract === undefined ? {} : { contract: spec.contract }),
    };

    this.stepNodes.set(id, node);
    this.runtimeRegistry.set(symbolRef, node.run);
    this.graph.addNode(id, { kind: "step" });
  }

  // Reuse a child workflow as one node. Emission expands it into the child's
  // steps under "<id>--<child step id>", so the IR has no call nodes.
  call<ChildInput, ChildOutput>(
    id: string,
    child: WorkflowBuilder<ChildInput, ChildOutput>
  ): StepHandle<ChildInput, ChildOutput> {
    if (id === START_NODE || id === END_NODE || this.graph.hasNode(id)) {
      throw new GraphValidationError(`Duplicate or reserved call id "${id}"`);
    }
    if (!NODE_ID_PATTERN.test(id)) {
      throw new GraphValidationError(
        `Call id "${id}" must be 1-128 characters of A-Z, a-z, 0-9, and _.@:#-`
      );
    }

    this.callNodes.set(id, { id, kind: "call", child });
    this.graph.addNode(id, { kind: "call" });
    return new StepHandle<ChildInput, ChildOutput>(id);
  }

  // The runtime schema of the value a node produces.
  valueSchema(nodeId: string): AnySchema {
    if (nodeId === START_NODE) return this.input;
    const step = this.stepNodes.get(nodeId);
    if (step !== undefined) return step.output;
    const call = this.callNodes.get(nodeId);
    if (call !== undefined) return call.child.output;
    throw new GraphValidationError(`Node "${nodeId}" does not produce a value`);
  }

  start(): PathBuilder<Input> {
    return new PathBuilder<Input>(this, START_NODE);
  }

  from<StepInput, StepOutput>(step: StepHandle<StepInput, StepOutput>): PathBuilder<StepOutput> {
    return new PathBuilder<StepOutput>(this, step.nodeId);
  }

  merge<StepOutput>(steps: readonly StepHandle<never, StepOutput>[]): MergeBuilder<StepOutput> {
    if (steps.length === 0) {
      throw new GraphValidationError("Merge requires at least one upstream step");
    }
    return new MergeBuilder<StepOutput>(
      this,
      steps.map((step) => step.nodeId)
    );
  }

  end(): EndHandle<Output> {
    return new EndHandle<Output>(END_NODE);
  }

  addEdge(from: string, to: string): void {
    if (!this.graph.hasNode(from)) {
      throw new GraphValidationError(`Unknown source node "${from}"`);
    }
    if (!this.graph.hasNode(to)) {
      throw new GraphValidationError(`Unknown target node "${to}"`);
    }
    this.graph.mergeDirectedEdge(from, to);
  }

  addMergeEdges(from: readonly string[], to: string): void {
    const target = this.stepNodes.get(to) ?? this.callNodes.get(to);
    if (target === undefined) {
      throw new GraphValidationError(`Merge target "${to}" must be a step or call`);
    }
    if (target.mergeInputs !== undefined) {
      throw new GraphValidationError(`Node "${to}" already has merge inputs`);
    }

    target.mergeInputs = [...from];
    for (const source of from) {
      this.addEdge(source, to);
    }
  }

  freeze(): void {
    Object.freeze(this);
  }
}

export function workflow<InputSchema extends AnySchema, OutputSchema extends AnySchema>(
  config: WorkflowConfig<InputSchema, OutputSchema>
): WorkflowBuilder<z.infer<InputSchema>, z.infer<OutputSchema>> {
  return new WorkflowBuilder<z.infer<InputSchema>, z.infer<OutputSchema>>(
    config.name,
    config.input,
    config.output,
    config.defaults
  );
}
