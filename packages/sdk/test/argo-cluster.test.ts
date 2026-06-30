import { assertStringIncludes, assertEquals } from "jsr:@std/assert";
import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import { compile, compileArgoWorkflow, datastore, workflow } from "../src/index.ts";

Deno.test("local Argo cluster runs generated 100-way batch merge DAG", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-argo-cluster-" });
  try {
    const compiled = await compile(makeBatchMergeWorkflow(100), {
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
    assertEquals(main.dag.tasks.length, 102);
    assertEquals(mergeTask?.dependencies?.length, 100);

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

function makeBatchMergeWorkflow(size: number) {
  const g = workflow({ name: "batch-merge", input: z.number(), output: z.number() });
  const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
  const workers = Array.from({ length: size }, (_, index) =>
    g.step(`worker-${String(index).padStart(3, "0")}`, {
      input: z.number(),
      output: z.number(),
      run: async ({ input }) => input + index,
    })
  );
  const merge = g.step("merge", {
    input: z.array(z.number()),
    output: z.number(),
    run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
  });

  g.start().to(split);
  for (const worker of workers) {
    g.from(split).to(worker);
  }
  g.merge(workers).to(merge).to(g.end());
  return g;
}

function mainTemplate(manifest: ReturnType<typeof compileArgoWorkflow>) {
  const template = manifest.spec.templates.find((candidate) => "dag" in candidate);
  if (template === undefined || !("dag" in template)) {
    throw new Error("Argo manifest does not contain a DAG template");
  }
  return template;
}
