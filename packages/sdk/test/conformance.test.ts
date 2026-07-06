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
  const caseIds = (await listFixtureCases("specs")).filter((caseId) => !caseId.startsWith("invalid-"));
  assert(caseIds.length >= 3, "expected at least the passthrough, linear-chain, and diamond spec fixtures");

  for (const caseId of caseIds) {
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

Deno.test("StepInvocationDescriptor JSON Schema validates linear-chain descriptor fixture", async () => {
  const validate = await compileStepInvocationDescriptorValidator();
  const descriptor = await readJson("../../../conformance/fixtures/descriptors/linear-chain/descriptor.json");

  assert(validate(descriptor), `linear-chain descriptor should validate: ${JSON.stringify(validate.errors)}`);
});

Deno.test("StepInvocationDescriptor JSON Schema rejects descriptors missing required fields", async () => {
  const validate = await compileStepInvocationDescriptorValidator();
  const descriptor = (await readJson("../../../conformance/fixtures/descriptors/linear-chain/descriptor.json")) as Record<
    string,
    unknown
  >;
  const missingPlanHash = { ...descriptor };
  delete missingPlanHash.planHash;

  assertEquals(validate(missingPlanHash), false);
  assert(JSON.stringify(validate.errors).includes("planHash"));
});

Deno.test("WorkflowPlan JSON projection is structurally consistent with matching WorkflowSpec fixtures", async () => {
  const caseIds = await listFixtureCases("plans");
  assert(caseIds.length >= 3, "expected at least the passthrough, linear-chain, and diamond plan fixtures");

  for (const caseId of caseIds) {
    const [spec, plan] = await Promise.all([
      readJson(`../../../conformance/fixtures/specs/${caseId}/workflow-spec.json`),
      readJson(`../../../conformance/fixtures/plans/${caseId}/workflow-plan.json`),
    ]);

    assertSpecPlanStructuralConsistency(spec, plan, caseId);
    assertPlanReferencesResolve(plan, caseId);
    assertPlanHashRefDigests(plan, caseId);
  }
});

Deno.test("StepInvocationDescriptor JSON Schema rejects datastore keys that violate key syntax", async () => {
  const validate = await compileStepInvocationDescriptorValidator();
  const descriptor = (await readJson("../../../conformance/fixtures/descriptors/linear-chain/descriptor.json")) as {
    output: { artifact: { key: string } };
  };

  for (const key of ["..\\..\\etc\\passwd", "a//b/output.json", "./x", "/absolute/output.json", "a/../b.json"]) {
    const tampered = structuredClone(descriptor);
    tampered.output.artifact.key = key;
    assertEquals(validate(tampered), false, `key ${JSON.stringify(key)} should be rejected`);
  }
});

async function readGraphCatalog(): Promise<GraphCatalog> {
  return GraphCatalogSchema.parse(await readJson("../../../conformance/graph-catalog.json"));
}

let workflowSpecValidator: Promise<ValidateFunction> | undefined;
let stepInvocationDescriptorValidator: Promise<ValidateFunction> | undefined;

function compileWorkflowSpecValidator(): Promise<ValidateFunction> {
  workflowSpecValidator ??= compileSchema("../../../conformance/schema/workflow-spec.schema.json");
  return workflowSpecValidator;
}

function compileStepInvocationDescriptorValidator(): Promise<ValidateFunction> {
  stepInvocationDescriptorValidator ??= compileSchema(
    "../../../conformance/schema/step-invocation-descriptor.schema.json"
  );
  return stepInvocationDescriptorValidator;
}

async function compileSchema(path: string): Promise<ValidateFunction> {
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  return ajv.compile((await readJson(path)) as AnySchema);
}

async function listFixtureCases(kind: "specs" | "plans"): Promise<string[]> {
  const cases: string[] = [];
  for await (const entry of Deno.readDir(new URL(`../../../conformance/fixtures/${kind}`, import.meta.url))) {
    if (entry.isDirectory) {
      cases.push(entry.name);
    }
  }
  return cases.sort();
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

const HASH_REF_PATTERN = /^sha256:[0-9a-f]{64}$/;

type WorkflowSpecFixture = {
  workflow: { name: string };
  graph: {
    nodes: { id: string; kind: string; contractRef?: string }[];
    edges: { from: string; to: string }[];
  };
};

type WorkflowPlanFixture = {
  graph: {
    workflowName: string;
    nodes: { id: string; kind: string; contractRef?: string }[];
    edges: { from: string; to: string }[];
  };
  contracts: { contractRef: string; environmentRef: string }[];
  environments: { envRef: string }[];
};

function assertSpecPlanStructuralConsistency(spec: unknown, plan: unknown, caseId: string): void {
  const typedSpec = spec as WorkflowSpecFixture;
  const typedPlan = plan as WorkflowPlanFixture;

  assertEquals(typedPlan.graph.workflowName, typedSpec.workflow.name, `${caseId} workflow name`);
  assertEquals(
    new Set(typedPlan.graph.nodes.map((node) => node.id)),
    new Set(typedSpec.graph.nodes.map((node) => node.id)),
    `${caseId} node IDs`
  );
  assertEquals(
    normalizeEdges(typedPlan.graph.edges),
    normalizeEdges(typedSpec.graph.edges),
    `${caseId} edges`
  );

  for (const node of typedPlan.graph.nodes) {
    if (node.kind !== "step") {
      continue;
    }

    assert(typeof node.contractRef === "string" && node.contractRef.length > 0, `${caseId} step ${node.id} contractRef`);
  }
}

function assertPlanReferencesResolve(plan: unknown, caseId: string): void {
  const typedPlan = plan as WorkflowPlanFixture;
  const contractRefs = new Set(typedPlan.contracts.map((contract) => contract.contractRef));
  const environmentRefs = new Set(typedPlan.environments.map((environment) => environment.envRef));

  for (const node of typedPlan.graph.nodes) {
    if (node.kind !== "step") {
      continue;
    }

    assert(
      node.contractRef !== undefined && contractRefs.has(node.contractRef),
      `${caseId} step ${node.id} contractRef does not resolve in plan.contracts`
    );
  }

  for (const contract of typedPlan.contracts) {
    assert(
      environmentRefs.has(contract.environmentRef),
      `${caseId} contract ${contract.contractRef} environmentRef does not resolve in plan.environments`
    );
  }
}

function assertPlanHashRefDigests(plan: unknown, caseId: string): void {
  for (const digest of collectHashRefDigests(plan)) {
    assert(HASH_REF_PATTERN.test(digest), `${caseId} invalid HashRef digest: ${digest}`);
  }
}

function collectHashRefDigests(value: unknown): string[] {
  if (typeof value === "string") {
    return value.startsWith("sha256:") ? [value] : [];
  }

  if (Array.isArray(value)) {
    return value.flatMap((entry) => collectHashRefDigests(entry));
  }

  if (value !== null && typeof value === "object") {
    return Object.values(value).flatMap((entry) => collectHashRefDigests(entry));
  }

  return [];
}

function normalizeEdges(edges: readonly { from: string; to: string }[]): string[] {
  return edges.map((edge) => `${edge.from}->${edge.to}`).sort();
}
