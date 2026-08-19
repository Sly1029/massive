import { assertEquals, assertNotEquals } from "jsr:@std/assert";
import { join } from "node:path";
import { z } from "zod";
import {
  computeDeploymentHash,
  defineWorkflowPackage,
  deployment,
  emitDeploymentSpec,
  emitWorkflowSpec,
  env,
  workflow,
} from "../src/index.ts";
import { sha256Text, stableStringify } from "../src/stable.ts";

Deno.test("deployment profiles lower separately from a package workflow spec", async () => {
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
      deploymentProfiles: [
        deployment.local(),
        deployment.argo({
          name: "argo-staging",
          artifactStoreBinding: "staging-artifacts",
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

    const local = emitDeploymentSpec(
      "sha256:" + "1".repeat(64),
      workflowPackage.deploymentProfiles![0],
    );
    const argo = emitDeploymentSpec(
      "sha256:" + "1".repeat(64),
      workflowPackage.deploymentProfiles![1],
    );
    assertEquals(local.planHash, argo.planHash);
    assertEquals(local.hashing, {
      algorithm: "sha256",
      canonicalization: "canonical-json-v0",
      recipe: "deployment-spec",
      recipeVersion: 1,
    });
    assertEquals(local.profile, {
      name: "local",
      artifactStoreBinding: "local-artifacts",
      target: { kind: "local" },
    });
    assertNotEquals(local.deploymentHash, argo.deploymentHash);
    assertEquals(Object.values(spec.environments), [{
      kind: "node",
      version: "22.12.0",
      packageManager: "pnpm",
      lockfile: "pnpm-lock.yaml",
    }]);
    assertEquals(
      spec.symbols["ts-main:./src/workflow.ts#double"]?.module,
      "./src/workflow.ts",
    );
  });
});

Deno.test("deployment profiles do not participate in WorkflowSpec hash", async () => {
  await withPackageRoot(async (root) => {
    const basePackage = defineWorkflowPackage({
      include: ["src/workflow.ts"],
      entrypoint: "./src/workflow.ts#default",
      deploymentProfiles: [
        deployment.local({
          name: "local",
          artifactStoreBinding: "local-artifacts",
        }),
      ],
    });
    const argoPackage = defineWorkflowPackage({
      include: ["src/workflow.ts"],
      entrypoint: "./src/workflow.ts#default",
      deploymentProfiles: [
        deployment.argo({
          name: "argo",
          artifactStoreBinding: "argo-artifacts",
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

    assertEquals(argoSpec.specHash, localSpec.specHash);
    assertEquals(
      specHash,
      `sha256:${sha256Text(stableStringify(withoutSpecHash))}`,
    );
  });
});

Deno.test("DeploymentSpec shared fixtures retain their canonical hashes", async () => {
  for (const name of ["local", "argo"]) {
    const fixture = JSON.parse(
      await Deno.readTextFile(
        new URL(
          `../../../conformance/fixtures/deployments/${name}/deployment-spec.json`,
          import.meta.url,
        ),
      ),
    );
    assertEquals(
      computeDeploymentHash(fixture),
      fixture.deploymentHash,
      `${name} deployment fixture hash`,
    );
  }
});

function mathWorkflow() {
  const g = workflow({ name: "math", input: z.int(), output: z.int() });
  const double = g.step("double", {
    input: z.int(),
    output: z.int(),
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
    await Deno.writeTextFile(
      join(root, "pnpm-lock.yaml"),
      "lockfileVersion: '9.0'\n",
    );
    await callback(root);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}
