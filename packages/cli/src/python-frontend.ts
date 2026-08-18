import { delimiter, dirname, join, resolve } from "node:path";
import {
  MassiveError,
  parseWorkflowSpecText,
  type WorkflowSpec,
} from "@massive/sdk";

export interface PythonWorkflowEmission {
  readonly packageRoot: string;
  readonly spec: WorkflowSpec;
}

const decoder = new TextDecoder();

// The Python frontend is a process seam: Python owns module loading, Pydantic
// models, and canonical emission; the TypeScript CLI owns orchestration. The
// only value crossing that seam is a validated, language-neutral WorkflowSpec.
export async function emitPythonWorkflow(
  entry: string,
  massiveRepoRoot: string,
): Promise<PythonWorkflowEmission> {
  const entryPath = resolve(stripExport(entry));
  if (!entryPath.endsWith(".py")) {
    throw new MassiveError(
      `Python workflow entrypoint must end in .py: ${entry}`,
    );
  }

  const command = await pythonFrontendCommand(massiveRepoRoot);
  const exportSeparator = entry.lastIndexOf("#");
  const frontendEntry = exportSeparator === -1
    ? entryPath
    : `${entryPath}#${entry.slice(exportSeparator + 1)}`;
  let output: Deno.CommandOutput;
  try {
    output = await new Deno.Command(command, {
      args: ["emit", frontendEntry],
      cwd: dirname(entryPath),
      stdout: "piped",
      stderr: "piped",
    }).output();
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) {
      throw new MassiveError(
        "Python workflow support requires the massive-python-frontend executable; install the Massive Python SDK in the active environment",
      );
    }
    throw error;
  }

  if (output.code !== 0) {
    const diagnostic = decoder.decode(output.stderr).trim();
    throw new MassiveError(
      diagnostic === ""
        ? `Python workflow frontend exited with code ${output.code}`
        : diagnostic,
    );
  }

  try {
    return {
      packageRoot: dirname(entryPath),
      spec: await parseWorkflowSpecText(decoder.decode(output.stdout)),
    };
  } catch (error) {
    throw new MassiveError(
      `Python workflow frontend emitted an invalid WorkflowSpec: ${
        error instanceof Error ? error.message : String(error)
      }`,
    );
  }
}

export async function pythonRunnerEnvironment(
  massiveRepoRoot: string,
  spec: WorkflowSpec,
): Promise<Record<string, string> | undefined> {
  const needsPython = Object.values(spec.sourcePackages).some(
    (sourcePackage) => sourcePackage.language === "python",
  );
  if (!needsPython) return undefined;

  const localBin = pythonEnvironmentBin(massiveRepoRoot);
  const localRunner = join(
    localBin,
    Deno.build.os === "windows"
      ? "massive-python-runner.exe"
      : "massive-python-runner",
  );
  if (!(await exists(localRunner))) return undefined;
  const path = Deno.env.get("PATH");
  return {
    PATH: path === undefined || path === ""
      ? localBin
      : `${localBin}${delimiter}${path}`,
  };
}

async function pythonFrontendCommand(massiveRepoRoot: string): Promise<string> {
  const local = join(
    pythonEnvironmentBin(massiveRepoRoot),
    Deno.build.os === "windows"
      ? "massive-python-frontend.exe"
      : "massive-python-frontend",
  );
  return await exists(local) ? local : "massive-python-frontend";
}

function pythonEnvironmentBin(massiveRepoRoot: string): string {
  return join(
    massiveRepoRoot,
    "packages",
    "python",
    ".venv",
    Deno.build.os === "windows" ? "Scripts" : "bin",
  );
}

function stripExport(entry: string): string {
  const hash = entry.lastIndexOf("#");
  return hash === -1 ? entry : entry.slice(0, hash);
}

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.stat(path);
    return true;
  } catch {
    return false;
  }
}
