import { dirname, join } from "node:path";
import { stat } from "node:fs/promises";

// Raised when a required external tool is absent and cannot be provisioned; the
// CLI maps it to the toolchain exit code with an install hint.
export class ToolchainMissingError extends Error {
  constructor(readonly tool: "go" | "deno") {
    super(`required toolchain "${tool}" not found`);
    this.name = "ToolchainMissingError";
  }
}

// Builds and locates the Go orchestrator binary. On first use the CLI builds
// `~/.massive/bin/massive-orchestrator@<go-version>` and reuses it thereafter;
// `--rebuild` forces a rebuild and `MASSIVE_ORCHESTRATOR_BIN` overrides the
// lookup entirely (tests point it at a once-built binary). Building is the
// single biggest CLI-owned latency lever, so `go run` is never on the hot path.
export interface Toolchain {
  ensure(rebuild: boolean): Promise<string>;
}

const decoder = new TextDecoder();

export function createToolchain(repoRoot: string): Toolchain {
  return {
    async ensure(rebuild: boolean): Promise<string> {
      const override = Deno.env.get("MASSIVE_ORCHESTRATOR_BIN");
      if (override !== undefined && override !== "") return override;

      const selection = await selectOrchestratorBinary(repoRoot);
      // A dirty Go tree has no stable revision to cache against, so always
      // rebuild; otherwise reuse the revision-keyed binary unless --rebuild.
      if (
        !rebuild && !selection.mustRebuild && (await pathExists(selection.path))
      ) {
        return selection.path;
      }

      await Deno.mkdir(dirname(selection.path), { recursive: true });
      await build(selection.path, repoRoot);
      return selection.path;
    },
  };
}

export interface BinarySelection {
  readonly path: string;
  // True when the cache must be bypassed (dirty Go sources or no resolvable
  // revision): the cached artifact could be stale relative to the working tree.
  readonly mustRebuild: boolean;
}

// Resolves the cache path for the orchestrator binary. The cache key is the Go
// toolchain version plus the massive repo's HEAD revision, so a binary built
// from a different commit is never reused. If the Go sources are dirty (or the
// revision is unresolvable) the key is marked dirty and the caller rebuilds.
export async function selectOrchestratorBinary(
  repoRoot: string,
): Promise<BinarySelection> {
  const version = await goVersion();
  const home = Deno.env.get("HOME") ?? Deno.env.get("USERPROFILE");
  if (home === undefined || home === "") {
    throw new Error(
      "massive: cannot resolve a home directory for the binary cache",
    );
  }
  const rev = await gitHead(repoRoot);
  const dirty = rev === undefined ? true : await goSourcesDirty(repoRoot);
  const revSlug = rev === undefined ? "dirty" : rev.slice(0, 12);
  const suffix = dirty ? `${revSlug}-dirty` : revSlug;
  const path = join(
    home,
    ".massive",
    "bin",
    `massive-orchestrator@${slug(version)}-${suffix}`,
  );
  return { path, mustRebuild: dirty };
}

async function gitHead(repoRoot: string): Promise<string | undefined> {
  try {
    const { code, stdout } = await new Deno.Command("git", {
      args: ["-C", repoRoot, "rev-parse", "HEAD"],
      stdout: "piped",
      stderr: "null",
    }).output();
    if (code !== 0) return undefined;
    const rev = decoder.decode(stdout).trim();
    return rev === "" ? undefined : rev;
  } catch {
    return undefined;
  }
}

// Whether the orchestrator's Go sources have uncommitted changes. Scoped to the
// paths that affect the built binary so unrelated edits (e.g. the TS CLI) do not
// force a rebuild.
async function goSourcesDirty(repoRoot: string): Promise<boolean> {
  try {
    const { code, stdout } = await new Deno.Command("git", {
      args: [
        "-C",
        repoRoot,
        "status",
        "--porcelain",
        "--",
        "cmd",
        "internal",
        "conformance/schema",
        "go.mod",
        "go.sum",
      ],
      stdout: "piped",
      stderr: "null",
    }).output();
    if (code !== 0) return true;
    return decoder.decode(stdout).trim() !== "";
  } catch {
    return true;
  }
}

async function goVersion(): Promise<string> {
  try {
    const { code, stdout } = await new Deno.Command("go", {
      args: ["version"],
      stdout: "piped",
      stderr: "null",
    }).output();
    if (code !== 0) throw new ToolchainMissingError("go");
    return decoder.decode(stdout).trim();
  } catch (error) {
    if (error instanceof ToolchainMissingError) throw error;
    // A missing executable surfaces as NotFound from spawn; any spawn failure
    // here means `go` is not runnable.
    throw new ToolchainMissingError("go");
  }
}

async function build(binary: string, repoRoot: string): Promise<void> {
  const { code, stderr } = await new Deno.Command("go", {
    args: ["build", "-o", binary, "./cmd/massive-orchestrator"],
    cwd: repoRoot,
    stdout: "null",
    stderr: "piped",
  }).output();
  if (code !== 0) {
    throw new Error(
      `massive: go build failed:\n${decoder.decode(stderr).trim()}`,
    );
  }
}

function slug(version: string): string {
  return version.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await stat(path);
    return true;
  } catch {
    return false;
  }
}
