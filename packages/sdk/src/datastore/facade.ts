import { Key } from "./key.ts";
import { LocalDatastoreClient } from "./local.ts";
import type { S3Config } from "./s3.ts";
import type { DatastoreClient, PutOptions } from "./types.ts";

export interface Datastore {
  readonly kind: "local" | "s3";
  put(
    key: string,
    value: string | Uint8Array,
    options?: PutOptions,
  ): Promise<void>;
  get(key: string): Promise<Uint8Array>;
  exists(key: string): Promise<boolean>;
}

export interface LocalDatastore extends Datastore {
  readonly kind: "local";
  readonly root: string;
}

export interface S3Datastore extends Datastore {
  readonly kind: "s3";
}

export const datastore = {
  local(config: { readonly path: string }): LocalDatastore {
    return new LocalDatastoreFacade(config);
  },

  async s3(config: S3Config): Promise<S3Datastore> {
    // Keep the AWS SDK out of the local runner's module graph. Besides making
    // local startup smaller, this preserves its intentionally scoped Deno
    // environment permissions; the SDK probes environment configuration while
    // initializing its default credential provider chain.
    const { S3DatastoreClient } = await import("./s3.ts");
    return new RemoteDatastore(new S3DatastoreClient(config));
  },
};

class LocalDatastoreFacade implements LocalDatastore {
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

class RemoteDatastore implements S3Datastore {
  readonly kind = "s3" as const;

  constructor(private readonly client: DatastoreClient) {}

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
