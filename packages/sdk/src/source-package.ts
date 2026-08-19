import { readFile, realpath, stat } from "node:fs/promises";
import { isAbsolute, posix, relative, resolve, sep } from "node:path";
import { MassiveError, SourcePackagePathError } from "./errors.ts";
import {
  compareCodeUnits,
  sha256RefBytes,
  sha256RefText,
  stableStringify,
} from "./stable.ts";
import { SOURCE_PACKAGE_HASHING } from "./hashing.ts";

export interface SourceSpec {
  readonly root: string;
  readonly include: readonly string[];
}

export interface SourcePackage {
  readonly root: string;
  readonly include: string[];
  readonly files: { readonly path: string; readonly hash: string }[];
  readonly sourcePackageHash: string;
}

export function sourcePackageDigest(
  files: readonly { readonly path: string; readonly hash: string }[],
): string {
  for (const [index, file] of files.entries()) {
    if (
      file.path === "" || file.path.includes("\\") ||
      file.path.startsWith("/") || posix.normalize(file.path) !== file.path ||
      file.path === "."
    ) {
      throw new SourcePackagePathError(
        `source package file ${index} path is not a normalized relative path: ${file.path}`,
      );
    }
    if (
      index > 0 && compareCodeUnits(files[index - 1]!.path, file.path) >= 0
    ) {
      throw new SourcePackagePathError(
        "source package files must have unique paths in UTF-16 code-unit order",
      );
    }
  }
  return sha256RefText(stableStringify({
    files,
    hashing: SOURCE_PACKAGE_HASHING,
    kind: "SourcePackageHashInput",
    schemaVersion: 0,
  }));
}

export async function hashSourcePackage(
  source: SourceSpec,
): Promise<SourcePackage> {
  if (source.include.length === 0) {
    throw new MassiveError(
      "compile source.include must contain at least one pattern",
    );
  }

  // Imported lazily so that importing "@massive/sdk" (and therefore the step
  // runner that loads a workflow module) never evaluates fast-glob, which probes
  // os.cpus() at load and would require --allow-sys the runner does not grant.
  const { default: fg } = await import("fast-glob");
  const root = await realpath(resolve(source.root));
  const files = await fg([...source.include], {
    cwd: root,
    dot: true,
    followSymbolicLinks: false,
    objectMode: true,
    onlyFiles: false,
    unique: true,
  });

  const entries: { path: string; hash: string }[] = [];
  for (
    const entry of files.sort((left, right) =>
      compareCodeUnits(left.path, right.path)
    )
  ) {
    if (!entry.dirent.isFile() && !entry.dirent.isSymbolicLink()) {
      continue;
    }

    const file = entry.path;
    const absolute = resolve(root, file);
    const backToRoot = relative(root, absolute);
    if (
      backToRoot === "" || backToRoot.startsWith(`..${sep}`) ||
      isAbsolute(backToRoot)
    ) {
      throw new MassiveError(
        `compile source include resolved outside root: ${file}`,
      );
    }

    const realFile = await realpath(absolute);
    const realBackToRoot = relative(root, realFile);
    if (
      realBackToRoot === "" || realBackToRoot.startsWith(`..${sep}`) ||
      isAbsolute(realBackToRoot)
    ) {
      throw new SourcePackagePathError(
        `compile source include resolved outside root after following symlinks: ${file}`,
      );
    }

    if (!(await stat(realFile)).isFile()) {
      continue;
    }

    entries.push({
      path: normalizeObjectPath(backToRoot),
      hash: sha256RefBytes(await readFile(realFile)),
    });
  }

  const sourcePackageHash = sourcePackageDigest(entries);
  return {
    root,
    include: [...source.include],
    files: entries,
    sourcePackageHash,
  };
}

function normalizeObjectPath(path: string): string {
  return path.split(sep).join("/");
}
