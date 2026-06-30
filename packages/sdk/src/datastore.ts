import { randomUUID } from "node:crypto";
import { mkdir, readFile, rename, stat, writeFile } from "node:fs/promises";
import { basename, dirname, isAbsolute, relative, resolve, sep } from "node:path";
import { DatastoreKeyError } from "./errors.ts";

export interface Datastore {
  readonly kind: "local";
  readonly root: string;
  put(key: string, value: string | Uint8Array): Promise<void>;
  get(key: string): Promise<Uint8Array>;
  exists(key: string): Promise<boolean>;
}

export const datastore = {
  local(config: { readonly path: string }): Datastore {
    return new LocalDatastore(config.path);
  },
};

class LocalDatastore implements Datastore {
  readonly kind = "local" as const;
  readonly root: string;

  constructor(root: string) {
    this.root = resolve(root);
  }

  async put(key: string, value: string | Uint8Array): Promise<void> {
    const target = this.pathForKey(key);
    const directory = dirname(target);
    await mkdir(directory, { recursive: true });

    const temporary = resolve(directory, `.tmp-${basename(target)}-${randomUUID()}`);
    await writeFile(temporary, value);
    await rename(temporary, target);
  }

  async get(key: string): Promise<Uint8Array> {
    return readFile(this.pathForKey(key));
  }

  async exists(key: string): Promise<boolean> {
    try {
      await stat(this.pathForKey(key));
      return true;
    } catch {
      return false;
    }
  }

  private pathForKey(key: string): string {
    validateObjectKey(key);

    const resolved = resolve(this.root, key);
    const backToRoot = relative(this.root, resolved);
    if (backToRoot === "" || backToRoot.startsWith(`..${sep}`) || isAbsolute(backToRoot)) {
      throw new DatastoreKeyError(key, "resolved path escapes datastore root");
    }

    return resolved;
  }
}

function validateObjectKey(key: string): void {
  if (key.length === 0) {
    throw new DatastoreKeyError(key, "key cannot be empty");
  }
  if (isAbsolute(key)) {
    throw new DatastoreKeyError(key, "key cannot be absolute");
  }
  if (key.includes("\\")) {
    throw new DatastoreKeyError(key, "key must use forward slashes");
  }

  for (const segment of key.split("/")) {
    if (segment === "" || segment === "." || segment === "..") {
      throw new DatastoreKeyError(key, `invalid path segment "${segment}"`);
    }
  }
}
