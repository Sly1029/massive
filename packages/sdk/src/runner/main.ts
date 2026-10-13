import { readFile, rename, writeFile } from "node:fs/promises";
import { parseStepInvocationDescriptorText } from "./descriptor.ts";
import { executeStep } from "./execute.ts";
import {
  DescriptorError,
  descriptorResolutionFailure,
  formatOutcomeDiagnostic,
  type StepOutcome,
} from "./outcomes.ts";

interface DenoRuntime {
  readonly args: readonly string[];
  exit(code?: number): never;
}

export async function runStep(descriptorPath: string): Promise<StepOutcome> {
  let descriptor;
  try {
    descriptor = await parseStepInvocationDescriptorText(
      await readFile(descriptorPath, "utf8"),
    );
  } catch (error) {
    if (error instanceof DescriptorError) {
      return descriptorResolutionFailure(error);
    }

    const message = error instanceof Error ? error.message : String(error);
    return descriptorResolutionFailure(
      new DescriptorError(
        `could not read descriptor ${descriptorPath}: ${message}`,
      ),
    );
  }
  return executeStep(descriptor);
}

// Intended package bin wiring once packaging is ready:
// "massive-step-runner": "packages/sdk/src/runner/main.ts"
const deno =
  (globalThis as typeof globalThis & { readonly Deno?: DenoRuntime }).Deno;
if (
  (import.meta as ImportMeta & { readonly main?: boolean }).main === true &&
  deno !== undefined
) {
  const descriptorPath = deno.args[0];
  const outcome = descriptorPath === undefined
    ? descriptorResolutionFailure(
      new DescriptorError("missing descriptor path argument"),
    )
    : await runStep(descriptorPath);

  if (outcome.kind !== "success") {
    if (descriptorPath !== undefined) {
      try {
        const path = descriptorPath + ".failure.json";
        const record = JSON.stringify({
          exitCode: outcome.exitCode,
          ...(outcome.kind === "fatal-run-failure"
            ? { category: outcome.error.category }
            : {}),
          message: Array.from(
            outcome.kind === "fatal-run-failure"
              ? outcome.error.message
              : `${outcome.error.name}: ${outcome.error.message}`,
          ).slice(0, 1024).join(""),
        }).replace(
          /[\u007f-\uffff]/g,
          (char) => `\\u${char.charCodeAt(0).toString(16).padStart(4, "0")}`,
        );
        await writeFile(path + ".tmp", record);
        await rename(path + ".tmp", path);
      } catch (error) {
        console.error(`runner-result-publication-failure: ${error}`);
        deno.exit(70);
      }
    }
    console.error(formatOutcomeDiagnostic(outcome));
  }

  deno.exit(outcome.exitCode);
}
