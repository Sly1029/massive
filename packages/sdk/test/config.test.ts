import { assertEquals, assertNotEquals } from "jsr:@std/assert";
import { join } from "node:path";
import { z } from "zod";
import {
  defineWorkflowPackage,
  emitWorkflowSpec,
  env,
  target,
  workflow,
} from "../src/index.ts";
import { sha256Text, stableStringify } from "../src/stable.ts";

Deno.test("package config emits requested local and Argo targets", async () => {
  await withPackageRoot(async (root) => {
    const g = mathWorkflow();
    const workflowPackage = defineWorkflowPackage({
      include: ["src/workflow.ts", "package.json", "pnpm-lock.yaml"],
      entrypoint: "./src/workflow.ts#default",
      environment: env.node({
        version: "22.12.0",
        packageManager: "pnpm",
        lockfile: "pnpm-lock.yaml",
      }),
      targets: [
        target.local(),
        target.argo({
          namespace: "workflows",
          serviceAccountName: "massive-runner",
          workflowTemplateName: "math",
        }),
      ],
    });

    const spec = await emitWorkflowSpec(g, {
      package: workflowPackage,
      packageRoot: root,
    });

    assertEquals(spec.targets, [
      { kind: "local" },
      {
        kind: "argo",
        namespace: "workflows",
        serviceAccountName: "massive-runner",
        workflowTemplateName: "math",
      },
    ]);
    assertEquals(Object.values(spec.environments), [{
      kind: "node",
      version: "22.12.0",
      packageManager: "pnpm",
      lockfile: "pnpm-lock.yaml",
    }]);
    assertEquals(spec.symbols["ts-main:./src/workflow.ts#double"]?.module, "./src/workflow.ts");
  });
});

Deno.test("target requests participate in WorkflowSpec hash", async () => {
  await withPackageRoot(async (root) => {
    const basePackage = defineWorkflowPackage({
      include: ["src/workflow.ts"],
      entrypoint: "./src/workflow.ts#default",
      targets: [target.local()],
    });
    const argoPackage = defineWorkflowPackage({
      include: ["src/workflow.ts"],
      entrypoint: "./src/workflow.ts#default",
      targets: [
        target.local(),
        target.argo({
          namespace: "workflows",
          serviceAccountName: "massive-runner",
        }),
      ],
    });

    const localSpec = await emitWorkflowSpec(mathWorkflow(), {
      package: basePackage,
      packageRoot: root,
    });
    const argoSpec = await emitWorkflowSpec(mathWorkflow(), {
      package: argoPackage,
      packageRoot: root,
    });
    const { specHash, ...withoutSpecHash } = argoSpec;

    assertNotEquals(argoSpec.specHash, localSpec.specHash);
    assertEquals(
      specHash,
      `sha256:${sha256Text(stableStringify(withoutSpecHash))}`,
    );
  });
});

function mathWorkflow() {
  const g = workflow({ name: "math", input: z.number(), output: z.number() });
  const double = g.step("double", {
    input: z.number(),
    output: z.number(),
    run: ({ input }) => input * 2,
  });
  g.start().to(double).to(g.end());
  return g;
}

async function withPackageRoot(
  callback: (root: string) => Promise<void>,
): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-config-" });
  try {
    await Deno.mkdir(join(root, "src"), { recursive: true });
    await Deno.writeTextFile(
      join(root, "src", "workflow.ts"),
      "export const workflow = true;\n",
    );
    await Deno.writeTextFile(join(root, "package.json"), "{}\n");
    await Deno.writeTextFile(join(root, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n");
    await callback(root);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}
