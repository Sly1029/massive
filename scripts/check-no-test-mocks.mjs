#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { extname } from "node:path";

const stagedOnly = process.argv.includes("--staged");
const codeExtensions = new Set([
  ".cjs",
  ".cts",
  ".go",
  ".js",
  ".jsx",
  ".mjs",
  ".mts",
  ".py",
  ".ts",
  ".tsx",
]);

const ignoredPathParts = new Set([
  ".git",
  ".turbo",
  "coverage",
  "dist",
  "node_modules",
  "vendor",
]);

const bannedPatterns = [
  {
    name: "Vitest mock API",
    pattern: /\bvi\.(mock|doMock|fn|spyOn|stubGlobal|stubEnv|mocked)\b/,
    guidance:
      "Use a functional fixture: real filesystem/object-store data, a real compiled plan, or a real local runner/backend invocation.",
  },
  {
    name: "Jest mock API",
    pattern: /\bjest\.(mock|doMock|fn|spyOn|mocked)\b/,
    guidance:
      "Use a functional fixture instead of replacing collaborators with mock functions.",
  },
  {
    name: "Sinon test double",
    pattern: /\bsinon\b/,
    guidance:
      "Exercise the behavior through real adapters and inspect generated artifacts or persisted data.",
  },
  {
    name: "Python unittest.mock import",
    pattern: /\bfrom\s+unittest\.mock\s+import\b|\bimport\s+unittest\.mock\b/,
    guidance:
      "Use real fixture objects and integration boundaries instead of unittest.mock.",
  },
  {
    name: "Python MagicMock-style double",
    pattern: /\b(MagicMock|AsyncMock|Mock)\s*\(/,
    guidance:
      "Prefer a typed fake only when it is a real in-memory implementation of the interface; otherwise use the real backend.",
  },
  {
    name: "Python patch API",
    pattern: /\bpatch(?:\.object)?\s*\(/,
    guidance:
      "Inject configuration or use real functional fixtures instead of monkeypatching behavior.",
  },
];

function git(args) {
  return execFileSync("git", args, { encoding: "utf8" });
}

function listFiles() {
  try {
    const output = stagedOnly
      ? git(["diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR"])
      : git(["ls-files", "--cached", "--others", "--exclude-standard", "-z"]);
    return output.split("\0").filter(Boolean);
  } catch {
    return [];
  }
}

function shouldScan(path) {
  if (!existsSync(path)) return false;
  if (!codeExtensions.has(extname(path))) return false;
  return !path.split("/").some((part) => ignoredPathParts.has(part));
}

const violations = [];

for (const path of listFiles().filter(shouldScan)) {
  const lines = readFileSync(path, "utf8").split(/\r?\n/);

  lines.forEach((line, index) => {
    for (const banned of bannedPatterns) {
      if (banned.pattern.test(line)) {
        violations.push({
          path,
          line: index + 1,
          name: banned.name,
          guidance: banned.guidance,
          source: line.trim(),
        });
      }
    }
  });
}

if (violations.length === 0) {
  process.exit(0);
}

console.error("\nMocking is banned in this repository. Tests must be functional.\n");

for (const violation of violations) {
  console.error(`${violation.path}:${violation.line} ${violation.name}`);
  console.error(`  ${violation.source}`);
  console.error(`  ${violation.guidance}\n`);
}

console.error("Allowed alternatives:");
console.error("- temp directories and local filesystem datastore fixtures");
console.error("- local S3-compatible services such as MinIO when object-store behavior matters");
console.error("- kind/minikube/orbstack Kubernetes clusters for Argo manifest execution");
console.error("- generated manifests validated by Kubernetes/Argo schemas");
console.error("- typed in-memory implementations only when they preserve real interface behavior\n");

process.exit(1);

