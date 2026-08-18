export {
  type Datastore,
  datastore,
  type LocalDatastore,
  type S3Datastore,
} from "./facade.ts";

export {
  blobKeyForBytes,
  blobKeySHA256Hex,
  Key,
  validateObjectKey,
} from "./key.ts";
export { type LocalConfig, LocalDatastoreClient } from "./local.ts";
export {
  type S3Config,
  S3DatastoreClient,
  type S3EnvironmentCredentials,
} from "./s3.ts";
export {
  type DatastoreClient,
  DatastoreConflictError,
  DatastoreNotFoundError,
  type DatastoreObject,
  type ObjectInfo,
  type PutOptions,
} from "./types.ts";
