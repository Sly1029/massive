import { fileURLToPath } from "node:url";
import { join } from "node:path";

// Repo root, derived from this file's location (packages/cli/test/harness.ts).
export function repoRoot(): string {
  return fileURLToPath(new URL("../../../", import.meta.url));
}

const decoder = new TextDecoder();
function decode(bytes: Uint8Array): string {
  return decoder.decode(bytes);
}

// The CLI is a real Deno program; tests always spawn it as a subprocess (no
// in-process import of its modules) so the process boundary is exercised for
// real. Permissions are broad here because the CLI itself sequences further
// subprocesses (the orchestrator binary + the Deno step runner) and writes to
// temp stores in arbitrary locations.
const CLI_PERMISSIONS = [
  "--allow-read",
  "--allow-write",
  "--allow-run",
  "--allow-env",
  "--allow-sys",
] as const;

// Build the Go orchestrator once per test process and reuse the binary via
// MASSIVE_ORCHESTRATOR_BIN, mirroring the CLI's build-and-cache strategy without
// paying the build cost on every spawn.
let orchestratorBinaryPromise: Promise<string> | undefined;
export function orchestratorBinary(): Promise<string> {
  if (orchestratorBinaryPromise === undefined) {
    orchestratorBinaryPromise = buildOrchestratorBinary();
  }
  return orchestratorBinaryPromise;
}

async function buildOrchestratorBinary(): Promise<string> {
  const dir = await Deno.makeTempDir({ prefix: "massive-orchestrator-bin-" });
  const binary = join(dir, "massive-orchestrator");
  const build = new Deno.Command("go", {
    args: ["build", "-o", binary, "./cmd/massive-orchestrator"],
    cwd: repoRoot(),
    stdout: "piped",
    stderr: "piped",
  });
  const { code, stderr } = await build.output();
  if (code !== 0) {
    throw new Error(`go build massive-orchestrator failed:\n${decode(stderr)}`);
  }
  return binary;
}

export interface CliResult {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

export interface RunCliOptions {
  readonly cwd?: string;
  readonly env?: Record<string, string>;
  // When false, do not build/set MASSIVE_ORCHESTRATOR_BIN (forces the CLI's own
  // toolchain path — used by the missing-`go` preflight test).
  readonly useOrchestratorBinary?: boolean;
}

// Spawns the CLI. Deno is launched by absolute path (Deno.execPath) so that a
// caller overriding PATH (to hide `go`) still starts the CLI, while the CLI's
// own subprocess lookups honor the overridden PATH.
export async function runCli(
  args: readonly string[],
  options: RunCliOptions = {},
): Promise<CliResult> {
  const env: Record<string, string> = { ...options.env };
  if (options.useOrchestratorBinary !== false) {
    env.MASSIVE_ORCHESTRATOR_BIN = await orchestratorBinary();
  }

  const command = new Deno.Command(Deno.execPath(), {
    args: [
      "run",
      ...CLI_PERMISSIONS,
      "--config",
      join(repoRoot(), "deno.json"),
      join(repoRoot(), "packages", "cli", "src", "main.ts"),
      ...args,
    ],
    cwd: options.cwd ?? repoRoot(),
    env,
    stdout: "piped",
    stderr: "piped",
  });
  const { code, stdout, stderr } = await command.output();
  return { code, stdout: decode(stdout), stderr: decode(stderr) };
}

// A fresh temp datastore root. Tests never touch ~/.massive/store.
export function makeStore(): Promise<string> {
  return Deno.makeTempDir({ prefix: "massive-store-" });
}

// Copies a checked-in fixture package into a fresh temp dir so tests can run and
// (for the cache test) mutate the source without dirtying the working tree.
export async function copyFixture(name: string): Promise<string> {
  const source = join(repoRoot(), "packages", "cli", "test", "fixtures", name);
  const destination = await Deno.makeTempDir({
    prefix: `massive-fixture-${name}-`,
  });
  await copyTree(source, destination);
  return destination;
}

async function copyTree(source: string, destination: string): Promise<void> {
  await Deno.mkdir(destination, { recursive: true });
  for await (const entry of Deno.readDir(source)) {
    const from = join(source, entry.name);
    const to = join(destination, entry.name);
    if (entry.isDirectory) {
      await copyTree(from, to);
    } else if (entry.isFile) {
      await Deno.copyFile(from, to);
    }
  }
}

// An empty directory usable as a PATH that hides all real tools (e.g. `go`).
export function makeEmptyPathDir(): Promise<string> {
  return Deno.makeTempDir({ prefix: "massive-empty-path-" });
}

const DATASTORE_METADATA_DIR = ".massive-datastore-metadata";

// Locates a project-scoped run artifact (result.json, run-manifest.json, ...)
// without recomputing the Go-owned project-key normalization: the run id is
// unique, so glob projects/<project-key>/runs/<run-id>/<name>.
export async function findRunArtifact(
  storeRoot: string,
  runId: string,
  name: string,
): Promise<string | undefined> {
  const projects = join(storeRoot, "projects");
  for await (const project of safeReadDir(projects)) {
    if (!project.isDirectory) continue;
    const candidate = join(projects, project.name, "runs", runId, name);
    if (await exists(candidate)) {
      return candidate;
    }
  }
  return undefined;
}

// Relative object keys currently stored (excluding the content-type metadata
// sidecar). Used to snapshot the store and assert `inspect` writes nothing new.
export async function listStoreKeys(storeRoot: string): Promise<string[]> {
  const keys: string[] = [];
  await walk(storeRoot, storeRoot, keys);
  return keys.sort();
}

async function walk(
  root: string,
  current: string,
  keys: string[],
): Promise<void> {
  for await (const entry of safeReadDir(current)) {
    if (entry.isDirectory && entry.name === DATASTORE_METADATA_DIR) {
      continue;
    }
    const child = join(current, entry.name);
    if (entry.isDirectory) {
      await walk(root, child, keys);
    } else if (entry.isFile) {
      keys.push(child.slice(root.length + 1).split("\\").join("/"));
    }
  }
}

// Directories under the given prefix (e.g. specs/ or plans/ dir entries).
export async function listDirEntries(path: string): Promise<string[]> {
  const names: string[] = [];
  for await (const entry of safeReadDir(path)) {
    names.push(entry.name);
  }
  return names.sort();
}

async function* safeReadDir(path: string): AsyncIterable<Deno.DirEntry> {
  try {
    for await (const entry of Deno.readDir(path)) {
      yield entry;
    }
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) {
      return;
    }
    throw error;
  }
}

export async function exists(path: string): Promise<boolean> {
  try {
    await Deno.stat(path);
    return true;
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) {
      return false;
    }
    throw error;
  }
}

// The absolute path to a copied fixture's workflow.ts entrypoint.
export function fixtureEntry(fixtureDir: string): string {
  return join(fixtureDir, "workflow.ts");
}

export { join };
