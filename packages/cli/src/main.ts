import { join } from "node:path";
import { datastore } from "@massive/sdk";
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
//                     [--store <dir>] [--project <owner/repo>] [--run-id <id>]
//                     [--target local] [--verbose] [--json] [--rebuild]
// massive inspect <run-id> [--store <dir>] [--project <owner/repo>] [--step <id>]

const VALUE_FLAGS = new Set([
  "input",
  "input-file",
  "store",
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
    storeRoot: storeRoot(parsed),
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

function storeRoot(parsed: Parsed): string {
  const explicit = parsed.values.store ?? Deno.env.get("MASSIVE_STORE");
  if (explicit !== undefined && explicit !== "") return explicit;
  const home = Deno.env.get("HOME") ?? Deno.env.get("USERPROFILE") ?? ".";
  return join(home, ".massive", "store");
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
      `✗ ${message}\n\n  next  see usage: massive run <entry> [--input <json>] [--store <dir>]\n`,
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
