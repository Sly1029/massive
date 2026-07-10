import type { RunOutcome, StepSummary } from "./run.ts";

export interface ReportOptions {
  readonly verbose: boolean;
  readonly json: boolean;
  readonly storeRoot: string;
}

export interface Rendered {
  readonly stdout: string;
  readonly stderr: string;
}

// Renders a RunOutcome. Quiet by default (per-step marks + inline result, no
// hashes or store paths); --verbose reveals specHash/planHash/runId, the store
// root, and every artifact key; --json emits one machine-readable object built
// from the run manifest. Diagnostics and the actionable `next:` line go to
// stderr; the author-facing result goes to stdout.
export function renderOutcome(
  outcome: RunOutcome,
  options: ReportOptions,
): Rendered {
  if (options.json) {
    return { stdout: renderJson(outcome), stderr: "" };
  }
  switch (outcome.kind) {
    case "succeeded":
      return { stdout: renderSuccess(outcome, options), stderr: "" };
    case "step-failed":
      return {
        stdout: stepList(outcome.steps),
        stderr: renderStepFailure(outcome),
      };
    case "run-failed":
      return {
        stdout: outcome.steps === undefined ? "" : stepList(outcome.steps),
        stderr: renderRunFailure(outcome),
      };
    case "compile-rejected":
      return {
        stdout: "",
        stderr: block("✗ workflow spec rejected", outcome.diagnostics, [
          "next  fix the reported spec/compile errors and re-run",
        ]),
      };
    case "resolve-failed":
      return {
        stdout: "",
        stderr: block(
          "✗ cannot resolve workflow entrypoint",
          [outcome.message],
          [
            "next  name the export (massive run workflow.ts#name) or add target.local() to massive.config.ts",
          ],
        ),
      };
    case "config-error":
      return {
        stdout: "",
        stderr: block("✗ configuration error", [outcome.message], [
          "next  run with --target local, or add the target to massive.config.ts",
        ]),
      };
    case "toolchain-missing":
      return {
        stdout: "",
        stderr: block(`✗ required toolchain not found: ${outcome.tool}`, [], [
          outcome.tool === "go"
            ? "next  install Go 1.24+ (https://go.dev/dl) and re-run"
            : "next  install Deno (https://deno.com) and re-run",
        ]),
      };
  }
}

function renderSuccess(
  outcome: Extract<RunOutcome, { kind: "succeeded" }>,
  options: ReportOptions,
): string {
  const lines = [
    stepList(outcome.steps),
    "",
    "✓ succeeded",
    `  result  ${formatResult(outcome.result)}`,
  ];
  if (options.verbose) {
    lines.push("", ...verboseLines(outcome, options));
  }
  return lines.join("\n") + "\n";
}

function verboseLines(
  outcome: Extract<RunOutcome, { kind: "succeeded" }>,
  options: ReportOptions,
): string[] {
  const lines = [
    ...outcome.notes.map((note) => `  note      ${note}`),
    `  spec      ${
      outcome.specReused ? "reused" : "emitted"
    } ${outcome.specHash}`,
    `  plan      ${outcome.planReused ? "reused" : "compiled"}`,
    `  specHash  ${outcome.specHash}`,
    `  planHash  ${outcome.planHash}`,
    `  runId     ${outcome.runId}`,
    `  store     ${options.storeRoot}`,
    `  result    ${outcome.resultKey}`,
  ];
  for (const step of outcome.steps) {
    if (step.outputKey !== undefined) {
      lines.push(`  ${step.nodeId.padEnd(8)}  ${step.outputKey}`);
    }
  }
  return lines;
}

function renderStepFailure(
  outcome: Extract<RunOutcome, { kind: "step-failed" }>,
): string {
  const detail = [
    outcome.failed.diagnostic ?? `runner exit ${outcome.exitCode}`,
  ];
  if (outcome.stderrTail !== undefined && outcome.stderrTail !== "") {
    detail.push(...outcome.stderrTail.split("\n"));
  }
  return block(
    `✗ ${outcome.failed.nodeId} failed (exit ${outcome.exitCode})`,
    detail,
    [
      "next  inspect the input that reached this step:",
      `      massive inspect ${outcome.runId} --step ${outcome.failed.nodeId}`,
    ],
  );
}

function renderRunFailure(
  outcome: Extract<RunOutcome, { kind: "run-failed" }>,
): string {
  const detail = outcome.diagnostic === ""
    ? ["the orchestrator exited without producing a run result"]
    : outcome.diagnostic.split("\n");
  const next = outcome.runId === undefined
    ? ["next  re-run with --verbose to see the full orchestrator diagnostic"]
    : [
      "next  inspect the partial run for what was recorded:",
      `      massive inspect ${outcome.runId}`,
    ];
  return block(`✗ run failed (exit ${outcome.exitCode})`, detail, next);
}

function stepList(steps: readonly StepSummary[]): string {
  const width = steps.reduce(
    (max, step) => Math.max(max, step.nodeId.length),
    0,
  );
  return steps
    .map((step) => `  ${step.nodeId.padEnd(width)}  ${mark(step.status)}`)
    .join("\n");
}

function mark(status: StepSummary["status"]): string {
  if (status === "succeeded") return "✓";
  if (status === "failed") return "✗";
  return "·";
}

function formatResult(result: unknown): string {
  return typeof result === "string" ? result : JSON.stringify(result);
}

function block(
  headline: string,
  detail: readonly string[],
  next: readonly string[],
): string {
  const lines = [headline];
  for (const line of detail) {
    if (line !== "") lines.push(`    ${line}`);
  }
  lines.push("", ...next.map((line) => `  ${line}`));
  return lines.join("\n") + "\n";
}

function renderJson(outcome: RunOutcome): string {
  if (outcome.kind === "succeeded") {
    return JSON.stringify({
      runId: outcome.runId,
      status: "succeeded",
      result: outcome.result,
      steps: outcome.steps.map((step) => ({
        nodeId: step.nodeId,
        status: step.status,
      })),
      keys: {
        result: outcome.resultKey,
        specHash: outcome.specHash,
        planHash: outcome.planHash,
      },
    }) + "\n";
  }
  if (outcome.kind === "step-failed") {
    return JSON.stringify({
      runId: outcome.runId,
      status: "failed",
      exitCode: outcome.exitCode,
      steps: outcome.steps.map((step) => ({
        nodeId: step.nodeId,
        status: step.status,
        ...(step.diagnostic === undefined
          ? {}
          : { diagnostic: step.diagnostic }),
      })),
    }) + "\n";
  }
  if (outcome.kind === "run-failed") {
    return JSON.stringify({
      ...(outcome.runId === undefined ? {} : { runId: outcome.runId }),
      status: "failed",
      exitCode: outcome.exitCode,
      ...(outcome.diagnostic === "" ? {} : { diagnostic: outcome.diagnostic }),
      ...(outcome.steps === undefined ? {} : {
        steps: outcome.steps.map((step) => ({
          nodeId: step.nodeId,
          status: step.status,
        })),
      }),
    }) + "\n";
  }
  return JSON.stringify({
    status: "error",
    kind: outcome.kind,
    exitCode: outcome.exitCode,
  }) + "\n";
}
