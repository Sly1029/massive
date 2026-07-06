import { assertEquals, assertRejects } from "jsr:@std/assert";
import { join } from "node:path";
import {
  MassiveError,
  resolveWorkflowEntrypoint,
} from "../src/index.ts";

const sdkUrl = new URL("../src/index.ts", import.meta.url).href;
const zodUrl = new URL("../../../node_modules/zod/index.js", import.meta.url).href;

Deno.test("resolver selects default export for workflow.ts", async () => {
  await withTempDir(async (root) => {
    const file = join(root, "workflow.ts");
    await Deno.writeTextFile(file, workflowModule({
      defaultName: "defaulted",
      named: ["other"],
    }));

    const resolved = await resolveWorkflowEntrypoint(file);

    assertEquals(resolved.workflow.name, "defaulted");
    assertEquals(resolved.selectedExport, "default");
    assertEquals(resolved.explicitConfig, false);
    assertEquals(resolved.package.targets, [{ kind: "local" }]);
  });
});

Deno.test("resolver selects named export for workflow.ts#name", async () => {
  await withTempDir(async (root) => {
    const file = join(root, "workflow.ts");
    await Deno.writeTextFile(file, workflowModule({
      defaultName: "defaulted",
      named: ["chosen"],
    }));

    const resolved = await resolveWorkflowEntrypoint(`${file}#chosen`);

    assertEquals(resolved.workflow.name, "chosen");
    assertEquals(resolved.selectedExport, "chosen");
    assertEquals(resolved.package.entrypoint, "./workflow.ts#chosen");
  });
});

Deno.test("resolver reports ambiguity with exported workflow candidates", async () => {
  await withTempDir(async (root) => {
    const file = join(root, "workflow.ts");
    await Deno.writeTextFile(file, workflowModule({
      named: ["alpha", "beta"],
    }));

    await assertRejects(
      () => resolveWorkflowEntrypoint(file),
      MassiveError,
      "alpha, beta",
    );
  });
});

Deno.test("resolver loads directory entrypoint through massive.config.ts", async () => {
  await withTempDir(async (root) => {
    await Deno.mkdir(join(root, "flows"), { recursive: true });
    await Deno.writeTextFile(
      join(root, "flows", "workflow.ts"),
      workflowModule({ named: ["chosen"] }),
    );
    await Deno.writeTextFile(
      join(root, "massive.config.ts"),
      [
        `import { defineWorkflowPackage, target } from "${sdkUrl}";`,
        "export default defineWorkflowPackage({",
        "  include: ['flows/workflow.ts', 'package.json'],",
        "  entrypoint: './flows/workflow.ts#chosen',",
        "  targets: [target.local(), target.argo({ namespace: 'workflows', serviceAccountName: 'runner' })],",
        "});",
        "",
      ].join("\n"),
    );
    await Deno.writeTextFile(join(root, "package.json"), "{}\n");

    const resolved = await resolveWorkflowEntrypoint(root);

    assertEquals(resolved.workflow.name, "chosen");
    assertEquals(resolved.selectedExport, "chosen");
    assertEquals(resolved.packageRoot, root);
    assertEquals(resolved.explicitConfig, true);
    assertEquals(resolved.source, {
      root,
      include: ["flows/workflow.ts", "package.json"],
      module: "./flows/workflow.ts",
    });
    assertEquals(resolved.package.targets, [
      { kind: "local" },
      {
        kind: "argo",
        namespace: "workflows",
        serviceAccountName: "runner",
      },
    ]);
  });
});

Deno.test("zero-config includes nearby package files and refuses Argo", async () => {
  await withTempDir(async (root) => {
    const file = join(root, "workflow.ts");
    await Deno.writeTextFile(file, workflowModule({
      defaultName: "single",
    }));
    await Deno.writeTextFile(join(root, "package.json"), "{}\n");
    await Deno.writeTextFile(join(root, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n");

    const resolved = await resolveWorkflowEntrypoint(file, { target: "local" });

    assertEquals(resolved.explicitConfig, false);
    assertEquals(resolved.package.include, [
      "workflow.ts",
      "package.json",
      "pnpm-lock.yaml",
    ]);
    assertEquals(resolved.package.targets, [{ kind: "local" }]);
    await assertRejects(
      () => resolveWorkflowEntrypoint(file, { target: "argo" }),
      MassiveError,
      "requires massive.config.ts",
    );
  });
});

function workflowModule(config: {
  readonly defaultName?: string;
  readonly named?: readonly string[];
}): string {
  const lines = [
    `import { workflow } from "${sdkUrl}";`,
    `import { z } from "${zodUrl}";`,
    "function build(name: string) {",
    "  const g = workflow({ name, input: z.number(), output: z.number() });",
    "  const step = g.step('step', { input: z.number(), output: z.number(), run: ({ input }) => input });",
    "  g.start().to(step).to(g.end());",
    "  return g;",
    "}",
  ];

  for (const name of config.named ?? []) {
    lines.push(`export const ${name} = build('${name}');`);
  }
  if (config.defaultName !== undefined) {
    lines.push(`export default build('${config.defaultName}');`);
  }

  return `${lines.join("\n")}\n`;
}

async function withTempDir(callback: (root: string) => Promise<void>): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-resolve-" });
  try {
    await callback(root);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}
