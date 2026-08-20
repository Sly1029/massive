import { readFile, realpath, stat } from "node:fs/promises";
import { isAbsolute, posix, relative, resolve, sep } from "node:path";
import { z } from "zod";
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
  readonly files: SourcePackageFiles;
  readonly sourcePackageHash: string;
}

const sourcePackagePathSchema = z.string().min(1).superRefine(
  (filePath, context) => {
    const segments = filePath.split("/");
    if (
      filePath.includes("\\") || filePath.startsWith("/") ||
      posix.normalize(filePath) !== filePath ||
      segments.some((segment) =>
        segment === "" || segment === "." || segment === ".."
      )
    ) {
      context.addIssue({
        code: "custom",
        message: "path must be a normalized POSIX-relative path",
      });
    }
  },
).brand<"SourcePackagePath">();

const sourcePackageFileSchema = z.object({
  path: sourcePackagePathSchema,
  hash: z.string().regex(
    /^sha256:[0-9a-f]{64}$/,
    "hash must be sha256:<64 lowercase hex>",
  ).brand<"SHA256Ref">(),
}).readonly();

const sourcePackageFilesSchema = z.array(sourcePackageFileSchema).min(1)
  .superRefine((files, context) => {
    for (let index = 1; index < files.length; index++) {
      if (compareCodeUnits(files[index - 1]!.path, files[index]!.path) >= 0) {
        context.addIssue({
          code: "custom",
          path: [index, "path"],
          message:
            "source package files must have unique paths in UTF-16 code-unit order",
        });
      }
    }
  }).readonly().brand<"SourcePackageFiles">();

export type SourcePackageFiles = z.infer<typeof sourcePackageFilesSchema>;

export function parseSourcePackageFiles(files: unknown): SourcePackageFiles {
  const parsed = sourcePackageFilesSchema.safeParse(files);
  if (!parsed.success) {
    throw new SourcePackagePathError(
      parsed.error.issues[0]?.message ?? "invalid source package files",
    );
  }
  return parsed.data;
}

export function sourcePackageDigest(
  files: SourcePackageFiles,
): string {
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

  const parsedEntries = parseSourcePackageFiles(entries);
  const sourcePackageHash = sourcePackageDigest(parsedEntries);
  return {
    root,
    include: [...source.include],
    files: parsedEntries,
    sourcePackageHash,
  };
}

function normalizeObjectPath(path: string): string {
  return path.split(sep).join("/");
}
