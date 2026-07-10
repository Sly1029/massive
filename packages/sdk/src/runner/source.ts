import { mkdir, rm } from "node:fs/promises";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { pathToFileURL } from "node:url";
import { datastore } from "../datastore/facade.ts";
import { sha256RefBytes } from "../stable.ts";
import type { StepRun } from "../workflow.ts";
import type { StepInvocationDescriptor } from "./descriptor.ts";
import { SymbolResolutionError } from "./outcomes.ts";

export interface ResolvedStepSymbol {
  readonly packageRoot: string;
  readonly run: StepRun<unknown, unknown>;
}

interface SourceFetchPointer {
  readonly sourceFetch: string;
}

// Test-only source package artifact: canonical JSON `{ "sourceFetch": "/abs/path" }`.
// The descriptor shape stays frozen; this is an alternate local artifact body
// used until tar.zst extraction is available in the runtime.
const SOURCE_FETCH_CONTENT_TYPE =
  "application/vnd.massive.source-directory+json";

export async function resolveStepSymbol(
  descriptor: StepInvocationDescriptor,
): Promise<ResolvedStepSymbol> {
  if (descriptor.symbol.language !== "typescript") {
    throw new SymbolResolutionError(
      `language "${descriptor.symbol.language}" is not supported by the TypeScript runner`,
    );
  }
  if (descriptor.sourcePackage.language !== "typescript") {
    throw new SymbolResolutionError(
      `source package language "${descriptor.sourcePackage.language}" is not supported by the TypeScript runner`,
    );
  }
  if (descriptor.symbol.packageId !== descriptor.sourcePackage.packageId) {
    throw new SymbolResolutionError(
      `symbol package "${descriptor.symbol.packageId}" does not match source package "${descriptor.sourcePackage.packageId}"`,
    );
  }

  // sourceArchive.hash is the digest of the artifact body and is verified
  // against the fetched bytes in fetchSourcePackage. It is intentionally
  // distinct from packageHash (the plan's content-addressed package hash),
  // so the two are not required to be equal here.
  const packageRoot = await fetchSourcePackage(descriptor);
  const modulePath = resolveModulePath(packageRoot, descriptor.symbol.module);
  const module = (await import(
    `${pathToFileURL(modulePath).href}?packageHash=${
      encodeURIComponent(
        descriptor.sourcePackage.packageHash,
      )
    }`
  )) as Record<string, unknown>;
  const exported = module[descriptor.symbol.export];
  const run = stepRunFromExport(exported);
  if (run === undefined) {
    throw new SymbolResolutionError(
      `export "${descriptor.symbol.export}" in module "${descriptor.symbol.module}" is not a step run function`,
    );
  }

  return { packageRoot, run };
}

async function fetchSourcePackage(
  descriptor: StepInvocationDescriptor,
): Promise<string> {
  if (descriptor.datastore.kind !== "local") {
    throw new SymbolResolutionError(
      "only local datastores are supported by the v0 TypeScript runner",
    );
  }

  const store = datastore.local({ path: descriptor.datastore.path });
  let bytes: Uint8Array;
  try {
    bytes = await store.get(descriptor.sourcePackage.sourceArchive.key);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new SymbolResolutionError(
      `source archive ${descriptor.sourcePackage.sourceArchive.key} could not be read: ${message}`,
    );
  }
  const actualHash = sha256RefBytes(bytes);
  if (actualHash !== descriptor.sourcePackage.sourceArchive.hash) {
    throw new SymbolResolutionError(
      `source archive hash mismatch: expected ${descriptor.sourcePackage.sourceArchive.hash}, got ${actualHash}`,
    );
  }

  if (
    descriptor.sourcePackage.sourceArchive.contentType ===
      SOURCE_FETCH_CONTENT_TYPE
  ) {
    return sourceFetchRoot(bytes);
  }

  if (
    descriptor.sourcePackage.sourceArchive.contentType === "application/zstd"
  ) {
    const unpackRoot = await prepareUnpackRoot(
      descriptor.sourcePackage.packageHash,
    );
    await rm(unpackRoot, { force: true, recursive: true });
    await mkdir(unpackRoot, { recursive: true });
    throw new SymbolResolutionError(
      "source archive content type application/zstd requires tar.zst extraction support; " +
        `use ${SOURCE_FETCH_CONTENT_TYPE} for local fixture packages`,
    );
  }

  throw new SymbolResolutionError(
    `unsupported source archive content type "${descriptor.sourcePackage.sourceArchive.contentType}"`,
  );
}

function sourceFetchRoot(bytes: Uint8Array): string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder().decode(bytes));
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new SymbolResolutionError(
      `sourceFetch artifact is not valid JSON: ${message}`,
    );
  }

  if (!isSourceFetchPointer(parsed)) {
    throw new SymbolResolutionError(
      'sourceFetch artifact must be an object with a string "sourceFetch" path',
    );
  }

  return resolve(parsed.sourceFetch);
}

function resolveModulePath(packageRoot: string, module: string): string {
  const modulePath = module.startsWith("./") ? module.slice(2) : module;
  const resolved = resolve(packageRoot, modulePath);
  const backToRoot = relative(packageRoot, resolved);
  if (
    backToRoot === "" || backToRoot.startsWith(`..${sep}`) ||
    isAbsolute(backToRoot)
  ) {
    throw new SymbolResolutionError(
      `module "${module}" resolves outside source package root`,
    );
  }

  return resolved;
}

async function prepareUnpackRoot(packageHash: string): Promise<string> {
  const digest = packageHash.slice("sha256:".length);
  return resolve(
    await import("node:os").then((os) => os.tmpdir()),
    "massive-source-packages",
    digest,
  );
}

function stepRunFromExport(
  value: unknown,
): StepRun<unknown, unknown> | undefined {
  if (typeof value === "function") {
    return value as StepRun<unknown, unknown>;
  }
  if (value !== null && typeof value === "object" && "run" in value) {
    const run = (value as { readonly run?: unknown }).run;
    if (typeof run === "function") {
      return run as StepRun<unknown, unknown>;
    }
  }

  return undefined;
}

function isSourceFetchPointer(value: unknown): value is SourceFetchPointer {
  return (
    value !== null &&
    typeof value === "object" &&
    typeof (value as { readonly sourceFetch?: unknown }).sourceFetch ===
      "string"
  );
}
