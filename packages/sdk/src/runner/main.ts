import { readFile } from "node:fs/promises";
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
  try {
    const descriptor = await parseStepInvocationDescriptorText(
      await readFile(descriptorPath, "utf8"),
    );
    return await executeStep(descriptor);
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
    console.error(formatOutcomeDiagnostic(outcome));
  }

  deno.exit(outcome.exitCode);
}
