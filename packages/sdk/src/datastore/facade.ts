import { Key } from "./key.ts";
import { LocalDatastoreClient } from "./local.ts";
import type { PutOptions } from "./types.ts";

// Local-only string-keyed facade, kept free of the S3 client's module graph so
// the step runner (spawned with scoped Deno permissions) never loads the AWS
// SDK, which reads environment variables at module initialization.
export interface Datastore {
  readonly kind: "local";
  readonly root: string;
  put(
    key: string,
    value: string | Uint8Array,
    options?: PutOptions,
  ): Promise<void>;
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

  async put(
    key: string,
    value: string | Uint8Array,
    options?: PutOptions,
  ): Promise<void> {
    await this.client.put(Key.parse(key), value, options);
  }

  async get(key: string): Promise<Uint8Array> {
    return (await this.client.get(Key.parse(key))).body;
  }

  async exists(key: string): Promise<boolean> {
    return await this.client.exists(Key.parse(key));
  }
}
