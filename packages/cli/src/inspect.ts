import type { Datastore } from "@massive/sdk";
import { findRunManifestKeys } from "./run.ts";

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
  };

interface RunManifest {
  readonly planHash: string;
  readonly status: string;
  readonly steps: readonly {
    readonly nodeId: string;
    readonly status: string;
    readonly attempts?: readonly {
      readonly output?: { readonly key: string; readonly hash: string };
      readonly input?: { readonly key: string; readonly hash: string };
      readonly diagnostic?: string;
    }[];
  }[];
  readonly result?: { readonly key: string; readonly hash: string };
}

const decoder = new TextDecoder();

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

  const manifest = JSON.parse(
    decoder.decode(await store.get(key)),
  ) as RunManifest;
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
    if (attempt?.output !== undefined) {
      lines.push(`      output  ${attempt.output.key}  ${attempt.output.hash}`);
    }
    if (attempt?.diagnostic !== undefined && attempt.diagnostic !== "") {
      lines.push(`      error   ${attempt.diagnostic}`);
    }
  }
  if (manifest.result !== undefined) {
    lines.push(`  result    ${manifest.result.key}  ${manifest.result.hash}`);
  }
  return { kind: "ok", text: lines.join("\n") + "\n" };
}
