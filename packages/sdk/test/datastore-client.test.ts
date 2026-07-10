import {
  CreateBucketCommand,
  HeadBucketCommand,
  S3Client,
} from "npm:@aws-sdk/client-s3";
import {
  assert,
  assertEquals,
  assertRejects,
  assertThrows,
} from "jsr:@std/assert";
import {
  blobKeyForBytes,
  datastore,
  DatastoreConflictError,
  Key,
  LocalDatastoreClient,
  S3DatastoreClient,
} from "../src/datastore/index.ts";
import type { DatastoreClient } from "../src/datastore/index.ts";
import { DatastoreKeyError } from "../src/errors.ts";

const decoder = new TextDecoder();

Deno.test("local datastore client satisfies the datastore contract", async () => {
  const roots: string[] = [];
  try {
    await runDatastoreClientContract(async () => {
      const root = await Deno.makeTempDir({ prefix: "massive-datastore-" });
      roots.push(root);
      return new LocalDatastoreClient({ path: root });
    });
  } finally {
    await Promise.all(
      roots.map((root) => Deno.remove(root, { recursive: true })),
    );
  }
});

Deno.test({
  name:
    "s3 datastore client satisfies the datastore contract against configured MinIO",
  ignore: Deno.env.get("MASSIVE_TEST_S3_ENDPOINT") === undefined,
  async fn() {
    const bucket = Deno.env.get("MASSIVE_TEST_S3_BUCKET") ??
      "massive-datastore-client";
    await ensureS3Bucket(bucket);
    await runDatastoreClientContract(async () =>
      new S3DatastoreClient({
        endpoint: Deno.env.get("MASSIVE_TEST_S3_ENDPOINT")!,
        bucket,
        region: Deno.env.get("MASSIVE_TEST_S3_REGION") ?? "us-east-1",
        prefix: `ts-contract/${crypto.randomUUID()}`,
        accessKeyEnv: "MASSIVE_TEST_S3_ACCESS_KEY",
        secretAccessKeyEnv: "MASSIVE_TEST_S3_SECRET_KEY",
      })
    );
  },
});

Deno.test("Go and TypeScript local datastore clients interoperate on the same layout", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-datastore-" });
  try {
    const store = new LocalDatastoreClient({ path: root });

    await runGoInterop([
      "write-local",
      root,
      "interop/from-go.txt",
      "from go",
      "text/x-go",
    ]);
    const goObject = await store.get(Key.parse("interop/from-go.txt"));
    assertEquals(decoder.decode(goObject.body), "from go");
    assertEquals(goObject.info.contentType, "text/x-go");

    await store.put(Key.parse("interop/from-ts.txt"), "from ts", {
      contentType: "text/x-ts",
    });
    const output = await runGoInterop([
      "read-local",
      root,
      "interop/from-ts.txt",
    ]);
    assertEquals(output, "text/x-ts\nfrom ts\n");
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});

Deno.test("local datastore string API rejects invalid keys asynchronously", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-datastore-" });
  try {
    const store = datastore.local({ path: root });

    const operations: readonly [string, () => Promise<unknown>][] = [
      ["put", () => store.put("../escape", "bad")],
      ["get", () => store.get("../escape")],
      ["exists", () => store.exists("../escape")],
    ];

    for (const [name, operation] of operations) {
      let promise: Promise<unknown>;
      try {
        promise = operation();
      } catch (error) {
        throw new Error(`${name} threw synchronously`, { cause: error });
      }

      await assertRejects(() => promise, DatastoreKeyError);
    }
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});

async function runDatastoreClientContract(
  factory: () => Promise<DatastoreClient>,
): Promise<void> {
  await contractRoundTrip(await factory());
  await contractExists(await factory());
  await contractConditionalWrite(await factory());
  await contractList(await factory());
  await contractKeyValidation();
  await contractBlobHelpers(await factory());
}

async function contractRoundTrip(store: DatastoreClient): Promise<void> {
  const key = Key.parse("objects/round-trip.txt");
  const putInfo = await store.put(key, "hello", { contentType: "text/plain" });
  assertEquals(putInfo.key.toString(), key.toString());
  assertEquals(putInfo.size, 5);
  assertEquals(putInfo.contentType, "text/plain");

  const object = await store.get(key);
  assertEquals(decoder.decode(object.body), "hello");
  assertEquals(object.info.contentType, "text/plain");
}

async function contractExists(store: DatastoreClient): Promise<void> {
  const key = Key.parse("objects/exists.txt");
  assertEquals(await store.exists(key), false);
  await store.put(key, "present");
  assertEquals(await store.exists(key), true);
}

async function contractConditionalWrite(store: DatastoreClient): Promise<void> {
  const key = Key.parse("objects/conditional.txt");
  await store.put(key, "first", { ifAbsent: true });
  await assertRejects(
    () => store.put(key, "second", { ifAbsent: true }),
    DatastoreConflictError,
  );
  assertEquals(decoder.decode((await store.get(key)).body), "first");
}

async function contractList(store: DatastoreClient): Promise<void> {
  await store.put(Key.parse("prefix/a.json"), "a", {
    contentType: "application/json",
  });
  await store.put(Key.parse("prefix/nested/b.json"), "b", {
    contentType: "application/json",
  });
  await store.put(Key.parse("outside/c.json"), "c", {
    contentType: "application/json",
  });

  const objects = await store.list(Key.parse("prefix"));
  assertEquals(objects.map((object) => object.key.toString()), [
    "prefix/a.json",
    "prefix/nested/b.json",
  ]);
  assert(objects.every((object) => object.contentType === "application/json"));
}

async function contractKeyValidation(): Promise<void> {
  for (
    const key of [
      "",
      "/leading",
      String.raw`objects\backslash`,
      "objects//empty",
      "objects/./dot",
      "objects/../escape",
      "..",
      "C:/absolute",
    ]
  ) {
    assertThrows(() => Key.parse(key), DatastoreKeyError);
  }
}

async function contractBlobHelpers(store: DatastoreClient): Promise<void> {
  const body = new TextEncoder().encode("test");
  const key = await blobKeyForBytes(body);
  assertEquals(
    key.toString(),
    "blobs/sha256/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  );
  await store.put(key, body, {
    ifAbsent: true,
    contentType: "application/octet-stream",
  });
  assertEquals(decoder.decode((await store.get(key)).body), "test");
}

async function runGoInterop(args: string[]): Promise<string> {
  const command = new Deno.Command("go", {
    args: ["run", "./internal/datastore/interopcli", ...args],
    env: { GOCACHE: "/tmp/massive-go-cache" },
    stdout: "piped",
    stderr: "piped",
  });
  const output = await command.output();
  if (!output.success) {
    throw new Error(decoder.decode(output.stderr));
  }
  return decoder.decode(output.stdout);
}

async function ensureS3Bucket(bucket: string): Promise<void> {
  const accessKeyId = Deno.env.get("MASSIVE_TEST_S3_ACCESS_KEY");
  const secretAccessKey = Deno.env.get("MASSIVE_TEST_S3_SECRET_KEY");
  if (accessKeyId === undefined || secretAccessKey === undefined) {
    throw new Error(
      "S3 datastore test requires MASSIVE_TEST_S3_ACCESS_KEY and MASSIVE_TEST_S3_SECRET_KEY",
    );
  }

  const client = new S3Client({
    endpoint: Deno.env.get("MASSIVE_TEST_S3_ENDPOINT")!,
    region: Deno.env.get("MASSIVE_TEST_S3_REGION") ?? "us-east-1",
    forcePathStyle: true,
    credentials: {
      accessKeyId,
      secretAccessKey,
      sessionToken: Deno.env.get("MASSIVE_TEST_S3_SESSION_TOKEN"),
    },
  });

  try {
    await client.send(new HeadBucketCommand({ Bucket: bucket }));
  } catch {
    await client.send(new CreateBucketCommand({ Bucket: bucket }));
  }
}
