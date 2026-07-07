import fg from "fast-glob";
import { readFile } from "node:fs/promises";
import { isAbsolute, relative, resolve, sep } from "node:path";
import { MassiveError } from "./errors.ts";
import { sha256RefBytes, sha256RefText, stableStringify } from "./stable.ts";

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

  const root = resolve(source.root);
  const files = await fg([...source.include], {
    cwd: root,
    dot: true,
    followSymbolicLinks: false,
    onlyFiles: true,
    unique: true,
  });

  const entries: { path: string; hash: string }[] = [];
  for (const file of files.sort()) {
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

    entries.push({
      path: normalizeObjectPath(backToRoot),
      hash: sha256RefBytes(await readFile(absolute)),
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
