import { emitWorkflowSpec } from "./emit.ts";
import { resolveWorkflowEntrypoint } from "./resolve.ts";
import { stableStringify } from "./stable.ts";

// Author logs belong on stderr; stdout is the canonical transport only.
console.log = console.error;
console.info = console.error;
const [command, entry, ...extra] = Deno.args;
try {
  if (command !== "emit" || entry === undefined || extra.length !== 0) {
    throw new Error("usage: massive-typescript-frontend emit <entry.ts[#export]>");
  }
  const resolved = await resolveWorkflowEntrypoint(entry);
  const spec = await emitWorkflowSpec(resolved.workflow, {
    source: resolved.source, package: resolved.package,
  });
  await Deno.stdout.write(new TextEncoder().encode(stableStringify(spec)));
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  Deno.exit(2);
}
