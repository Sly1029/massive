import { join } from "node:path";
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

      const version = await goVersion();
      const home = Deno.env.get("HOME") ?? Deno.env.get("USERPROFILE");
      if (home === undefined || home === "") {
        throw new Error(
          "massive: cannot resolve a home directory for the binary cache",
        );
      }
      const binDir = join(home, ".massive", "bin");
      const binary = join(binDir, `massive-orchestrator@${slug(version)}`);
      if (!rebuild && (await pathExists(binary))) return binary;

      await Deno.mkdir(binDir, { recursive: true });
      await build(binary, repoRoot);
      return binary;
    },
  };
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
