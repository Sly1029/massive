import { z } from "zod";
import type { JsonValue } from "./stable.ts";

const JsonValueSchema: z.ZodType<JsonValue> = z.lazy(() =>
  z.union([
    z.null(),
    z.boolean(),
    z.number(),
    z.string(),
    z.array(JsonValueSchema),
    z.record(z.string(), JsonValueSchema),
  ])
);

export const WorkflowPlanJsonV0Schema = z.object({
  schemaVersion: z.literal(0),
  encoding: z.literal("json-v0"),
  planHash: z.string().optional(),
  target: z.literal("local"),
  workflow: z.object({
    name: z.string(),
    inputSchema: z.string(),
    outputSchema: z.string(),
  }),
  source: z.object({
    root: z.string(),
    include: z.array(z.string()),
    files: z.array(z.object({ path: z.string(), hash: z.string() })),
    sourcePackageHash: z.string(),
  }),
  symbols: z.object({
    symbolManifestHash: z.string(),
    steps: z.array(
      z.object({
        stepId: z.string(),
        name: z.string(),
        sourcePackageHash: z.string(),
      })
    ),
  }),
  schemas: z.record(z.string(), JsonValueSchema),
  graph: z.object({
    start: z.string(),
    end: z.string(),
    nodes: z.array(
      z.discriminatedUnion("kind", [
        z.object({ id: z.string(), kind: z.literal("start") }),
        z.object({ id: z.string(), kind: z.literal("end") }),
        z.object({
          id: z.string(),
          kind: z.literal("step"),
          inputSchema: z.string(),
          outputSchema: z.string(),
          symbolRef: z.string(),
          channel: z.string().optional(),
          mergeInputs: z.array(z.string()).optional(),
          publish: z.record(z.string(), z.string()).optional(),
        }),
      ])
    ),
    edges: z.array(z.object({ from: z.string(), to: z.string() })),
  }),
  channels: z.record(
    z.string(),
    z.object({
      schema: z.string(),
      reducer: z.literal("last"),
    })
  ),
});

export type WorkflowPlanJsonV0 = z.infer<typeof WorkflowPlanJsonV0Schema>;
