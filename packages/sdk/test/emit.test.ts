import {
  assert,
  assertEquals,
  assertNotEquals,
  assertRejects,
  assertThrows,
} from "jsr:@std/assert";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ValidateFunction } from "ajv/dist/2020.js";
import { join } from "node:path";
import { z } from "zod";
import { GraphValidationError, SourcePackagePathError } from "../src/errors.ts";
import {
  contract,
  emitWorkflowSpec,
  env,
  net,
  retry,
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

Deno.test("WorkflowSpec and source identities declare their hash recipes", async () => {
  await withSourcePackage(async (root) => {
    const spec = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });

    assertEquals(spec.hashing, {
      algorithm: "sha256",
      canonicalization: "canonical-json-v0",
      recipe: "workflow-spec",
      recipeVersion: 1,
    });
    assertEquals(sourcePackage(spec).hashing, {
      algorithm: "sha256",
      canonicalization: "canonical-json-v0",
      recipe: "source-package",
      recipeVersion: 1,
    });
  });
});

Deno.test("source checkout location does not change WorkflowSpec identity", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-emit-location-" });
  try {
    const firstRoot = join(root, "first");
    const secondRoot = join(root, "nested", "second");
    await Deno.mkdir(firstRoot, { recursive: true });
    await Deno.mkdir(secondRoot, { recursive: true });
    for (const checkout of [firstRoot, secondRoot]) {
      await Deno.writeTextFile(
        join(checkout, "workflow.ts"),
        "export const workflowVersion = 1;\n",
      );
    }

    const first = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root: firstRoot, include: ["workflow.ts"] },
    });
    const second = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root: secondRoot, include: ["workflow.ts"] },
    });

    assertEquals(second.specHash, first.specHash);
    assertEquals(second, first);
    assertEquals("root" in sourcePackage(first), false);
    assertEquals("include" in sourcePackage(first), false);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
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
      input: z.int(),
      output: z.int(),
      defaults,
    });
    const first = g.step("first", {
      input: z.int(),
      output: z.int(),
      contract: contract({
        resources: { memory: "1Gi" },
        secrets: [secret.ref("STEP_TOKEN")],
        network: net.allow("api.openai.com"),
      }),
      run: ({ input }) => input + 1,
    });
    const second = g.step("second", {
      input: z.int(),
      output: z.int(),
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

Deno.test("contract retry and timeout emit per step and validate against the WorkflowSpec schema", async () => {
  const validate = await compileWorkflowSpecValidator();

  await withSourcePackage(async (root) => {
    const g = workflow({
      name: "retries",
      input: z.int(),
      output: z.int(),
      defaults: contract({
        retry: retry({
          attempts: 3,
          delaySeconds: 5,
          backoffFactor: 3,
          maxDelaySeconds: 60,
        }),
        timeoutSeconds: 300,
      }),
    });
    const noRetry = g.step("noRetry", {
      input: z.int(),
      output: z.int(),
      contract: { retry: retry({ attempts: 1 }) },
      run: ({ input }) => input + 1,
    });
    const shortTimeout = g.step("shortTimeout", {
      input: z.int(),
      output: z.int(),
      contract: { timeoutSeconds: 60 },
      run: ({ input }) => input + 1,
    });
    g.start().to(noRetry).to(shortTimeout).to(g.end());

    const spec = await emitWorkflowSpec(g, {
      source: { root, include: ["workflow.ts"] },
    });
    assert(validate(spec), JSON.stringify(validate.errors));
    const contractFor = (id: string) => {
      const node = spec.graph.nodes.find((candidate) => candidate.id === id);
      return spec.contracts[node?.kind === "step" ? node.contractRef : ""]!;
    };

    // A step retry replaces the default policy as a unit.
    assertEquals(contractFor("noRetry").retry, {
      maxAttempts: 1,
      delaySeconds: 10,
      backoffFactor: 2,
      maxDelaySeconds: 600,
    });
    assertEquals(contractFor("noRetry").timeoutSeconds, 300);
    assertEquals(contractFor("shortTimeout").retry, {
      maxAttempts: 3,
      delaySeconds: 5,
      backoffFactor: 3,
      maxDelaySeconds: 60,
    });
    assertEquals(contractFor("shortTimeout").timeoutSeconds, 60);
  });
});

Deno.test("contracts without retry or timeout emit neither field", async () => {
  await withSourcePackage(async (root) => {
    const spec = await emitWorkflowSpec(graphCases[2]!.build(), {
      source: { root, include: ["workflow.ts"] },
    });

    for (const emitted of Object.values(spec.contracts)) {
      assertEquals("retry" in emitted, false);
      assertEquals("timeoutSeconds" in emitted, false);
    }
  });
});

Deno.test("ExecutionContract.extend replaces retry whole and overrides timeout field-wise", () => {
  const base = contract({
    retry: retry({
      attempts: 5,
      delaySeconds: 1,
      backoffFactor: 3,
      maxDelaySeconds: 30,
    }),
    timeoutSeconds: 120,
  });

  assertEquals(base.extend({ retry: retry({ attempts: 2 }) }).spec, {
    retry: {
      maxAttempts: 2,
      delaySeconds: 10,
      backoffFactor: 2,
      maxDelaySeconds: 600,
    },
    timeoutSeconds: 120,
  });
  assertEquals(base.extend({ timeoutSeconds: 10 }).spec, {
    retry: base.spec.retry,
    timeoutSeconds: 10,
  });
});

Deno.test("retry and timeout policies reject values outside the WorkflowSpec bounds", async () => {
  assertEquals(retry({ attempts: 4 }), {
    maxAttempts: 4,
    delaySeconds: 10,
    backoffFactor: 2,
    maxDelaySeconds: 600,
  });
  for (
    const [options, message] of [
      [{ attempts: 0 }, "retry() attempts must be an integer from 1 to 100, got 0"],
      [{ attempts: 101 }, "retry() attempts must be an integer from 1 to 100"],
      [{ attempts: 1.5 }, "retry() attempts must be an integer"],
      [{ attempts: 2, delaySeconds: -1 }, "retry() delaySeconds must be an integer from 0 to 86400"],
      [{ attempts: 2, backoffFactor: 0 }, "retry() backoffFactor must be an integer from 1 to 10"],
      [{ attempts: 2, backoffFactor: 11 }, "retry() backoffFactor must be an integer from 1 to 10"],
      [{ attempts: 2, maxDelaySeconds: 86_401 }, "retry() maxDelaySeconds must be an integer from 0 to 86400"],
      [{ attempts: 2, delaySeconds: 700 }, "retry() maxDelaySeconds (600) must be at least delaySeconds (700)"],
    ] as const
  ) {
    assertThrows(() => retry(options), Error, message);
  }
  for (const timeoutSeconds of [0, 604_801, 2.5]) {
    assertThrows(
      () => contract({ timeoutSeconds }),
      Error,
      "contract timeoutSeconds must be an integer from 1 to 604800",
    );
  }

  await withSourcePackage(async (root) => {
    const g = workflow({ name: "invalid-timeout", input: z.int(), output: z.int() });
    const step = g.step("step", {
      input: z.int(),
      output: z.int(),
      contract: { timeoutSeconds: 0 },
      run: ({ input }) => input,
    });
    g.start().to(step).to(g.end());
    await assertRejects(
      () =>
        emitWorkflowSpec(g, { source: { root, include: ["workflow.ts"] } }),
      Error,
      "contract timeoutSeconds must be an integer from 1 to 604800",
    );
  });
});

Deno.test("source package packageHash follows exact included bytes, not an AST", async () => {
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
      "// same program, different source bytes\nexport const version = 1;\n",
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

Deno.test("source package hashes a symlinked root identically to its real target", async () => {
  const packageRoot = await Deno.makeTempDir({
    prefix: "massive-emit-package-",
  });
  const realRoot = await Deno.makeTempDir({
    prefix: "massive-emit-real-",
  });
  try {
    await Deno.writeTextFile(
      join(realRoot, "workflow.ts"),
      "export const real = true;\n",
    );
    const symlinkRoot = join(packageRoot, "source");
    await Deno.symlink(realRoot, symlinkRoot, { type: "dir" });

    const viaReal = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root: realRoot, include: ["workflow.ts"] },
    });
    const viaSymlink = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root: symlinkRoot, include: ["workflow.ts"] },
    });

    assertEquals(
      sourcePackage(viaSymlink).packageHash,
      sourcePackage(viaReal).packageHash,
    );
    assertEquals(sourcePackage(viaSymlink).files, sourcePackage(viaReal).files);
  } finally {
    await Deno.remove(packageRoot, { recursive: true });
    await Deno.remove(realRoot, { recursive: true });
  }
});

Deno.test("source package manifest orders files by UTF-16 code units, not locale", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-emit-order-" });
  try {
    await Deno.writeTextFile(
      join(root, "workflow.ts"),
      "export const w = 1;\n",
    );
    await Deno.writeTextFile(join(root, "B.ts"), "export const b = 1;\n");
    await Deno.writeTextFile(join(root, "a.ts"), "export const a = 1;\n");
    await Deno.writeTextFile(join(root, "😀.ts"), "export const emoji = 1;\n");
    await Deno.writeTextFile(join(root, ".ts"), "export const privateUse = 1;\n");

    const first = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root, include: ["*.ts"] },
    });

    // The emoji's leading UTF-16 surrogate sorts before U+E000 even though its
    // Unicode code point is larger. Locale and code-point sorting both differ.
    assertEquals(
      sourcePackage(first).files.map((file) => file.path),
      ["B.ts", "a.ts", "workflow.ts", "😀.ts", ".ts"],
    );

    const second = await emitWorkflowSpec(graphCases[1]!.build(), {
      source: { root, include: ["*.ts"] },
    });
    assertEquals(
      sourcePackage(second).packageHash,
      sourcePackage(first).packageHash,
    );
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});

Deno.test("source package rejects included file symlinks escaping root", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-emit-source-" });
  const outsideRoot = await Deno.makeTempDir({
    prefix: "massive-emit-outside-",
  });
  try {
    await Deno.writeTextFile(
      join(outsideRoot, "workflow.ts"),
      "export const outside = true;\n",
    );
    await Deno.symlink(
      join(outsideRoot, "workflow.ts"),
      join(root, "workflow.ts"),
      {
        type: "file",
      },
    );

    await assertRejects(
      () =>
        emitWorkflowSpec(graphCases[1]!.build(), {
          source: { root, include: ["workflow.ts"] },
        }),
      SourcePackagePathError,
      "outside root after following symlinks",
    );
  } finally {
    await Deno.remove(root, { recursive: true });
    await Deno.remove(outsideRoot, { recursive: true });
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
