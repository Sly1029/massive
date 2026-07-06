import { DirectedGraph } from "graphology";
import type { z } from "zod";
import type { ContractSpec, ExecutionContract } from "./contract.ts";
import { GraphValidationError } from "./errors.ts";
import type { AnySchema } from "./schema.ts";

export const START_NODE = "__start";
export const END_NODE = "__end";

export type StepRun<Input, Output> = (context: {
  readonly input: Input;
  readonly state: Record<string, unknown>;
  readonly context: {
    readonly runId: string;
    readonly stepId: string;
  };
}) => Output | Promise<Output>;

export interface StepSpec<InputSchema extends AnySchema, OutputSchema extends AnySchema> {
  readonly input: InputSchema;
  readonly output: OutputSchema;
  readonly run: StepRun<z.infer<InputSchema>, z.infer<OutputSchema>>;
  readonly contract?: ContractSpec | ExecutionContract;
  readonly channel?: string;
  readonly publish?: Record<string, string>;
}

export interface WorkflowConfig<InputSchema extends AnySchema, OutputSchema extends AnySchema> {
  readonly name: string;
  readonly input: InputSchema;
  readonly output: OutputSchema;
  readonly state?: StateSchema;
  readonly defaults?: ContractSpec | ExecutionContract;
}

export interface ChannelDefinition<Output> {
  readonly schema: AnySchema;
  readonly reducer: "last";
  // Phantom type until StepRun receives typed channel state.
  readonly __output?: Output;
}

export type StateSchema = Record<string, ChannelDefinition<unknown>>;

export interface StepNode {
  readonly id: string;
  readonly kind: "step";
  readonly input: AnySchema;
  readonly output: AnySchema;
  readonly run: StepRun<unknown, unknown>;
  readonly symbolRef: string;
  readonly contract?: ContractSpec | ExecutionContract;
  readonly channel?: string;
  mergeInputs?: string[];
  readonly publish?: Record<string, string>;
}

export class StepHandle<Input, Output> {
  readonly __input?: Input;
  readonly __output?: Output;

  constructor(readonly nodeId: string) {}
}

export class EndHandle<Output> {
  readonly __input?: Output;

  constructor(readonly nodeId: string) {}
}

export class PathBuilder<Current> {
  constructor(
    private readonly builder: WorkflowBuilder<unknown, unknown>,
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
}

export class MergeBuilder<Current> {
  constructor(
    private readonly builder: WorkflowBuilder<unknown, unknown>,
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
  readonly runtimeRegistry = new Map<string, StepRun<unknown, unknown>>();
  readonly channels: StateSchema;

  constructor(
    readonly name: string,
    readonly input: AnySchema,
    readonly output: AnySchema,
    readonly defaults: ContractSpec | ExecutionContract | undefined,
    channels: StateSchema = {}
  ) {
    this.channels = channels;
    this.graph.addNode(START_NODE, { kind: "start" });
    this.graph.addNode(END_NODE, { kind: "end" });
  }

  step<InputSchema extends AnySchema, OutputSchema extends AnySchema>(
    id: string,
    spec: StepSpec<InputSchema, OutputSchema>
  ): StepHandle<z.infer<InputSchema>, z.infer<OutputSchema>> {
    if (id === START_NODE || id === END_NODE || this.graph.hasNode(id)) {
      throw new GraphValidationError(`Duplicate or reserved step id "${id}"`);
    }

    const symbolRef = `${this.name}/${id}`;
    const node: StepNode = {
      id,
      kind: "step",
      input: spec.input,
      output: spec.output,
      run: spec.run as StepRun<unknown, unknown>,
      symbolRef,
      ...(spec.contract === undefined ? {} : { contract: spec.contract }),
      ...(spec.channel === undefined ? {} : { channel: spec.channel }),
      ...(spec.publish === undefined ? {} : { publish: spec.publish }),
    };

    this.stepNodes.set(id, node);
    this.runtimeRegistry.set(symbolRef, node.run);
    this.graph.addNode(id, { kind: "step" });

    return new StepHandle<z.infer<InputSchema>, z.infer<OutputSchema>>(id);
  }

  start(): PathBuilder<Input> {
    return new PathBuilder<Input>(this as WorkflowBuilder<unknown, unknown>, START_NODE);
  }

  from<StepInput, StepOutput>(step: StepHandle<StepInput, StepOutput>): PathBuilder<StepOutput> {
    return new PathBuilder<StepOutput>(this as WorkflowBuilder<unknown, unknown>, step.nodeId);
  }

  merge<StepOutput>(steps: readonly StepHandle<unknown, StepOutput>[]): MergeBuilder<StepOutput> {
    if (steps.length === 0) {
      throw new GraphValidationError("Merge requires at least one upstream step");
    }
    return new MergeBuilder<StepOutput>(
      this as WorkflowBuilder<unknown, unknown>,
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
    const target = this.stepNodes.get(to);
    if (target === undefined) {
      throw new GraphValidationError(`Merge target "${to}" must be a step`);
    }
    if (target.mergeInputs !== undefined) {
      throw new GraphValidationError(`Step "${to}" already has merge inputs`);
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
    config.defaults,
    config.state
  );
}

export function channel<Schema extends AnySchema>(schema: Schema): ChannelDefinition<z.infer<Schema>> {
  return { schema, reducer: "last" };
}

export function stateSchema<const Schema extends StateSchema>(schema: Schema): Schema {
  return schema;
}
