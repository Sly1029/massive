import { emitWorkflowSpec } from "./emit.ts";
import { resolveWorkflowEntrypoint } from "./resolve.ts";
import { stableStringify } from "./stable.ts";

// The spec travels only through --output; stdout and stderr belong to author code.
const [command, flag, output, entry, ...extra] = Deno.args;
try {
  if (
    command !== "emit" || flag !== "--output" || output === undefined ||
    entry === undefined || extra.length !== 0
  ) {
    throw new Error(
      "usage: massive-typescript-frontend emit --output <spec.json> <entry.ts[#export]>",
    );
  }
  const resolved = await resolveWorkflowEntrypoint(entry);
  const spec = await emitWorkflowSpec(resolved.workflow, {
    source: resolved.source, package: resolved.package,
  });
  await Deno.writeTextFile(output, stableStringify(spec));
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  Deno.exit(2);
}
