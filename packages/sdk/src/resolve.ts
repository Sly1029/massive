import {
  basename,
  dirname,
  isAbsolute,
  relative,
  resolve,
  sep,
} from "node:path";
import { pathToFileURL } from "node:url";
import { z } from "zod";
import type { WorkflowPackageConfig, WorkflowSpecTarget } from "./config.ts";
import { defineWorkflowPackage } from "./config.ts";
import { MassiveError } from "./errors.ts";
import type { EmitSourceSpec } from "./emit.ts";
import { WorkflowBuilder } from "./workflow.ts";

export interface ResolveWorkflowEntrypointOptions {
  readonly target?: WorkflowSpecTarget["kind"];
}

export interface ResolvedWorkflowEntrypoint {
  readonly workflow: WorkflowBuilder<unknown, unknown>;
  readonly selectedExport: string;
  readonly packageRoot: string;
  readonly package: WorkflowPackageConfig;
  readonly source: EmitSourceSpec;
  readonly explicitConfig: boolean;
}

export async function resolveWorkflowEntrypoint(
  specifier: string,
  options: ResolveWorkflowEntrypointOptions = {},
): Promise<ResolvedWorkflowEntrypoint> {
  const parsed = parseEntrypoint(specifier);
  const path = resolve(parsed.path);

  if (await exists(resolve(path, "massive.config.ts"))) {
    const packageRoot = path;
    const workflowPackage = await loadWorkflowPackageConfig(
      resolve(packageRoot, "massive.config.ts"),
    );
    const packageEntrypoint = parseEntrypoint(workflowPackage.entrypoint);
    const entrypointPath = resolve(packageRoot, packageEntrypoint.path);
    const selected = await selectWorkflowExport(
      entrypointPath,
      packageEntrypoint.exportName,
    );

    return {
      workflow: selected.workflow,
      selectedExport: selected.exportName,
      packageRoot,
      package: workflowPackage,
      source: {
        root: packageRoot,
        include: workflowPackage.include,
        module: relativeModulePath(packageRoot, entrypointPath),
      },
      explicitConfig: true,
    };
  }

  const configPath = await findNearestConfig(dirname(path));
  if (configPath !== undefined) {
    const packageRoot = dirname(configPath);
    const workflowPackage = await loadWorkflowPackageConfig(configPath);
    const selected = await selectWorkflowExport(path, parsed.exportName);
    return {
      workflow: selected.workflow,
      selectedExport: selected.exportName,
      packageRoot,
      package: {
        ...workflowPackage,
        entrypoint: `${relativeModulePath(packageRoot, path)}#${
          selected.exportName
        }`,
      },
      source: {
        root: packageRoot,
        include: workflowPackage.include,
        module: relativeModulePath(packageRoot, path),
      },
      explicitConfig: true,
    };
  }

  if (options.target !== undefined && options.target !== "local") {
    throw new MassiveError(
      `Zero-config workflows only support the local target; target "${options.target}" requires massive.config.ts`,
    );
  }

  const packageRoot = dirname(path);
  const selected = await selectWorkflowExport(path, parsed.exportName);
  const include = [basename(path), ...await nearbyPackageFiles(packageRoot)];
  const workflowPackage = defineWorkflowPackage({
    entrypoint: `./${basename(path)}#${selected.exportName}`,
    include,
    targets: [{ kind: "local" }],
  });

  return {
    workflow: selected.workflow,
    selectedExport: selected.exportName,
    packageRoot,
    package: workflowPackage,
    source: {
      root: packageRoot,
      include,
      module: `./${basename(path)}`,
    },
    explicitConfig: false,
  };
}

const WorkflowPackageConfigSchema = z.object({
  projectId: z.string().min(1).optional(),
  include: z.array(z.string().min(1)),
  entrypoint: z.string().min(1),
  environment: z.union([
    z.object({ kind: z.literal("container") }).loose(),
    z.object({ kind: z.literal("node") }).loose(),
  ]).optional(),
  targets: z.array(
    z.union([
      z.object({ kind: z.literal("local") }),
      z.object({ kind: z.literal("argo") }).loose(),
    ]),
  ).optional(),
});

async function loadWorkflowPackageConfig(
  configPath: string,
): Promise<WorkflowPackageConfig> {
  const module = await import(pathToFileURL(configPath).href);
  const parsed = WorkflowPackageConfigSchema.safeParse(module.default);
  if (!parsed.success) {
    throw new MassiveError(
      `Invalid massive.config.ts at ${configPath}: ${z.prettifyError(parsed.error)}`,
    );
  }
  return module.default as WorkflowPackageConfig;
}

async function selectWorkflowExport(
  filePath: string,
  requestedExport: string | undefined,
): Promise<{
  readonly workflow: WorkflowBuilder<unknown, unknown>;
  readonly exportName: string;
}> {
  const module = await import(pathToFileURL(filePath).href);
  if (requestedExport !== undefined) {
    const selected = module[requestedExport] as unknown;
    if (selected instanceof WorkflowBuilder) {
      return { workflow: selected, exportName: requestedExport };
    }
    throw new MassiveError(
      `Workflow entrypoint "${filePath}#${requestedExport}" does not export a workflow`,
    );
  }

  if (module.default instanceof WorkflowBuilder) {
    return { workflow: module.default, exportName: "default" };
  }

  const candidates = Object.entries(module)
    .filter(([, value]) => value instanceof WorkflowBuilder)
    .map(([name]) => name)
    .sort();
  if (candidates.length === 1) {
    return {
      workflow: module[candidates[0]!] as WorkflowBuilder<unknown, unknown>,
      exportName: candidates[0]!,
    };
  }
  if (candidates.length > 1) {
    throw new MassiveError(
      `Workflow entrypoint "${filePath}" is ambiguous; specify one of: ${
        candidates.join(", ")
      }`,
    );
  }

  throw new MassiveError(`Workflow entrypoint "${filePath}" exports no workflows`);
}

function parseEntrypoint(specifier: string): {
  readonly path: string;
  readonly exportName?: string;
} {
  const hashIndex = specifier.lastIndexOf("#");
  if (hashIndex === -1) {
    return { path: specifier };
  }
  return {
    path: specifier.slice(0, hashIndex),
    exportName: specifier.slice(hashIndex + 1),
  };
}

async function findNearestConfig(start: string): Promise<string | undefined> {
  let current = resolve(start);
  while (true) {
    const candidate = resolve(current, "massive.config.ts");
    if (await exists(candidate)) {
      return candidate;
    }

    const next = dirname(current);
    if (next === current) {
      return undefined;
    }
    current = next;
  }
}

async function nearbyPackageFiles(root: string): Promise<string[]> {
  const files = ["package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock"];
  const present: string[] = [];
  for (const file of files) {
    if (await exists(resolve(root, file))) {
      present.push(file);
    }
  }
  return present;
}

async function exists(path: string): Promise<boolean> {
  const deno = (globalThis as {
    readonly Deno?: { readonly stat: (path: string) => Promise<unknown> };
  }).Deno;
  try {
    if (deno !== undefined) {
      await deno.stat(path);
    } else {
      const { access } = await import("node:fs/promises");
      await access(path);
    }
    return true;
  } catch {
    return false;
  }
}

function relativeModulePath(root: string, filePath: string): string {
  const relativePath = relative(root, filePath);
  if (
    relativePath === "" || relativePath.startsWith(`..${sep}`) ||
    isAbsolute(relativePath)
  ) {
    throw new MassiveError(
      `Workflow entrypoint must be inside package root: ${filePath}`,
    );
  }

  return `./${relativePath.split(sep).join("/")}`;
}
