import { assertStringIncludes, assertEquals } from "jsr:@std/assert";
import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { compile, compileArgoWorkflow, datastore } from "../src/index.ts";
import { batchMergeGraphCase } from "./graph-fixtures.ts";

Deno.test("local Argo cluster runs generated 100-way batch merge DAG", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-argo-cluster-" });
  try {
    const graphCase = batchMergeGraphCase(100);
    const compiled = await compile(graphCase.build(), {
      target: "local",
      datastore: datastore.local({ path: join(root, "store") }),
      source: {
        root: new URL(".", import.meta.url).pathname,
        include: ["fixtures/**/*.ts"],
      },
    });
    const manifest = compileArgoWorkflow(compiled);
    const main = mainTemplate(manifest);
    const mergeTask = main.dag.tasks.find((task) => task.name === "merge");
    assertEquals(main.dag.tasks.length, graphCase.expectedTasks);
    assertEquals(mergeTask?.dependencies, graphCase.mergeExpectations?.merge);

    const manifestPath = join(root, "workflow.json");
    await writeFile(manifestPath, JSON.stringify(manifest, null, 2));

    const submit = new Deno.Command("argo", {
      args: ["submit", "-n", "argo", "--wait", manifestPath],
      stdout: "piped",
      stderr: "piped",
    });
    const output = await submit.output();
    const stdout = new TextDecoder().decode(output.stdout);
    const stderr = new TextDecoder().decode(output.stderr);

    if (!output.success) {
      throw new Error(`argo submit failed\nSTDOUT:\n${stdout}\nSTDERR:\n${stderr}`);
    }

    assertStringIncludes(stdout, "Succeeded");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

function mainTemplate(manifest: ReturnType<typeof compileArgoWorkflow>) {
  const template = manifest.spec.templates.find((candidate) => "dag" in candidate);
  if (template === undefined || !("dag" in template)) {
    throw new Error("Argo manifest does not contain a DAG template");
  }
  return template;
}
