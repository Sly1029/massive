import { join } from "node:path";
import { datastore } from "@massive/sdk";
import { z } from "zod";
import {
  EXIT,
  isValidRunId,
  type RunRequest,
  runWorkflow,
  type TargetId,
} from "./run.ts";
import { renderOutcome } from "./report.ts";
import { inspectRun } from "./inspect.ts";

// massive run <entry> [--input <json> | --input-file <path> | -]
//                     [--store <dir>] [--store-prefix <key>] [--project <owner/repo>] [--run-id <id>]
//                     [--target local] [--verbose] [--json] [--rebuild]
// massive inspect <run-id> [--store <dir>] [--store-prefix <key>] [--project <owner/repo>] [--step <id>]

const VALUE_FLAGS = new Set([
  "input",
  "input-file",
  "store",
  "store-prefix",
  "project",
  "run-id",
  "target",
  "step",
]);
const BOOL_FLAGS = new Set(["verbose", "json", "rebuild"]);

interface Parsed {
  readonly command: string;
  readonly positionals: readonly string[];
  readonly values: Readonly<Record<string, string>>;
  readonly bools: ReadonlySet<string>;
}

const encoder = new TextEncoder();
const storePrefixSchema = z.string().min(1).superRefine((prefix, context) => {
  if (prefix.startsWith("/") || prefix.includes("\\")) {
    context.addIssue({
      code: "custom",
      message: "must be a relative forward-slash path",
    });
  }
  if (
    prefix.split("/").some((segment) =>
      segment === "" || segment === "." || segment === ".."
    )
  ) {
    context.addIssue({
      code: "custom",
      message: "must not contain empty, '.' or '..' path segments",
    });
  }
});

async function main(argv: readonly string[]): Promise<number> {
  const parsed = parseArgs(argv);
  if (parsed instanceof Error) return usage(parsed.message);

  if (parsed.command === "run") return await runCommand(parsed);
  if (parsed.command === "inspect") return await inspectCommand(parsed);
  return usage(`unknown command "${parsed.command}" (expected run or inspect)`);
}

async function runCommand(parsed: Parsed): Promise<number> {
  const entry = parsed.positionals[0];
  if (entry === undefined) return usage("run requires a workflow entrypoint");

  const target = parsed.values.target ?? "local";
  if (target !== "local" && target !== "argo") {
    return usage(`unknown --target "${target}" (expected local or argo)`);
  }

  const root = storeRoot(parsed);
  if (root instanceof Error) return usage(root.message);

  let input: Uint8Array;
  try {
    input = await resolveInput(parsed);
  } catch (error) {
    return usage(error instanceof Error ? error.message : String(error));
  }

  const req: RunRequest = {
    entry,
    target: target as TargetId,
    input,
    storeRoot: root,
    ...(parsed.values["run-id"] === undefined
      ? {}
      : { runId: parsed.values["run-id"] }),
    ...(parsed.values.project === undefined
      ? {}
      : { project: parsed.values.project }),
    rebuild: parsed.bools.has("rebuild"),
    verbose: parsed.bools.has("verbose"),
    json: parsed.bools.has("json"),
  };

  const outcome = await runWorkflow(req);
  const rendered = renderOutcome(outcome, {
    verbose: req.verbose,
    json: req.json,
    storeRoot: req.storeRoot,
  });
  await write(Deno.stdout, rendered.stdout);
  await write(Deno.stderr, rendered.stderr);
  return outcome.exitCode;
}

async function inspectCommand(parsed: Parsed): Promise<number> {
  const runId = parsed.positionals[0];
  if (runId === undefined) return usage("inspect requires a run id");
  if (!isValidRunId(runId)) {
    return usage(
      `invalid run id "${runId}" (must be a single path segment, no slashes or "..")`,
    );
  }

  const root = storeRoot(parsed);
  if (root instanceof Error) return usage(root.message);
  const result = await inspectRun(
    {
      runId,
      storeRoot: root,
      ...(parsed.values.project === undefined
        ? {}
        : { project: parsed.values.project }),
      ...(parsed.values.step === undefined ? {} : { step: parsed.values.step }),
      verbose: parsed.bools.has("verbose"),
      json: parsed.bools.has("json"),
    },
    datastore.local({ path: root }),
  );

  if (result.kind === "not-found") {
    await write(
      Deno.stderr,
      `✗ no run "${runId}" in this store\n\n  next  check the run id or --store, then re-run\n`,
    );
    return EXIT.config;
  }
  if (result.kind === "ambiguous") {
    const list = result.candidates.map((dir) => `    ${dir}`).join("\n");
    await write(
      Deno.stderr,
      `✗ run "${runId}" exists under multiple projects:\n${list}\n\n  next  re-run against a store scoped to one project\n`,
    );
    return EXIT.config;
  }
  if (result.kind === "invalid-manifest") {
    await write(
      Deno.stderr,
      `✗ cannot inspect run "${runId}": ${result.message}\n\n  next  upgrade the CLI for this run-manifest protocol or inspect it with a compatible Massive version\n`,
    );
    return EXIT.config;
  }
  await write(Deno.stdout, result.text);
  return EXIT.ok;
}

// --input > --input-file > stdin (`-`) > default literal `null`. The value is
// validated as JSON here (the CLI owns malformed-input errors); the runner
// still validates it against the workflow schema at the step boundary.
async function resolveInput(parsed: Parsed): Promise<Uint8Array> {
  let text: string;
  if (parsed.values.input !== undefined) {
    text = parsed.values.input;
  } else if (parsed.values["input-file"] !== undefined) {
    text = await Deno.readTextFile(parsed.values["input-file"]);
  } else if (parsed.positionals.includes("-")) {
    text = new TextDecoder().decode(await readAll(Deno.stdin));
  } else {
    text = "null";
  }
  try {
    JSON.parse(text);
  } catch {
    throw new Error("--input is not valid JSON");
  }
  return encoder.encode(text);
}

function storeRoot(parsed: Parsed): string | Error {
  const explicit = parsed.values.store ?? Deno.env.get("MASSIVE_STORE");
  const base = explicit !== undefined && explicit !== "" ? explicit : join(
    Deno.env.get("HOME") ?? Deno.env.get("USERPROFILE") ?? ".",
    ".massive",
    "store",
  );
  const cliPrefix = parsed.values["store-prefix"];
  const environmentPrefix = Deno.env.get("MASSIVE_STORE_PREFIX");
  const prefix = cliPrefix ?? environmentPrefix;
  if (prefix === undefined || (cliPrefix === undefined && prefix === "")) {
    return base;
  }
  const validated = storePrefixSchema.safeParse(prefix);
  if (!validated.success) {
    return new Error(
      `invalid storage prefix ${JSON.stringify(prefix)}: ${
        validated.error.issues[0]?.message ?? "invalid value"
      }`,
    );
  }
  return join(base, ...validated.data.split("/"));
}

function parseArgs(argv: readonly string[]): Parsed | Error {
  if (argv.length === 0) return new Error("expected a command: run or inspect");
  const command = argv[0]!;
  const positionals: string[] = [];
  const values: Record<string, string> = {};
  const bools = new Set<string>();

  for (let index = 1; index < argv.length; index++) {
    const token = argv[index]!;
    if (!token.startsWith("--")) {
      positionals.push(token);
      continue;
    }
    const eq = token.indexOf("=");
    const name = eq === -1 ? token.slice(2) : token.slice(2, eq);
    if (BOOL_FLAGS.has(name)) {
      bools.add(name);
      continue;
    }
    if (!VALUE_FLAGS.has(name)) return new Error(`unknown flag --${name}`);
    if (eq !== -1) {
      values[name] = token.slice(eq + 1);
      continue;
    }
    const next = argv[index + 1];
    if (next === undefined) return new Error(`flag --${name} requires a value`);
    values[name] = next;
    index++;
  }

  return { command, positionals, values, bools };
}

function usage(message: string): number {
  Deno.stderr.writeSync(
    encoder.encode(
      `✗ ${message}\n\n  next  see usage: massive run <entry> [--input <json>] [--store <dir>] [--store-prefix <key>]\n`,
    ),
  );
  return EXIT.usage;
}

async function readAll(
  reader: { readable: ReadableStream<Uint8Array> },
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  for await (const chunk of reader.readable) chunks.push(chunk);
  const total = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

async function write(
  writer: { write(bytes: Uint8Array): Promise<number> },
  text: string,
): Promise<void> {
  if (text === "") return;
  await writer.write(encoder.encode(text));
}

Deno.exit(await main(Deno.args));
