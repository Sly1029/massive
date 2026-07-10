import { assert, assertEquals, assertStringIncludes } from "jsr:@std/assert";
import {
  copyFixture,
  fixtureEntry,
  join,
  makeStore,
  runCli,
} from "./harness.ts";

// The CLI classifies orchestrator failures by what the orchestrator actually
// reported, not by "the run didn't succeed". A run-level failure must not be
// misattributed to a succeeded step, and unparseable output that is not an
// identifiable spec/plan diagnostic is a run failure (exit 1), not a compile
// rejection (exit 5).
//
// These drive the CLI against a canned orchestrator supplied through the same
// MASSIVE_ORCHESTRATOR_BIN seam the CLI uses in production — a real subprocess
// at the documented process boundary, producing controlled stdout/stderr. It is
// the only way to exercise orchestrator output shapes the emit path cannot
// otherwise produce.

async function cannedOrchestrator(
  stdout: string,
  stderr: string,
): Promise<string> {
  const dir = await Deno.makeTempDir({ prefix: "massive-canned-orch-" });
  const binary = join(dir, "orchestrator");
  const quote = (value: string): string =>
    `'${value.replaceAll("'", "'\\''")}'`;
  await Deno.writeTextFile(
    binary,
    `#!/usr/bin/env sh\nprintf %s ${quote(stdout)}\nprintf %s ${
      quote(stderr)
    } 1>&2\nexit 1\n`,
  );
  await Deno.chmod(binary, 0o755);
  return binary;
}

async function runAgainstCanned(binary: string): Promise<
  { code: number; stdout: string; stderr: string }
> {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  return await runCli(
    [
      "run",
      fixtureEntry(fixture),
      "--input",
      "20",
      "--store",
      store,
      "--project",
      "acme/wf",
      "--run-id",
      "run-canned",
    ],
    { useOrchestratorBinary: false, env: { MASSIVE_ORCHESTRATOR_BIN: binary } },
  );
}

Deno.test("run-level failure with no failed step -> run-failed (exit 1), not misattributed to a step", async () => {
  // A run object whose steps all succeeded but whose run status is failed: the
  // orchestrator failed at the run level (e.g. after the last step).
  const binary = await cannedOrchestrator(
    JSON.stringify({
      runId: "run-canned",
      status: "failed",
      steps: [{ nodeId: "double", status: "succeeded" }],
    }),
    "massive-orchestrator: run aborted while finalizing result\n",
  );
  const result = await runAgainstCanned(binary);

  assertEquals(
    result.code,
    1,
    `stdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
  );
  assertStringIncludes(result.stderr, "run failed");
  assertStringIncludes(result.stderr, "run aborted while finalizing result");
  assertStringIncludes(result.stderr, "next");
});

Deno.test("unparseable orchestrator output (not a spec/plan diagnostic) -> run-failed (exit 1), not compile-rejected", async () => {
  const binary = await cannedOrchestrator(
    "this is not json\n",
    "massive-orchestrator: could not open datastore\n",
  );
  const result = await runAgainstCanned(binary);

  assertEquals(
    result.code,
    1,
    `stdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
  );
  assert(
    result.code !== 5,
    "an orchestrator error that is not a spec/plan diagnostic must not be a compile rejection",
  );
  assertStringIncludes(result.stderr, "run failed");
  assertStringIncludes(result.stderr, "could not open datastore");
  assertStringIncludes(result.stderr, "next");
});

Deno.test("spec/plan diagnostic on stderr stays compile-rejected (exit 5)", async () => {
  // The orchestrator prints identifiable spec diagnostics with a stable prefix;
  // that structure — not the absence of JSON — is what maps to exit 5.
  const binary = await cannedOrchestrator(
    "",
    'invalid workflow spec: step "double": output schema is not portable\n',
  );
  const result = await runAgainstCanned(binary);

  assertEquals(
    result.code,
    5,
    `stdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
  );
  assertStringIncludes(result.stderr, "spec");
  assertStringIncludes(result.stderr, "next");
});
