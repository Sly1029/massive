import fg from "fast-glob";
import { readFile, realpath, stat } from "node:fs/promises";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { MassiveError, SourcePackagePathError } from "./errors.ts";
import {
  compareCodeUnits,
  sha256RefBytes,
  sha256RefText,
  stableStringify,
} from "./stable.ts";

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

export async function hashSourcePackage(
  source: SourceSpec,
): Promise<SourcePackage> {
  if (source.include.length === 0) {
    throw new MassiveError(
      "compile source.include must contain at least one pattern",
    );
  }

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

  const sourcePackageHash = sha256RefText(stableStringify(entries));
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
