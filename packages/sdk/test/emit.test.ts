import { assert, assertEquals, assertNotEquals } from "jsr:@std/assert";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ValidateFunction } from "ajv/dist/2020.js";
import { join } from "node:path";
import { z } from "zod";
import {
  contract,
  emitWorkflowSpec,
  env,
  net,
  secret,
  workflow,
  type WorkflowSpec,
} from "../src/index.ts";
import { sha256Text, stableStringify } from "../src/stable.ts";
import { graphCases } from "./graph-fixtures.ts";

Deno.test("emitted WorkflowSpec validates for every graph fixture", async () => {
  const validate = await compileWorkflowSpecValidator();

  await withSourcePackage(async (root) => {
    for (const graphCase of graphCases) {
      const spec = await emitWorkflowSpec(graphCase.build(), {
        source: { root, include: ["workflow.ts"] },
      });

      assert(
        validate(spec),
        `${graphCase.name} should validate: ${JSON.stringify(validate.errors)}`,
      );
      assertEquals(
        spec.graph.nodes.filter((node) => node.kind === "step").length,
        graphCase.expectedTasks,
      );
      assertEquals(spec.graph.edges.length, graphCase.expectedEdges);

      for (
        const [stepId, mergeInputs] of Object.entries(
          graphCase.mergeExpectations ?? {},
        )
      ) {
        const node = spec.graph.nodes.find((candidate) =>
          candidate.id === stepId
        );
        assertEquals(
          node?.kind === "step" ? node.mergeInputs : undefined,
          mergeInputs,
          graphCase.name,
        );
      }
    }
  });
});

Deno.test("WorkflowSpec emission is deterministic", async () => {
  await withSourcePackage(async (root) => {
    const first = await emitWorkflowSpec(graphCases[2]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });
    const second = await emitWorkflowSpec(graphCases[2]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });

    assertEquals(stableStringify(second), stableStringify(first));
    assertEquals(second.specHash, first.specHash);
  });
});

Deno.test("WorkflowSpec specHash excludes itself", async () => {
  await withSourcePackage(async (root) => {
    const spec = await emitWorkflowSpec(graphCases[3]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });
    const { specHash, ...withoutSpecHash } = spec;

    assertEquals(
      specHash,
      `sha256:${sha256Text(stableStringify(withoutSpecHash))}`,
    );
  });
});

Deno.test("contract merge emits effective contract refs and dedupes environments by environment inputs", async () => {
  await withSourcePackage(async (root) => {
    const defaults = contract({
      env: env.node({
        version: "22.12.0",
        packageManager: "pnpm",
        lockfile: "pnpm-lock.yaml",
      }),
      resources: { cpu: "0.5", memory: "512Mi" },
      secrets: [secret.ref("BASE_TOKEN")],
      network: net.denyAll(),
    });
    const g = workflow({
      name: "contracts",
      input: z.number(),
      output: z.number(),
      defaults,
    });
    const first = g.step("first", {
      input: z.number(),
      output: z.number(),
      contract: contract({
        resources: { memory: "1Gi" },
        secrets: [secret.ref("STEP_TOKEN")],
        network: net.allow("api.openai.com"),
      }),
      run: ({ input }) => input + 1,
    });
    const second = g.step("second", {
      input: z.number(),
      output: z.number(),
      contract: contract({
        resources: { cpu: "1" },
        secrets: [secret.ref("OTHER_TOKEN")],
      }),
      run: ({ input }) => input + 1,
    });
    g.start().to(first).to(second).to(g.end());

    const spec = await emitWorkflowSpec(g, {
      source: { root, include: ["workflow.ts"] },
    });
    const stepNodes = spec.graph.nodes.filter((node) => node.kind === "step");
    const firstContract = spec.contracts[
      stepNodes.find((node) => node.id === "first")!.contractRef
    ]!;
    const secondContract = spec
      .contracts[stepNodes.find((node) => node.id === "second")!.contractRef]!;

    assertEquals(Object.keys(spec.environments).length, 1);
    assert(stepNodes.every((node) => node.contractRef in spec.contracts));
    assertEquals(firstContract.environmentRef, secondContract.environmentRef);
    assertEquals(firstContract.resources, { cpu: "0.5", memory: "1Gi" });
    assertEquals(firstContract.network, {
      egress: "declared",
      hosts: ["api.openai.com"],
    });
    assertEquals(firstContract.secrets, [
      secret.ref("BASE_TOKEN"),
      secret.ref("STEP_TOKEN"),
    ]);
    assertEquals(secondContract.resources, { cpu: "1", memory: "512Mi" });
    assertEquals(secondContract.network, { egress: "none" });
    assertEquals(secondContract.secrets, [
      secret.ref("BASE_TOKEN"),
      secret.ref("OTHER_TOKEN"),
    ]);
  });
});

Deno.test("source package packageHash follows included file content only", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-emit-source-" });
  try {
    await Deno.writeTextFile(
      join(root, "workflow.ts"),
      "export const version = 1;\n",
    );
    await Deno.writeTextFile(
      join(root, "ignored.ts"),
      "export const version = 1;\n",
    );

    const first = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });
    await Deno.writeTextFile(
      join(root, "workflow.ts"),
      "export const version = 2;\n",
    );
    const second = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });
    await Deno.writeTextFile(
      join(root, "ignored.ts"),
      "export const version = 2;\n",
    );
    const third = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });

    assertNotEquals(
      sourcePackage(first).packageHash,
      sourcePackage(second).packageHash,
    );
    assertNotEquals(
      sourcePackage(first).files[0]!.hash,
      sourcePackage(second).files[0]!.hash,
    );
    assertEquals(
      sourcePackage(third).packageHash,
      sourcePackage(second).packageHash,
    );
    assertEquals(omitSourceAndHash(first), omitSourceAndHash(second));
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});

let workflowSpecValidator: Promise<ValidateFunction> | undefined;

function compileWorkflowSpecValidator(): Promise<ValidateFunction> {
  workflowSpecValidator ??= compileSchema(
    "../../../conformance/schema/workflow-spec.schema.json",
  );
  return workflowSpecValidator;
}

async function compileSchema(path: string): Promise<ValidateFunction> {
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  return ajv.compile((await readJson(path)) as AnySchema);
}

async function readJson(path: string): Promise<unknown> {
  return JSON.parse(await Deno.readTextFile(new URL(path, import.meta.url)));
}

async function withSourcePackage(
  callback: (root: string) => Promise<void>,
): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-emit-" });
  try {
    await Deno.writeTextFile(
      join(root, "workflow.ts"),
      "export const workflow = true;\n",
    );
    await callback(root);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}

function sourcePackage(
  spec: WorkflowSpec,
): WorkflowSpec["sourcePackages"][string] {
  return spec.sourcePackages["ts-main"]!;
}

function omitSourceAndHash(
  spec: WorkflowSpec,
): Omit<WorkflowSpec, "sourcePackages" | "specHash"> {
  const { sourcePackages: _sourcePackages, specHash: _specHash, ...rest } =
    spec;
  return rest;
}
