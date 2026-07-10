import { dirname, relative, resolve as resolvePath, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { stat } from "node:fs/promises";
import {
  computeSpecHash,
  type Datastore,
  datastore,
  emitWorkflowSpec,
  MassiveError,
  parseWorkflowPackageConfig,
  resolveWorkflowEntrypoint,
  type WorkflowPackageConfig,
  type WorkflowSpec,
  type WorkflowSpecTarget,
} from "@massive/sdk";
import { hashSourcePackage } from "../../sdk/src/source-package.ts";
import { sha256Text, stableStringify } from "../../sdk/src/stable.ts";
import { validateObjectKey } from "../../sdk/src/datastore/key.ts";
import { RUNNER_EXIT_CODES } from "../../sdk/src/runner/outcomes.ts";
import { createToolchain, ToolchainMissingError } from "./toolchain.ts";

// A malformed massive.config.ts shape discovered on the emit fast path. Kept
// distinct from MassiveError (which maps to resolve-failed) so runWorkflow can
// map it to the config-error exit code rather than a resolve failure.
export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

// Pre-run exit categories owned by the CLI/SDK/Go layers. Step-failure exit
// codes are the runner's own (64/65/66) and are propagated unchanged, so the
// CLI's exit code *is* the runner outcome rather than a re-invented space.
export const EXIT = {
  ok: 0,
  // A run was created (or attempted) but failed for a reason not attributable
  // to a specific step outcome — a run-level orchestrator error, or an
  // orchestrator that exited without emitting a parseable run object.
  run: 1,
  usage: 2,
  resolve: 3,
  config: 4,
  compile: 5,
  toolchain: 70,
} as const;

// Step-failure exit codes are the runner's own non-zero codes (64/65/66); 0 is
// excluded because a success is never reported through a step-failed outcome.
export type RunnerExit = Exclude<
  (typeof RUNNER_EXIT_CODES)[keyof typeof RUNNER_EXIT_CODES],
  0
>;
export type TargetId = WorkflowSpecTarget["kind"];

// Project identity is never a bare string downstream: the CLI resolves it and
// always passes --project to the orchestrator so failures surface here with an
// actionable message. "derived" is the zero-config ephemeral id used outside a
// git repo (`local/` + first 12 hex of sha256(absolute packageRoot)); a
// deployable target request is refused before that id is ever used.
export type ProjectIdentity =
  | { readonly kind: "configured"; readonly projectId: string }
  | { readonly kind: "git-origin"; readonly ownerRepo: string }
  | { readonly kind: "derived"; readonly projectId: string };

export interface RunRequest {
  readonly entry: string;
  readonly target: TargetId;
  readonly input: Uint8Array; // raw JSON bytes; default is the literal `null`
  readonly storeRoot: string;
  readonly runId?: string;
  readonly project?: string;
  readonly rebuild: boolean;
  readonly verbose: boolean;
  readonly json: boolean;
}

export interface Emitted {
  readonly spec: WorkflowSpec;
  readonly specKey: string; // object key: specs/<spec-key>/workflow-spec.json
  readonly specReused: boolean;
  readonly project: ProjectIdentity;
  readonly packageRoot: string;
  // Verbose-only diagnostics accrued during emit (e.g. a self-healed cache
  // pointer). Surfaced under --verbose; empty on the common path.
  readonly notes: readonly string[];
}

export interface StepSummary {
  readonly nodeId: string;
  readonly status: "succeeded" | "failed" | "pending";
  readonly diagnostic?: string;
  readonly outputKey?: string;
}

export type RunOutcome =
  | {
    readonly kind: "succeeded";
    readonly exitCode: typeof EXIT.ok;
    readonly runId: string;
    readonly resultKey: string;
    readonly result: unknown;
    readonly steps: readonly StepSummary[];
    readonly specReused: boolean;
    readonly planReused: boolean;
    readonly specHash: string;
    readonly planHash: string;
    readonly notes: readonly string[];
  }
  | {
    readonly kind: "step-failed";
    readonly exitCode: RunnerExit;
    readonly runId: string;
    readonly failed: StepSummary;
    readonly steps: readonly StepSummary[];
    // Orchestrator stderr tail, when it carried a diagnostic beyond the
    // per-step outcome.
    readonly stderrTail?: string;
  }
  | {
    // A run was created (or the orchestrator was reached) but failed for a
    // reason not attributable to a specific step: a run-level orchestrator
    // error, or unparseable orchestrator output that is not an identifiable
    // spec/plan diagnostic.
    readonly kind: "run-failed";
    readonly exitCode: typeof EXIT.run;
    readonly runId?: string;
    readonly diagnostic: string;
    readonly steps?: readonly StepSummary[];
  }
  | {
    readonly kind: "compile-rejected";
    readonly exitCode: typeof EXIT.compile;
    readonly diagnostics: readonly string[];
  }
  | {
    readonly kind: "resolve-failed";
    readonly exitCode: typeof EXIT.resolve;
    readonly message: string;
  }
  | {
    readonly kind: "config-error";
    readonly exitCode: typeof EXIT.config;
    readonly message: string;
  }
  | {
    readonly kind: "toolchain-missing";
    readonly exitCode: typeof EXIT.toolchain;
    readonly tool: "go" | "deno";
  };

const decoder = new TextDecoder();

// Pipe of ensure-toolchain -> resolve/emit (with cache) -> orchestrate.
// Total: every path returns a RunOutcome. A malformed config maps to
// config-error; any other unexpected throw becomes run-failed rather than an
// uncaught stack trace, so the exit-code contract is exhaustive.
export async function runWorkflow(req: RunRequest): Promise<RunOutcome> {
  try {
    return await executeRun(req);
  } catch (error) {
    if (error instanceof ConfigError) {
      return {
        kind: "config-error",
        exitCode: EXIT.config,
        message: error.message,
      };
    }
    return {
      kind: "run-failed",
      exitCode: EXIT.run,
      diagnostic: error instanceof Error ? error.message : String(error),
    };
  }
}

async function executeRun(req: RunRequest): Promise<RunOutcome> {
  const root = repoRoot();
  let binary: string;
  try {
    binary = await createToolchain(root).ensure(req.rebuild);
  } catch (error) {
    if (error instanceof ToolchainMissingError) {
      return {
        kind: "toolchain-missing",
        exitCode: EXIT.toolchain,
        tool: error.tool,
      };
    }
    throw error;
  }

  const store = datastore.local({ path: req.storeRoot });
  let emitted: Emitted;
  try {
    emitted = await prepare(req, store);
  } catch (error) {
    if (error instanceof MassiveError) {
      return {
        kind: "resolve-failed",
        exitCode: EXIT.resolve,
        message: error.message,
      };
    }
    // ConfigError and any unexpected error bubble to runWorkflow's catch-all.
    throw error;
  }

  if (req.target !== "local") {
    // A configured deployable target resolves but has no local execution path in
    // M1 (zero-config argo is already refused by the SDK during prepare).
    return {
      kind: "config-error",
      exitCode: EXIT.config,
      message:
        `target "${req.target}" cannot be executed locally in this milestone`,
    };
  }

  return await orchestrate(emitted, req, store, binary, root);
}

// Resolve the entrypoint, emit its spec, and persist it — reusing a cached spec
// (and skipping the workflow-module import entirely) when the source package is
// unchanged. Emit is in-process; the source-package hash is the cache key.
async function prepare(req: RunRequest, store: Datastore): Promise<Emitted> {
  const cfg = await resolveSourceConfig(req.entry);
  const notes: string[] = [];
  let cacheKey: string | undefined;
  let identityHash: string | undefined;
  if (cfg !== undefined) {
    const source = await hashSourcePackage(cfg.source);
    // The identity hash keys the emit cache on everything that determines the
    // emitted spec: source content, targets, the RESOLVED entrypoint identity
    // (module + export, so two workflows in one package do not collide), and the
    // EVALUATED config (so an edit to an imported settings module is a miss even
    // though the config file bytes are unchanged). Derived statically — the
    // workflow module is never imported on a hit.
    const entry = staticEntryIdentity(req.entry, cfg.packageRoot);
    identityHash = emitIdentityHash(
      source.sourcePackageHash,
      cfg.targets,
      entry,
      cfg.configHash,
    );
    cacheKey = `cache/emit/${segmentForHash(identityHash)}.json`;
    const reused = await readCachedSpec(store, cacheKey, identityHash);
    if (reused !== undefined) {
      return {
        spec: reused.spec,
        specKey: reused.specKey,
        specReused: true,
        project: await resolveProject(req, cfg.packageRoot, cfg.projectId),
        packageRoot: cfg.packageRoot,
        notes,
      };
    }
    if (await store.exists(cacheKey)) {
      // The pointer existed but could not be honored (missing/corrupt/mismatched
      // spec, or a pointer bound to a different identity): fall through and
      // re-emit, self-healing the cache entry.
      notes.push("emit cache invalid; re-emitting");
    }
  }

  // Cache miss (or zero-config): import the workflow module and emit its spec.
  const resolved = await resolveWorkflowEntrypoint(req.entry, {
    target: req.target,
  });
  const spec = await emitWorkflowSpec(resolved.workflow, {
    source: resolved.source,
    package: resolved.package,
  });
  const specKey = `specs/${segmentForHash(spec.specHash)}/workflow-spec.json`;
  await store.put(specKey, JSON.stringify(spec), {
    contentType: "application/json",
  });
  if (cacheKey !== undefined && identityHash !== undefined) {
    // The pointer records the identity it was emitted for, so a later read can
    // reject a pointer that has been repointed at a different spec.
    await store.put(
      cacheKey,
      JSON.stringify({ specKey, identity: identityHash }),
      { contentType: "application/json" },
    );
  }
  return {
    spec,
    specKey,
    specReused: false,
    project: await resolveProject(
      req,
      resolved.packageRoot,
      resolved.package.projectId,
    ),
    packageRoot: resolved.packageRoot,
    notes,
  };
}

// Reads the cached spec a pointer references, or undefined when the cache entry
// cannot be honored. It verifies three things before trusting a hit, so neither
// a corrupt/stale pointer nor a pointer repointed at a DIFFERENT valid spec is
// silently executed:
//   1. identity binding — the pointer records the identity hash it was emitted
//      for; it must equal this run's identity (else it points at another spec).
//   2. content integrity — the loaded spec must hash (specHash-excluded, per the
//      SDK's own rule) to its recorded specHash.
//   3. key binding — the pointer's target key must be the content-addressed key
//      for that specHash.
// ANY failure (missing pointer/spec, parse error, bad key, any mismatch) is
// treated as a miss so the caller re-emits; a corrupt cache never crashes a run.
async function readCachedSpec(
  store: Datastore,
  cacheKey: string,
  identityHash: string,
): Promise<{ spec: WorkflowSpec; specKey: string } | undefined> {
  try {
    if (!(await store.exists(cacheKey))) return undefined;
    const pointer = JSON.parse(decoder.decode(await store.get(cacheKey))) as {
      readonly specKey?: unknown;
      readonly identity?: unknown;
    };
    if (pointer.identity !== identityHash) return undefined;
    if (typeof pointer.specKey !== "string") return undefined;
    const specKey = pointer.specKey;
    const spec = JSON.parse(
      decoder.decode(await store.get(specKey)),
    ) as WorkflowSpec;
    if (typeof spec.specHash !== "string" || spec.specHash === "") {
      return undefined;
    }
    if (computeSpecHash(spec) !== spec.specHash) return undefined;
    if (
      `specs/${segmentForHash(spec.specHash)}/workflow-spec.json` !== specKey
    ) {
      return undefined;
    }
    return { spec, specKey };
  } catch {
    return undefined;
  }
}

// The entrypoint identity used in the emit cache key: the module path relative
// to the package root and the selected export ("default" when unspecified).
// Derived purely from the entry string + package root, so a cache hit never
// imports the workflow module. On a miss the authoritative
// resolveWorkflowEntrypoint runs and produces the real spec, so any drift here
// only forgoes a hit — it can never key a run to the wrong spec, because the
// source-package hash already pins the module contents.
function staticEntryIdentity(
  entry: string,
  packageRoot: string,
): { readonly module: string; readonly export: string } {
  const hash = entry.lastIndexOf("#");
  const path = hash === -1 ? entry : entry.slice(0, hash);
  const exportName = hash === -1 ? "default" : entry.slice(hash + 1);
  const module = relative(packageRoot, resolvePath(path)).split(sep).join("/");
  return { module, export: exportName };
}

// Write the spec to a handoff file outside the package tree, then drive the
// prebuilt Go orchestrator with --source-root/--json and read the authoritative
// run manifest back through the datastore.
async function orchestrate(
  emitted: Emitted,
  req: RunRequest,
  store: Datastore,
  binary: string,
  root: string,
): Promise<RunOutcome> {
  const handoffDir = await Deno.makeTempDir({ prefix: "massive-spec-" });
  try {
    return await runOrchestrator(emitted, req, store, binary, root, handoffDir);
  } finally {
    // The handoff spec is only needed for the orchestrator's lifetime; remove
    // the temp dir regardless of how the run ended so it is not leaked.
    await Deno.remove(handoffDir, { recursive: true }).catch(() => {});
  }
}

async function runOrchestrator(
  emitted: Emitted,
  req: RunRequest,
  store: Datastore,
  binary: string,
  root: string,
  handoffDir: string,
): Promise<RunOutcome> {
  const specFile = `${handoffDir}/workflow-spec.json`;
  await Deno.writeTextFile(specFile, JSON.stringify(emitted.spec));

  const args = [
    "run",
    "--spec",
    specFile,
    "--store",
    req.storeRoot,
    "--project",
    projectString(emitted.project),
    "--source-root",
    emitted.packageRoot,
    "--json",
    "--input",
    decoder.decode(req.input),
  ];
  if (req.runId !== undefined) {
    args.push("--run-id", req.runId);
  }

  const { code, stdout, stderr } = await new Deno.Command(binary, {
    args,
    cwd: root,
    stdout: "piped",
    stderr: "piped",
  }).output();

  const stderrText = decoder.decode(stderr);
  const parsed = parseRunJson(decoder.decode(stdout));
  if (parsed === undefined) {
    // No run object on stdout. Only an identifiable spec/plan diagnostic is a
    // compile rejection (exit 5); any other unparseable output is a run-level
    // failure (exit 1) carrying the orchestrator's stderr.
    if (isSpecCompileRejection(stderrText)) {
      return {
        kind: "compile-rejected",
        exitCode: EXIT.compile,
        diagnostics: splitDiagnostics(stderrText),
      };
    }
    return {
      kind: "run-failed",
      exitCode: EXIT.run,
      diagnostic: stderrTail(stderrText),
    };
  }

  // Locate the manifest by deriving it from the orchestrator's own resultKey
  // (its sibling), never by scanning projects/ — the resultKey embeds the exact
  // Go-normalized project key, so there is no ambiguity and nothing to guess.
  // On a failure with no resultKey the per-step statuses come from the run JSON.
  const manifestKey = parsed.resultKey === undefined || parsed.resultKey === ""
    ? undefined
    : manifestKeyFromResultKey(parsed.resultKey);
  const manifest = manifestKey === undefined
    ? undefined
    : await readManifestAt(store, manifestKey);
  const steps = buildSteps(parsed, manifest);

  if (code === 0 && parsed.status === "succeeded") {
    const resultKey = parsed.resultKey ?? manifest?.result?.key ?? "";
    const result = resultKey === ""
      ? null
      : JSON.parse(decoder.decode(await store.get(resultKey)));
    return {
      kind: "succeeded",
      exitCode: EXIT.ok,
      runId: parsed.runId,
      resultKey,
      result,
      steps,
      specReused: emitted.specReused,
      // Same spec ⇒ same plan; a reused spec means the plan artifact was already
      // compiled and persisted by an earlier run.
      planReused: emitted.specReused,
      specHash: emitted.spec.specHash,
      planHash: manifest?.planHash ?? "",
      notes: emitted.notes,
    };
  }

  // Only classify a step failure when a step actually failed; otherwise a
  // run-level orchestrator error must not be misattributed to a succeeded step.
  const failed = steps.find((step) => step.status === "failed");
  const tail = stderrTail(stderrText);
  if (failed !== undefined) {
    const exitCode = runnerExitFromDiagnostic(failed.diagnostic) ??
      RUNNER_EXIT_CODES.stepExecutionFailure;
    return {
      kind: "step-failed",
      exitCode,
      runId: parsed.runId,
      failed,
      steps,
      ...(tail === "" ? {} : { stderrTail: tail }),
    };
  }
  return {
    kind: "run-failed",
    exitCode: EXIT.run,
    runId: parsed.runId,
    diagnostic: tail,
    steps,
  };
}

// --- Project identity ------------------------------------------------------

async function resolveProject(
  req: RunRequest,
  packageRoot: string,
  configProjectId: string | undefined,
): Promise<ProjectIdentity> {
  if (req.project !== undefined && req.project !== "") {
    return { kind: "configured", projectId: req.project };
  }
  if (configProjectId !== undefined && configProjectId !== "") {
    return { kind: "configured", projectId: configProjectId };
  }
  const origin = await gitOrigin(packageRoot);
  if (origin !== undefined) {
    return { kind: "git-origin", ownerRepo: origin };
  }
  const digest = sha256Text(resolvePath(packageRoot)).slice(0, 12);
  return { kind: "derived", projectId: `local/${digest}` };
}

export function projectString(project: ProjectIdentity): string {
  return project.kind === "git-origin" ? project.ownerRepo : project.projectId;
}

const HTTPS_REMOTE =
  /^https:\/\/(?:github|gitlab)\.com\/([^/]+)\/([^/]+?)(?:\.git)?\/?$/;
const SSH_REMOTE = /^git@(?:github|gitlab)\.com:([^/]+)\/([^/]+?)(?:\.git)?$/;

async function gitOrigin(packageRoot: string): Promise<string | undefined> {
  try {
    const { code, stdout } = await new Deno.Command("git", {
      args: ["-C", packageRoot, "config", "--get", "remote.origin.url"],
      stdout: "piped",
      stderr: "null",
    }).output();
    if (code !== 0) return undefined;
    const origin = decoder.decode(stdout).trim();
    const match = HTTPS_REMOTE.exec(origin) ?? SSH_REMOTE.exec(origin);
    return match === null ? undefined : `${match[1]}/${match[2]}`;
  } catch {
    return undefined;
  }
}

// --- Source-config fast path (no workflow-module import) -------------------

interface SourceConfig {
  readonly packageRoot: string;
  readonly source: {
    readonly root: string;
    readonly include: readonly string[];
  };
  readonly targets: readonly WorkflowSpecTarget[];
  readonly projectId?: string;
  // sha256 (hex) of the EVALUATED, spec-relevant config fields (entrypoint,
  // include, environment, targets), folded into the emit cache key so a config
  // change invalidates a cached spec even when the config file is not covered by
  // the source `include` globs — including edits that flow in via an imported
  // settings module without changing the config file bytes.
  readonly configHash: string;
}

// Loads massive.config.ts to derive the source spec + targets WITHOUT importing
// the workflow module, so a cache hit can be decided before paying the import
// cost. Returns undefined for zero-config packages (no cache fast path); the
// authoritative resolveWorkflowEntrypoint still runs on every cache miss, so
// any drift from its config discovery only forgoes a cache hit, never
// correctness. Mirrors resolve.ts's nearest-config discovery.
async function resolveSourceConfig(
  entry: string,
): Promise<SourceConfig | undefined> {
  const path = resolvePath(stripExport(entry));
  const configPath = await findNearestConfig(dirname(path));
  if (configPath === undefined) return undefined;
  const packageRoot = dirname(configPath);
  const raw = (await import(pathToFileURL(configPath).href)).default;
  // Validate the evaluated config with the SDK's own schema so a malformed shape
  // surfaces as a config-error (exit 4) here instead of throwing a TypeError
  // deeper in. ConfigError is distinct from MassiveError (resolve-failed).
  let config: WorkflowPackageConfig;
  try {
    config = parseWorkflowPackageConfig(raw, configPath);
  } catch (error) {
    throw new ConfigError(
      error instanceof Error ? error.message : String(error),
    );
  }
  // Hash the EVALUATED, spec-relevant config fields — not the file bytes — so an
  // edit to an imported settings module (which changes these values without
  // changing the config file) is a cache miss, while a dynamic-but-equal config
  // still hits.
  const configHash = sha256Text(stableStringify({
    entrypoint: config.entrypoint,
    include: config.include,
    environment: config.environment,
    targets: config.targets,
  }));
  return {
    packageRoot,
    source: { root: packageRoot, include: config.include },
    targets: config.targets ?? [{ kind: "local" }],
    projectId: config.projectId,
    configHash,
  };
}

async function findNearestConfig(start: string): Promise<string | undefined> {
  let current = resolvePath(start);
  while (true) {
    const candidate = resolvePath(current, "massive.config.ts");
    if (await pathExists(candidate)) return candidate;
    const next = dirname(current);
    if (next === current) return undefined;
    current = next;
  }
}

function stripExport(specifier: string): string {
  const hash = specifier.lastIndexOf("#");
  return hash === -1 ? specifier : specifier.slice(0, hash);
}

// --- Run manifest read (authoritative) -------------------------------------

interface ManifestView {
  readonly planHash: string;
  readonly status: string;
  readonly steps: readonly {
    readonly nodeId: string;
    readonly status: string;
    readonly attempts?: readonly {
      readonly output?: { readonly key: string; readonly hash: string };
      readonly diagnostic?: string;
    }[];
  }[];
  readonly result?: { readonly key: string; readonly hash: string };
}

// Reads a manifest at a known key; undefined on any failure (best-effort — the
// caller degrades to the run JSON for step statuses).
async function readManifestAt(
  store: Datastore,
  key: string,
): Promise<ManifestView | undefined> {
  try {
    return JSON.parse(decoder.decode(await store.get(key))) as ManifestView;
  } catch {
    return undefined;
  }
}

// The run manifest is the sibling of the result artifact under the same run
// directory (datastore-layout.md: projects/<key>/runs/<id>/{result,run-manifest}.json).
function manifestKeyFromResultKey(resultKey: string): string {
  const slash = resultKey.lastIndexOf("/");
  return slash === -1
    ? "run-manifest.json"
    : `${resultKey.slice(0, slash)}/run-manifest.json`;
}

// Lists every run-manifest key matching a run id across projects, without
// recomputing the Go-owned project-key normalization. inspect uses this to
// disambiguate: exactly one match is returned unambiguously; multiple matches
// (the same run id under different projects) are surfaced to the caller rather
// than silently resolving to the first.
export async function findRunManifestKeys(
  storeRoot: string,
  runId: string,
): Promise<string[]> {
  const projectsDir = `${storeRoot}/projects`;
  const keys: string[] = [];
  try {
    for await (const entry of Deno.readDir(projectsDir)) {
      if (!entry.isDirectory) continue;
      const key = `projects/${entry.name}/runs/${runId}/run-manifest.json`;
      if (await pathExists(`${storeRoot}/${key}`)) keys.push(key);
    }
  } catch (error) {
    if (!(error instanceof Deno.errors.NotFound)) throw error;
  }
  return keys.sort();
}

function buildSteps(
  parsed: RunJson,
  manifest: ManifestView | undefined,
): StepSummary[] {
  return parsed.steps.map((step) => {
    const attempt = manifest?.steps.find((entry) =>
      entry.nodeId === step.nodeId
    )?.attempts?.[0];
    const diagnostic = step.diagnostic !== undefined && step.diagnostic !== ""
      ? step.diagnostic
      : attempt?.diagnostic;
    const summary: StepSummary = {
      nodeId: step.nodeId,
      status: normalizeStatus(step.status),
      ...(diagnostic === undefined || diagnostic === "" ? {} : { diagnostic }),
      ...(attempt?.output === undefined
        ? {}
        : { outputKey: attempt.output.key }),
    };
    return summary;
  });
}

function normalizeStatus(status: string): StepSummary["status"] {
  return status === "succeeded" || status === "failed" ? status : "pending";
}

// --- orchestrator --json parsing -------------------------------------------

interface RunJson {
  readonly runId: string;
  readonly status: string;
  readonly resultKey?: string;
  readonly steps: readonly {
    readonly nodeId: string;
    readonly status: string;
    readonly diagnostic?: string;
  }[];
}

function parseRunJson(stdout: string): RunJson | undefined {
  const trimmed = stdout.trim();
  if (trimmed === "") return undefined;
  try {
    const value = JSON.parse(trimmed) as RunJson;
    return Array.isArray(value.steps) ? value : undefined;
  } catch {
    return undefined;
  }
}

function runnerExitFromDiagnostic(
  diagnostic: string | undefined,
): RunnerExit | undefined {
  const match = diagnostic === undefined
    ? null
    : /\(exit (\d+)\)/.exec(diagnostic);
  if (match === null) return undefined;
  const code = Number(match[1]);
  return code === RUNNER_EXIT_CODES.descriptorResolutionFailure ||
      code === RUNNER_EXIT_CODES.schemaValidationFailure ||
      code === RUNNER_EXIT_CODES.stepExecutionFailure
    ? code
    : undefined;
}

function splitDiagnostics(stderr: string): string[] {
  return stderr.split("\n").map((line) => line.trim()).filter((line) =>
    line !== ""
  );
}

// --- shared helpers --------------------------------------------------------

// The emit-cache identity hash: everything that determines the emitted spec.
// The cache pointer lives at cache/emit/<segment(identityHash)>.json and records
// this hash so a read can confirm the pointer is bound to the current run.
function emitIdentityHash(
  sourcePackageHash: string,
  targets: readonly WorkflowSpecTarget[],
  entry: { readonly module: string; readonly export: string },
  configHash: string,
): string {
  return sha256Text(
    stableStringify({ sourcePackageHash, targets, entry, configHash }),
  );
}

// The Go orchestrator prints identifiable spec/plan diagnostics with this stable
// prefix (main.go, `*spec.DiagnosticsError`); anything else on stderr is a
// run-level failure rather than a spec/plan rejection.
const SPEC_REJECTION_MARKER = "invalid workflow spec:";

function isSpecCompileRejection(stderr: string): boolean {
  return stderr.includes(SPEC_REJECTION_MARKER);
}

// Last few non-empty stderr lines, for surfacing an orchestrator diagnostic
// without dumping the whole stream.
function stderrTail(stderr: string, limit = 10): string {
  const lines = stderr.split("\n").map((line) => line.trimEnd()).filter((
    line,
  ) => line.trim() !== "");
  return lines.slice(-limit).join("\n");
}

// A run id must be a single safe datastore path segment: the CLI interpolates it
// into a stat path when locating run artifacts, so reject anything that is not
// one clean segment (no separators, no ".", "..", or empty) before use.
export function isValidRunId(runId: string): boolean {
  if (runId.includes("/") || runId.includes("\\")) return false;
  try {
    validateObjectKey(runId);
    return true;
  } catch {
    return false;
  }
}

function segmentForHash(hash: string): string {
  return hash.startsWith("sha256:") ? hash.replace(":", "-") : `sha256-${hash}`;
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await stat(path);
    return true;
  } catch {
    return false;
  }
}

// Repo root, derived from this module's location (packages/cli/src/run.ts): the
// orchestrator resolves the Deno runner and deno.json relative to its own
// working directory, so it must run with the repo root as cwd regardless of
// where the user invoked `massive`.
export function repoRoot(): string {
  return resolvePath(dirname(fileURLToPath(import.meta.url)), "..", "..", "..");
}
