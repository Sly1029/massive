import { Key } from "./key.ts";
import { LocalDatastoreClient } from "./local.ts";

export interface Datastore {
  readonly kind: "local";
  readonly root: string;
  put(key: string, value: string | Uint8Array): Promise<void>;
  get(key: string): Promise<Uint8Array>;
  exists(key: string): Promise<boolean>;
}

export const datastore = {
  local(config: { readonly path: string }): Datastore {
    return new LocalDatastore(config);
  },
};

class LocalDatastore implements Datastore {
  readonly kind = "local" as const;
  readonly client: LocalDatastoreClient;

  constructor(config: { readonly path: string }) {
    this.client = new LocalDatastoreClient(config);
  }

  get root(): string {
    return this.client.root;
  }

  async put(key: string, value: string | Uint8Array): Promise<void> {
    await this.client.put(Key.parse(key), value);
  }

  async get(key: string): Promise<Uint8Array> {
    return (await this.client.get(Key.parse(key))).body;
  }

  exists(key: string): Promise<boolean> {
    return this.client.exists(Key.parse(key));
  }
}

export {
  blobKeyForBytes,
  blobKeySHA256Hex,
  Key,
  validateObjectKey,
} from "./key.ts";
export { type LocalConfig, LocalDatastoreClient } from "./local.ts";
export { type S3Config, S3DatastoreClient } from "./s3.ts";
export {
  type DatastoreClient,
  DatastoreConflictError,
  DatastoreNotFoundError,
  type DatastoreObject,
  type ObjectInfo,
  type PutOptions,
} from "./types.ts";
