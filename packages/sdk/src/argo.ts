import { z } from "zod";
import type { CompiledWorkflow } from "./compile.ts";
import { WorkflowPlanJsonV0Schema } from "./plan.ts";
import { run } from "./run.ts";

export const ArgoWorkflowManifestSchema = z.object({
  apiVersion: z.literal("argoproj.io/v1alpha1"),
  kind: z.literal("Workflow"),
  metadata: z.object({
    generateName: z.string(),
    labels: z.record(z.string(), z.string()),
    annotations: z.record(z.string(), z.string()),
  }),
  spec: z.object({
    entrypoint: z.literal("main"),
    serviceAccountName: z.literal("argo"),
    templates: z.array(
      z.union([
        z.object({
          name: z.literal("main"),
          dag: z.object({
            tasks: z.array(
              z.object({
                name: z.string(),
                template: z.string(),
                dependencies: z.array(z.string()).optional(),
              })
            ),
          }),
        }),
        z.object({
          name: z.string(),
          container: z.object({
            image: z.string(),
            command: z.array(z.string()),
            args: z.array(z.string()),
          }),
        }),
      ])
    ),
  }),
});

export type ArgoWorkflowManifest = z.infer<typeof ArgoWorkflowManifestSchema>;

export function compileArgoWorkflow(compiled: CompiledWorkflow<unknown>): ArgoWorkflowManifest {
  const plan = WorkflowPlanJsonV0Schema.parse(compiled.plan);
  const stepNodes = plan.graph.nodes.filter((node) => node.kind === "step");
  const tasks = stepNodes.map((node) => {
    const dependencies = plan.graph.edges
      .filter((edge) => edge.to === node.id && edge.from !== plan.graph.start)
      .map((edge) => edge.from)
      .sort();

    return {
      name: node.id,
      template: "massive-step",
      ...(dependencies.length === 0 ? {} : { dependencies }),
    };
  });

  return ArgoWorkflowManifestSchema.parse({
    apiVersion: "argoproj.io/v1alpha1",
    kind: "Workflow",
    metadata: {
      generateName: `${plan.workflow.name}-`,
      labels: {
        "app.kubernetes.io/name": "massive",
        "massive.dev/workflow": plan.workflow.name,
      },
      annotations: {
        "massive.dev/encoding": plan.encoding,
        "massive.dev/plan-hash": plan.planHash ?? "",
      },
    },
    spec: {
      entrypoint: "main",
      serviceAccountName: "argo",
      templates: [
        {
          name: "main",
          dag: { tasks },
        },
        {
          name: "massive-step",
          container: {
            image: "alpine:3.20",
            command: ["sh", "-c"],
            args: ["echo massive local argo placeholder"],
          },
        },
      ],
    },
  });
}

export async function runArgoLocal<Output>(
  compiled: CompiledWorkflow<Output>,
  inputConfig: { readonly input: unknown }
): Promise<{ readonly manifest: ArgoWorkflowManifest; readonly output: Output }> {
  const manifest = compileArgoWorkflow(compiled);
  return {
    manifest,
    output: await run(compiled, inputConfig),
  };
}
