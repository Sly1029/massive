import { randomUUID } from "node:crypto";
import { GraphValidationError, MassiveError } from "./errors.ts";
import { WorkflowPlanJsonV0Schema } from "./plan.ts";
import { stableStringify } from "./stable.ts";
import { END_NODE, START_NODE } from "./workflow.ts";
import type { CompiledWorkflow } from "./compile.ts";

export async function run<Output>(
  compiled: CompiledWorkflow<Output>,
  inputConfig: { readonly input: unknown }
): Promise<Output> {
  const plan = WorkflowPlanJsonV0Schema.parse(compiled.plan);
  const runId = randomUUID();
  const workflowInput = schemaFor(compiled, plan.workflow.inputSchema).parse(inputConfig.input);
  const outputs = new Map<string, unknown>([[START_NODE, workflowInput]]);
  const state: Record<string, unknown> = {};

  for (const nodeId of topologicalOrder(plan)) {
    if (nodeId === START_NODE || nodeId === END_NODE) continue;

    const node = plan.graph.nodes.find((candidate) => candidate.id === nodeId);
    if (node === undefined || node.kind !== "step") {
      throw new GraphValidationError(`Plan references unknown step "${nodeId}"`);
    }

    const inbound = plan.graph.edges.filter((edge) => edge.to === nodeId);
    const rawInput =
      node.mergeInputs === undefined
        ? singleInput(inbound, outputs, nodeId)
        : mergedInput(node.mergeInputs, inbound, outputs, nodeId);
    const stepInput = schemaFor(compiled, node.inputSchema).parse(rawInput);
    const stepRun = compiled.runtimeRegistry.get(node.symbolRef);
    if (stepRun === undefined) {
      throw new MassiveError(`No runtime symbol registered for "${node.symbolRef}"`);
    }

    const rawOutput = await stepRun({ input: stepInput, state, context: { runId, stepId: node.id } });
    const stepOutput = schemaFor(compiled, node.outputSchema).parse(rawOutput);
    outputs.set(nodeId, stepOutput);

    await compiled.datastore.put(
      `runs/${runId}/steps/${node.id}/attempts/1/output.json`,
      stableStringify(stepOutput)
    );

    if (node.channel !== undefined) {
      await persistChannel(compiled, runId, state, node.channel, stepOutput);
    }

    if (node.publish !== undefined) {
      for (const [field, channel] of Object.entries(node.publish).sort(([left], [right]) => left.localeCompare(right))) {
        await persistChannel(compiled, runId, state, channel, readOutputField(stepOutput, field, node.id));
      }
    }
  }

  const endInbound = plan.graph.edges.filter((edge) => edge.to === END_NODE);
  const result = schemaFor(compiled, plan.workflow.outputSchema).parse(singleInput(endInbound, outputs, END_NODE)) as Output;
  await compiled.datastore.put(`runs/${runId}/result.json`, stableStringify(result));
  return result;
}

function singleInput(inbound: { readonly from: string; readonly to: string }[], outputs: ReadonlyMap<string, unknown>, nodeId: string): unknown {
  if (inbound.length !== 1) {
    throw new GraphValidationError(`Local runner v0 requires exactly one input edge for "${nodeId}"`);
  }
  return outputs.get(inbound[0]!.from);
}

function mergedInput(
  mergeInputs: readonly string[],
  inbound: { readonly from: string; readonly to: string }[],
  outputs: ReadonlyMap<string, unknown>,
  nodeId: string
): unknown[] {
  const inboundSources = new Set(inbound.map((edge) => edge.from));
  for (const source of mergeInputs) {
    if (!inboundSources.has(source)) {
      throw new GraphValidationError(`Merge step "${nodeId}" is missing edge from "${source}"`);
    }
  }
  if (inbound.length !== mergeInputs.length) {
    throw new GraphValidationError(`Merge step "${nodeId}" has edges that are not declared merge inputs`);
  }
  return mergeInputs.map((source) => outputs.get(source));
}

function schemaFor(compiled: CompiledWorkflow<unknown>, schemaHash: string) {
  const schema = compiled.runtimeSchemas.get(schemaHash);
  if (schema === undefined) {
    throw new MassiveError(`No runtime schema registered for "${schemaHash}"`);
  }
  return schema;
}

async function persistChannel(
  compiled: CompiledWorkflow<unknown>,
  runId: string,
  state: Record<string, unknown>,
  channel: string,
  value: unknown
): Promise<void> {
  const channelPlan = compiled.plan.channels[channel];
  if (channelPlan === undefined) {
    throw new GraphValidationError(`Step published to unknown channel "${channel}"`);
  }

  const parsed = schemaFor(compiled, channelPlan.schema).parse(value);
  state[channel] = parsed;
  await compiled.datastore.put(`runs/${runId}/channels/${channel}/value.json`, stableStringify(parsed));
}

function readOutputField(output: unknown, field: string, stepId: string): unknown {
  if (output === null || typeof output !== "object" || !(field in output)) {
    throw new GraphValidationError(`Step "${stepId}" cannot publish missing output field "${field}"`);
  }
  return (output as Record<string, unknown>)[field];
}

function topologicalOrder(plan: ReturnType<typeof WorkflowPlanJsonV0Schema.parse>): string[] {
  const indegree = new Map<string, number>();
  const outbound = new Map<string, string[]>();

  for (const node of plan.graph.nodes) {
    indegree.set(node.id, 0);
    outbound.set(node.id, []);
  }

  for (const edge of plan.graph.edges) {
    indegree.set(edge.to, (indegree.get(edge.to) ?? 0) + 1);
    outbound.get(edge.from)?.push(edge.to);
  }

  const queue = [...indegree.entries()]
    .filter(([, degree]) => degree === 0)
    .map(([node]) => node)
    .sort();
  const order: string[] = [];

  while (queue.length > 0) {
    const node = queue.shift()!;
    order.push(node);

    for (const next of (outbound.get(node) ?? []).sort()) {
      const nextDegree = (indegree.get(next) ?? 0) - 1;
      indegree.set(next, nextDegree);
      if (nextDegree === 0) {
        queue.push(next);
        queue.sort();
      }
    }
  }

  if (order.length !== plan.graph.nodes.length) {
    throw new GraphValidationError("Plan graph contains a cycle");
  }

  return order;
}
