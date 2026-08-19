import type { Datastore } from "@massive/sdk";
import { findRunManifestKeys, readRunManifestAt } from "./run.ts";

export interface InspectRequest {
  readonly runId: string;
  readonly storeRoot: string;
  readonly project?: string;
  readonly step?: string;
  readonly verbose: boolean;
  readonly json: boolean;
}

export type InspectResult =
  | { readonly kind: "ok"; readonly text: string }
  | { readonly kind: "not-found"; readonly runId: string }
  // The same run id exists under multiple projects. The manifest records only
  // the Go-normalized project key (not the raw owner/repo), so --project cannot
  // be matched here without reimplementing that normalization; surface the
  // candidates and let the caller re-run against a store scoped to one project.
  | {
    readonly kind: "ambiguous";
    readonly runId: string;
    readonly candidates: readonly string[];
  }
  | { readonly kind: "invalid-manifest"; readonly message: string };

// Reads the run manifest + result for a past run and reports keys/hashes WITHOUT
// re-executing anything — it never spawns a step or writes new run artifacts.
export async function inspectRun(
  req: InspectRequest,
  store: Datastore,
): Promise<InspectResult> {
  const keys = await findRunManifestKeys(req.storeRoot, req.runId);
  if (keys.length === 0) return { kind: "not-found", runId: req.runId };
  if (keys.length > 1) {
    // Candidate project directories: projects/<project-key>.
    const candidates = keys.map((key) => key.split("/").slice(0, 2).join("/"));
    return { kind: "ambiguous", runId: req.runId, candidates };
  }
  const key = keys[0]!;

  let manifest;
  try {
    manifest = await readRunManifestAt(store, key);
  } catch (error) {
    return {
      kind: "invalid-manifest",
      message: error instanceof Error ? error.message : String(error),
    };
  }
  if (req.json) {
    return { kind: "ok", text: JSON.stringify(manifest) + "\n" };
  }

  const lines = [
    `▸ run ${req.runId}  ·  ${manifest.status}`,
    `  manifest  ${key}`,
    `  planHash  ${manifest.planHash}`,
  ];
  for (const step of manifest.steps) {
    if (req.step !== undefined && req.step !== step.nodeId) continue;
    const attempt = step.attempts?.[0];
    lines.push(`  ${step.nodeId}  ${step.status}`);
    if (attempt?.input !== undefined) {
      lines.push(`      input   ${attempt.input.key}  ${attempt.input.hash}`);
    }
    if (attempt !== undefined && "output" in attempt) {
      lines.push(
        `      output  ${attempt.output.manifest.key}  ${attempt.output.manifest.hash}`,
      );
    }
    if (
      attempt !== undefined && "diagnostic" in attempt &&
      attempt.diagnostic !== ""
    ) {
      lines.push(`      error   ${attempt.diagnostic}`);
    }
    if ("items" in step) {
      lines.push(`      items   ${step.items.length}`);
      for (const item of step.items) {
        const itemAttempt = item.attempts[0];
        lines.push(`      [${item.index}]  ${item.status}`);
        if (itemAttempt !== undefined && "output" in itemAttempt) {
          lines.push(
            `          output  ${itemAttempt.output.manifest.key}  ${itemAttempt.output.manifest.hash}`,
          );
        }
        if (
          itemAttempt !== undefined && "diagnostic" in itemAttempt &&
          itemAttempt.diagnostic !== ""
        ) {
          lines.push(`          error   ${itemAttempt.diagnostic}`);
        }
      }
    }
  }
  if (manifest.result !== undefined) {
    lines.push(`  result    ${manifest.result.key}  ${manifest.result.hash}`);
  }
  return { kind: "ok", text: lines.join("\n") + "\n" };
}
