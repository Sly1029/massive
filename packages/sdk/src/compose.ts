import type { ContractSpec, ExecutionContract } from "./contract.ts";
import { GraphValidationError } from "./errors.ts";
import { validateGraphShape } from "./graph-validate.ts";
import type { AnySchema } from "./schema.ts";
import { compareCodeUnits } from "./stable.ts";
import {
  END_NODE,
  NODE_ID_PATTERN,
  START_NODE,
  type StepRun,
  type WorkflowBuilder,
} from "./workflow.ts";

// A step of the emitted graph. Steps expanded from a call keep their own export
// and their own workflow's defaults under a scoped id.
export interface FlatStep {
  readonly id: string;
  readonly exportName: string;
  readonly input: AnySchema;
  readonly output: AnySchema;
  readonly run: StepRun<never, unknown>;
  readonly contract?: ContractSpec | ExecutionContract;
  readonly defaults: ContractSpec | ExecutionContract | undefined;
  readonly mergeInputs?: readonly string[];
}

export interface FlatGraph {
  readonly steps: ReadonlyMap<string, FlatStep>;
  readonly edges: readonly (readonly [from: string, to: string])[];
}

/**
 * Expand every `call` into the child's steps, matching the Python SDK: calls
 * expand in code-unit order of their ids, child ids become "<call>--<child>",
 * and edges and merge inputs that name a call are rewired to the child's single
 * entry or exit. The result is the graph a hand-inlined workflow would emit.
 */
export function flattenWorkflow(
  builder: WorkflowBuilder<unknown, never>,
  active: ReadonlySet<WorkflowBuilder<unknown, never>> = new Set(),
): FlatGraph {
  if (active.has(builder)) {
    throw new GraphValidationError(
      `Recursive workflow call through "${builder.name}"`,
    );
  }
  validateGraphShape(builder);
  validateCallEdges(builder);
  builder.freeze();

  const steps = new Map<string, FlatStep>();
  for (const step of builder.stepNodes.values()) {
    steps.set(step.id, {
      id: step.id,
      exportName: step.exportName,
      input: step.input,
      output: step.output,
      run: step.run,
      ...(step.contract === undefined ? {} : { contract: step.contract }),
      defaults: builder.defaults,
      ...(step.mergeInputs === undefined
        ? {}
        : { mergeInputs: [...step.mergeInputs] }),
    });
  }
  const pendingCalls = new Map(
    [...builder.callNodes.values()]
      .sort((left, right) => compareCodeUnits(left.id, right.id))
      .map((call) => [call.id, call.mergeInputs] as const),
  );
  let edges: (readonly [string, string])[] = [];
  builder.graph.forEachDirectedEdge((_edge, _attributes, source, target) => {
    edges.push([source, target]);
  });

  for (const callId of [...pendingCalls.keys()]) {
    const call = builder.callNodes.get(callId)!;
    const child = flattenWorkflow(call.child, new Set([...active, builder]));
    const entries = child.edges.filter(([from]) => from === START_NODE);
    const exits = child.edges.filter(([, to]) => to === END_NODE);
    if (
      entries.length !== 1 || exits.length !== 1 ||
      entries[0]![1] === END_NODE || exits[0]![0] === START_NODE
    ) {
      throw new GraphValidationError(
        `Call "${callId}" requires child "${call.child.name}" to have exactly one start successor and one end predecessor that are child nodes`,
      );
    }
    const scoped = (childId: string): string => {
      const scopedId = `${callId}--${childId}`;
      if (!NODE_ID_PATTERN.test(scopedId)) {
        throw new GraphValidationError(
          `Call "${callId}" scopes child node "${childId}" as a ${scopedId.length}-character id; "<call id>--<child node id>" must fit the 128-character node id limit`,
        );
      }
      return scopedId;
    };
    const first = scoped(entries[0]![1]);
    const last = scoped(exits[0]![0]);
    const occupied = new Set([...steps.keys(), ...pendingCalls.keys()]);
    const fanIn = pendingCalls.get(callId);
    pendingCalls.delete(callId);

    for (const step of child.steps.values()) {
      const id = scoped(step.id);
      if (occupied.has(id)) {
        throw new GraphValidationError(
          `Call "${callId}" produces a duplicate scoped node id "${id}"`,
        );
      }
      // A call that consumes a fan-in passes it to its child's entry step.
      const mergeInputs = id === first && fanIn !== undefined
        ? fanIn
        : step.mergeInputs?.map(scoped);
      steps.set(id, {
        ...step,
        id,
        ...(mergeInputs === undefined ? {} : { mergeInputs }),
      });
    }

    edges = [
      ...edges.map(([from, to]) =>
        [from === callId ? last : from, to === callId ? first : to] as const
      ),
      ...child.edges
        .filter(([from, to]) => from !== START_NODE && to !== END_NODE)
        .map(([from, to]) => [scoped(from), scoped(to)] as const),
    ];
    const rewire = (sources: readonly string[] | undefined) =>
      sources?.map((source) => source === callId ? last : source);
    for (const [id, step] of steps) {
      if (step.mergeInputs?.includes(callId)) {
        steps.set(id, { ...step, mergeInputs: rewire(step.mergeInputs)! });
      }
    }
    for (const [id, sources] of pendingCalls) {
      pendingCalls.set(id, rewire(sources));
    }
  }

  return { steps, edges };
}

function validateCallEdges(builder: WorkflowBuilder<unknown, never>): void {
  for (const call of builder.callNodes.values()) {
    const inbound = builder.graph.inboundNeighbors(call.id);
    if (inbound.length === 0) {
      throw new GraphValidationError(`Call "${call.id}" must have an incoming edge`);
    }
    if (call.mergeInputs === undefined && inbound.length > 1) {
      throw new GraphValidationError(
        `Call "${call.id}" has ${inbound.length} incoming edges; join them with merge(...)`,
      );
    }
    if (builder.graph.outboundNeighbors(call.id).length === 0) {
      throw new GraphValidationError(`Call "${call.id}" must have an outgoing edge`);
    }
  }
}
