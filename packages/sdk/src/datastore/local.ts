import { createHash, randomUUID } from "node:crypto";
import {
  link,
  mkdir,
  readdir,
  readFile,
  rename,
  stat,
  unlink,
  writeFile,
} from "node:fs/promises";
import {
  basename,
  dirname,
  isAbsolute,
  relative,
  resolve,
  sep,
} from "node:path";
import { DatastoreKeyError } from "../errors.ts";
import { Key } from "./key.ts";
import {
  type DatastoreClient,
  DatastoreConflictError,
  DatastoreNotFoundError,
  type DatastoreObject,
  defaultContentType,
  encodeBody,
  type ObjectInfo,
  type PutOptions,
} from "./types.ts";

const METADATA_DIR = ".massive-datastore-metadata";

interface LocalMetadata {
  readonly contentType: string;
}

export interface LocalConfig {
  readonly path: string;
}

export class LocalDatastoreClient implements DatastoreClient {
  readonly root: string;

  constructor(config: LocalConfig) {
    this.root = resolve(config.path);
  }

  async put(
    key: Key,
    body: string | Uint8Array,
    options: PutOptions = {},
  ): Promise<ObjectInfo> {
    const target = this.pathForKey(key);
    const directory = dirname(target);
    await mkdir(directory, { recursive: true });

    const bytes = encodeBody(body);
    const temporary = resolve(
      directory,
      `.tmp-${basename(target)}-${randomUUID()}`,
    );
    let installed = false;
    try {
      await writeFile(temporary, bytes);
      if (options.ifAbsent === true) {
        try {
          await link(temporary, target);
        } catch (error) {
          if (isAlreadyExists(error)) {
            throw new DatastoreConflictError(key);
          }
          throw error;
        }
        installed = true;
        await unlink(temporary);
      } else {
        await rename(temporary, target);
        installed = true;
      }
    } finally {
      if (!installed) {
        await unlink(temporary).catch(() => {});
      }
    }

    const contentType = defaultContentType(options.contentType);
    await this.writeMetadata(key, contentType);
    return { key, size: bytes.byteLength, contentType };
  }

  async get(key: Key): Promise<DatastoreObject> {
    try {
      const body = await readFile(this.pathForKey(key));
      return {
        info: {
          key,
          size: body.byteLength,
          contentType: await this.readContentType(key),
        },
        body,
      };
    } catch (error) {
      if (isNotFound(error)) {
        throw new DatastoreNotFoundError(key);
      }
      throw error;
    }
  }

  async exists(key: Key): Promise<boolean> {
    try {
      const info = await stat(this.pathForKey(key));
      return info.isFile();
    } catch (error) {
      if (isNotFound(error)) {
        return false;
      }
      throw error;
    }
  }

  async list(prefix: Key): Promise<ObjectInfo[]> {
    const root = this.pathForKey(prefix);
    try {
      await stat(root);
    } catch (error) {
      if (isNotFound(error)) {
        return [];
      }
      throw error;
    }

    const objects: ObjectInfo[] = [];
    await this.walk(root, async (file) => {
      const relativeKey = relative(this.root, file).split(sep).join("/");
      const key = Key.parse(relativeKey);
      const info = await stat(file);
      objects.push({
        key,
        size: info.size,
        contentType: await this.readContentType(key),
      });
    });

    return objects.sort((left, right) =>
      left.key.toString().localeCompare(right.key.toString())
    );
  }

  private async walk(
    root: string,
    visit: (file: string) => Promise<void>,
  ): Promise<void> {
    for (const entry of await readdir(root, { withFileTypes: true })) {
      const current = resolve(root, entry.name);
      if (entry.isDirectory()) {
        if (current === resolve(this.root, METADATA_DIR)) {
          continue;
        }
        await this.walk(current, visit);
      } else if (entry.isFile()) {
        await visit(current);
      }
    }
  }

  private pathForKey(key: Key): string {
    const target = resolve(this.root, key.toString());
    const backToRoot = relative(this.root, target);
    if (
      backToRoot === "" || backToRoot.startsWith(`..${sep}`) ||
      isAbsolute(backToRoot)
    ) {
      throw new DatastoreKeyError(
        key.toString(),
        "resolved path escapes datastore root",
      );
    }
    return target;
  }

  private metadataPath(key: Key): string {
    const digest = createHash("sha256").update(key.toString()).digest("hex");
    return resolve(this.root, METADATA_DIR, `${digest}.json`);
  }

  private async writeMetadata(key: Key, contentType: string): Promise<void> {
    const target = this.metadataPath(key);
    await mkdir(dirname(target), { recursive: true });
    const temporary = `${target}.tmp-${randomUUID()}`;
    await writeFile(
      temporary,
      JSON.stringify({ contentType } satisfies LocalMetadata),
    );
    await rename(temporary, target);
  }

  private async readContentType(key: Key): Promise<string> {
    try {
      const metadata = JSON.parse(
        await readFile(this.metadataPath(key), "utf8"),
      ) as LocalMetadata;
      return defaultContentType(metadata.contentType);
    } catch (error) {
      if (isNotFound(error)) {
        return defaultContentType(undefined);
      }
      throw error;
    }
  }
}

function isNotFound(error: unknown): boolean {
  return typeof error === "object" && error !== null && "code" in error &&
    error.code === "ENOENT";
}

function isAlreadyExists(error: unknown): boolean {
  return typeof error === "object" && error !== null && "code" in error &&
    error.code === "EEXIST";
}
