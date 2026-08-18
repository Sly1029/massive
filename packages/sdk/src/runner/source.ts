import { chmod, rm, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, relative, resolve, sep } from "node:path";
import { pathToFileURL } from "node:url";
import { Key } from "../datastore/key.ts";
import type { DatastoreClient } from "../datastore/types.ts";
import { sha256RefBytes } from "../stable.ts";
import type { StepRun } from "../workflow.ts";
import type { StepInvocationDescriptor } from "./descriptor.ts";
import { SymbolResolutionError } from "./outcomes.ts";

const SOURCE_ARCHIVE_CONTENT_TYPE = "application/vnd.massive.source-tar";
const TAR_BLOCK_SIZE = 512;
const MAX_SOURCE_FILES = 1_024;
const MAX_SOURCE_BYTES = 50 * 1024 * 1024;

export interface ResolvedStepSymbol {
  readonly packageRoot: string;
  readonly run: StepRun<unknown, unknown>;
  readonly cleanup: () => Promise<void>;
}

export async function resolveStepSymbol(
  descriptor: StepInvocationDescriptor,
  store: DatastoreClient,
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

  const packageRoot = await fetchSourcePackage(descriptor, store);
  try {
    const modulePath = resolveModulePath(packageRoot, descriptor.symbol.module);
    const module = (await import(
      `${pathToFileURL(modulePath).href}?packageHash=${encodeURIComponent(descriptor.sourcePackage.packageHash)}`
    )) as Record<string, unknown>;
    const run = stepRunFromExport(module[descriptor.symbol.export]);
    if (run === undefined) {
      throw new SymbolResolutionError(
        `export "${descriptor.symbol.export}" in module "${descriptor.symbol.module}" is not a step run function`,
      );
    }
    return {
      packageRoot,
      run,
      cleanup: async () => await rm(packageRoot, { force: true, recursive: true }),
    };
  } catch (error) {
    await rm(packageRoot, { force: true, recursive: true });
    throw error;
  }
}

async function fetchSourcePackage(
  descriptor: StepInvocationDescriptor,
  store: DatastoreClient,
): Promise<string> {
  if (descriptor.sourcePackage.sourceArchive.contentType !== SOURCE_ARCHIVE_CONTENT_TYPE) {
    throw new SymbolResolutionError(
      `unsupported source archive content type "${descriptor.sourcePackage.sourceArchive.contentType}"`,
    );
  }
  let bytes: Uint8Array;
  try {
    bytes = (await store.get(
      Key.parse(descriptor.sourcePackage.sourceArchive.key),
    )).body;
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

  const scratchParent = descriptor.datastore.kind === "local"
    ? descriptor.datastore.path
    : Deno.cwd();
  const root = await Deno.makeTempDir({ dir: scratchParent, prefix: "massive-source-" });
  try {
    await extractSourceArchive(bytes, root);
    return root;
  } catch (error) {
    await rm(root, { force: true, recursive: true });
    if (error instanceof SymbolResolutionError) throw error;
    const message = error instanceof Error ? error.message : String(error);
    throw new SymbolResolutionError(`invalid source archive: ${message}`);
  }
}

async function extractSourceArchive(bytes: Uint8Array, root: string): Promise<void> {
  let offset = 0;
  const names = new Set<string>();
  let sawEnd = false;
  let totalSize = 0;
  while (offset + TAR_BLOCK_SIZE <= bytes.length) {
    const header = bytes.slice(offset, offset + TAR_BLOCK_SIZE);
    offset += TAR_BLOCK_SIZE;
    if (header.every((byte) => byte === 0)) {
      if (offset + TAR_BLOCK_SIZE > bytes.length || !bytes.slice(offset, offset + TAR_BLOCK_SIZE).every((byte) => byte === 0)) {
        throw new Error("tar archive is missing its second zero block");
      }
      sawEnd = true;
      if (bytes.slice(offset + TAR_BLOCK_SIZE).some((byte) => byte !== 0)) {
        throw new Error("tar archive has trailing data after its end marker");
      }
      break;
    }
    verifyTarChecksum(header);
    if (tarString(header, 257, 6) !== "ustar" || tarString(header, 263, 2) !== "00") {
      throw new Error("source archive must use the USTAR format");
    }
    const name = tarString(header, 0, 100);
    const prefix = tarString(header, 345, 155);
    const path = prefix === "" ? name : `${prefix}/${name}`;
    if (!safeArchivePath(path) || names.has(path)) {
      throw new Error(`tar archive has unsafe or duplicate path ${JSON.stringify(path)}`);
    }
    const type = header[156];
    if (type !== 0 && type !== 48) {
      throw new Error(`tar archive entry ${JSON.stringify(path)} is not a regular file`);
    }
    const size = tarSize(header);
    totalSize += size;
    if (names.size >= MAX_SOURCE_FILES || totalSize > MAX_SOURCE_BYTES) {
      throw new Error("tar archive exceeds source package limits");
    }
    if (size > bytes.length - offset) throw new Error(`tar archive entry ${JSON.stringify(path)} is truncated`);
    const target = resolve(root, path);
    const backToRoot = relative(root, target);
    if (backToRoot === "" || backToRoot.startsWith(`..${sep}`) || isAbsolute(backToRoot)) {
      throw new Error(`tar archive entry ${JSON.stringify(path)} escapes extraction root`);
    }
    await Deno.mkdir(dirname(target), { recursive: true });
    await writeFile(target, bytes.slice(offset, offset + size), { mode: 0o644 });
    await chmod(target, 0o444);
    names.add(path);
    offset += Math.ceil(size / TAR_BLOCK_SIZE) * TAR_BLOCK_SIZE;
  }
  if (!sawEnd || offset > bytes.length) throw new Error("tar archive is incomplete");
}

function tarString(header: Uint8Array, start: number, length: number): string {
  const field = header.slice(start, start + length);
  const end = field.indexOf(0);
  return new TextDecoder().decode(end === -1 ? field : field.slice(0, end));
}

function tarSize(header: Uint8Array): number {
  const text = tarString(header, 124, 12).trim();
  if (!/^[0-7]+$/.test(text)) throw new Error("tar archive has an invalid file size");
  const size = Number.parseInt(text, 8);
  if (!Number.isSafeInteger(size) || size < 0) throw new Error("tar archive has an unsafe file size");
  return size;
}

function verifyTarChecksum(header: Uint8Array): void {
  const expected = tarOctal(header.slice(148, 156), "checksum");
  let actual = 0;
  for (let index = 0; index < TAR_BLOCK_SIZE; index++) {
    actual += index >= 148 && index < 156 ? 32 : header[index];
  }
  if (actual !== expected) throw new Error("tar archive header checksum mismatch");
}

function tarOctal(field: Uint8Array, name: string): number {
  const text = new TextDecoder().decode(field).replaceAll("\0", "").trim();
  if (!/^[0-7]+$/.test(text)) throw new Error(`tar archive has an invalid ${name}`);
  const value = Number.parseInt(text, 8);
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`tar archive has an unsafe ${name}`);
  return value;
}

function safeArchivePath(path: string): boolean {
  if (path === "" || path.startsWith("/") || path.includes("\\")) return false;
  return path.split("/").every((part) => part !== "" && part !== "." && part !== "..");
}

function resolveModulePath(packageRoot: string, module: string): string {
  const modulePath = module.startsWith("./") ? module.slice(2) : module;
  const resolved = resolve(packageRoot, modulePath);
  const backToRoot = relative(packageRoot, resolved);
  if (backToRoot === "" || backToRoot.startsWith(`..${sep}`) || isAbsolute(backToRoot)) {
    throw new SymbolResolutionError(`module "${module}" resolves outside source package root`);
  }
  return resolved;
}

function stepRunFromExport(value: unknown): StepRun<unknown, unknown> | undefined {
  if (typeof value === "function") return value as StepRun<unknown, unknown>;
  if (value !== null && typeof value === "object" && "run" in value) {
    const run = (value as { readonly run?: unknown }).run;
    if (typeof run === "function") return run as StepRun<unknown, unknown>;
  }
  return undefined;
}
