import { GraphValidationError } from "./errors.ts";
import { END_NODE, START_NODE, type WorkflowBuilder } from "./workflow.ts";

export function validateGraphShape(
  builder: WorkflowBuilder<unknown, unknown>,
): void {
  const graph = builder.graph;

  if (!graph.hasNode(START_NODE) || !graph.hasNode(END_NODE)) {
    throw new GraphValidationError(
      "Workflow graph must contain exactly one start and one end",
    );
  }

  assertAcyclic(builder);

  const reachableFromStart = traverse(builder, START_NODE, "outbound");
  for (const step of builder.stepNodes.keys()) {
    if (!reachableFromStart.has(step)) {
      throw new GraphValidationError(
        `Step "${step}" is not reachable from start`,
      );
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
      throw new GraphValidationError(
        `Workflow graph contains a cycle at "${node}"`,
      );
    }

    visiting.add(node);
    for (const neighbor of graph.outboundNeighbors(node)) {
      visit(neighbor);
    }
    visiting.delete(node);
    visited.add(node);
  };

  for (const node of graph.nodes()) {
    visit(node);
  }
}

function traverse(
  builder: WorkflowBuilder<unknown, unknown>,
  start: string,
  direction: "inbound" | "outbound",
): Set<string> {
  const graph = builder.graph;
  const seen = new Set<string>();
  const queue = [start];

  while (queue.length > 0) {
    const node = queue.shift();
    if (node === undefined || seen.has(node)) continue;
    seen.add(node);

    const next = direction === "outbound"
      ? graph.outboundNeighbors(node)
      : graph.inboundNeighbors(node);
    queue.push(...next);
  }

  return seen;
}
