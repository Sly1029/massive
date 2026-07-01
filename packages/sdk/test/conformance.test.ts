import { assert, assertEquals } from "jsr:@std/assert";
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

async function readGraphCatalog(): Promise<GraphCatalog> {
  return GraphCatalogSchema.parse(
    JSON.parse(await Deno.readTextFile(new URL("../../../conformance/graph-catalog.json", import.meta.url)))
  );
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
