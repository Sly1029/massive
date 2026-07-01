import { assert, assertEquals } from "jsr:@std/assert";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ValidateFunction } from "ajv/dist/2020.js";
import { z } from "zod";
import { graphCases } from "./graph-fixtures.ts";

const MergeInputSchema = z.object({
  stepId: z.string().min(1),
  sourceIds: z.array(z.string().min(1)),
});

const GraphCatalogSchema = z.object({
  schemaVersion: z.literal(0),
  cases: z.array(
    z.object({
      id: z.string().min(1),
      shape: z.string().min(1),
      topology: z.string().min(1),
      executableSteps: z.number().int().nonnegative(),
      directedEdges: z.number().int().nonnegative(),
      mergeInputs: z.array(MergeInputSchema),
    })
  ),
});

type GraphCatalog = z.infer<typeof GraphCatalogSchema>;

Deno.test("conformance graph catalog JSON matches executable SDK graph fixtures", async () => {
  const catalog = await readGraphCatalog();
  const catalogIds = catalog.cases.map((catalogCase) => catalogCase.id);

  assertEquals(new Set(catalogIds).size, catalogIds.length);
  assertEquals(catalogIds, graphCases.map((graphCase) => graphCase.name));

  for (const graphCase of graphCases) {
    const catalogCase = catalog.cases.find((candidate) => candidate.id === graphCase.name);
    assert(catalogCase !== undefined);
    assertEquals(catalogCase.executableSteps, graphCase.expectedTasks, graphCase.name);
    assertEquals(catalogCase.directedEdges, graphCase.expectedEdges, graphCase.name);
    assertEquals(
      Object.fromEntries(catalogCase.mergeInputs.map((mergeInput) => [mergeInput.stepId, mergeInput.sourceIds])),
      graphCase.mergeExpectations ?? {},
      graphCase.name
    );
  }
});

Deno.test("graph catalog markdown table is rendered from canonical JSON", async () => {
  const [catalog, markdown] = await Promise.all([
    readGraphCatalog(),
    Deno.readTextFile(new URL("../../../conformance/graph-catalog.md", import.meta.url)),
  ]);

  assertEquals(extractGeneratedCatalogTable(markdown), renderGraphCatalogTable(catalog));
});

Deno.test("WorkflowSpec JSON Schema validates v0 graph fixture specs", async () => {
  const validate = await compileWorkflowSpecValidator();

  for (const caseId of ["passthrough", "linear-chain", "diamond"]) {
    const spec = await readJson(`../../../conformance/fixtures/specs/${caseId}/workflow-spec.json`);
    assert(validate(spec), `${caseId} should validate: ${JSON.stringify(validate.errors)}`);
  }
});

Deno.test("WorkflowSpec JSON Schema rejects step nodes without contractRef", async () => {
  const validate = await compileWorkflowSpecValidator();
  const spec = await readJson("../../../conformance/fixtures/specs/invalid-missing-contract-ref/workflow-spec.json");

  assertEquals(validate(spec), false);
  assert(JSON.stringify(validate.errors).includes("contractRef"));
});

Deno.test("WorkflowSpec JSON Schema rejects post-M2 channel fields in schema v0", async () => {
  const validate = await compileWorkflowSpecValidator();
  const spec = (await readJson("../../../conformance/fixtures/specs/linear-chain/workflow-spec.json")) as {
    graph: { nodes: Record<string, unknown>[] };
  };
  spec.graph.nodes.find((node) => node.kind === "step")!.channel = "result";

  assertEquals(validate(spec), false);
  assert(JSON.stringify(validate.errors).includes("channel"));
});

async function readGraphCatalog(): Promise<GraphCatalog> {
  return GraphCatalogSchema.parse(await readJson("../../../conformance/graph-catalog.json"));
}

async function compileWorkflowSpecValidator(): Promise<ValidateFunction> {
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  return ajv.compile((await readJson("../../../conformance/schema/workflow-spec.schema.json")) as AnySchema);
}

async function readJson(path: string): Promise<unknown> {
  return JSON.parse(await Deno.readTextFile(new URL(path, import.meta.url)));
}

function extractGeneratedCatalogTable(markdown: string): string {
  const start = "<!-- graph-catalog:start -->";
  const end = "<!-- graph-catalog:end -->";
  const startIndex = markdown.indexOf(start);
  const endIndex = markdown.indexOf(end);

  assert(startIndex !== -1, "graph catalog markdown is missing the generated table start marker");
  assert(endIndex !== -1, "graph catalog markdown is missing the generated table end marker");
  assert(startIndex < endIndex, "graph catalog markdown generated table markers are out of order");

  return markdown.slice(startIndex + start.length, endIndex).trim();
}

function renderGraphCatalogTable(catalog: GraphCatalog): string {
  return [
    "| Case ID | Shape | Topology | Executable steps | Directed edges | Merge inputs |",
    "|---------|-------|----------|------------------|----------------|--------------|",
    ...catalog.cases.map(
      (catalogCase) =>
        `| \`${catalogCase.id}\` | ${catalogCase.shape} | ${catalogCase.topology} | ${catalogCase.executableSteps} | ${catalogCase.directedEdges} | ${formatMergeInputs(catalogCase.mergeInputs)} |`
    ),
  ].join("\n");
}

function formatMergeInputs(mergeInputs: readonly z.infer<typeof MergeInputSchema>[]): string {
  if (mergeInputs.length === 0) {
    return "none";
  }

  return mergeInputs
    .map((mergeInput) => `${mergeInput.stepId}: ${formatSourceIds(mergeInput.sourceIds)}`)
    .join("; ");
}

function formatSourceIds(sourceIds: readonly string[]): string {
  if (sourceIds.length > 12) {
    return `${sourceIds[0]}..${sourceIds[sourceIds.length - 1]} (${sourceIds.length} sources)`;
  }

  return sourceIds.join(", ");
}
