import {
  CreateBucketCommand,
  HeadBucketCommand,
  S3Client,
} from "npm:@aws-sdk/client-s3";
import { assertEquals } from "jsr:@std/assert";
import { rm } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { Key, S3DatastoreClient } from "../src/datastore/index.ts";
import {
  parseStepInvocationDescriptor,
  type StepInvocationDescriptor,
} from "../src/runner/descriptor.ts";
import {
  type JsonValue,
  sha256RefText,
  stableStringify,
} from "../src/stable.ts";

const accessKey = "massive-runner-test-access";
const secretKey = "massive-runner-test-secret";
const dockerAccessKeyEnv = "MASSIVE_RUNNER_S3_ACCESS_KEY";
const dockerSecretAccessKeyEnv = "MASSIVE_RUNNER_S3_SECRET_KEY";
const sourceFetchContentType = "application/vnd.massive.source-directory+json";
const valueSchema = {
  type: "object",
  additionalProperties: false,
  required: ["value"],
  properties: { value: { type: "number" } },
} satisfies JsonValue;

Deno.test("S3 invocation descriptors carry transport but no credentials", async () => {
  const descriptor = await parseStepInvocationDescriptor({
    kind: "StepInvocationDescriptor",
    schemaVersion: 0,
    encoding: "json-v0",
    planHash:
      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    runId: "run-s3-descriptor-0001",
    nodeId: "task",
    attempt: 1,
    symbol: {
      packageId: "ts-main",
      language: "typescript",
      module: "./workflow.ts",
      export: "task",
    },
    sourcePackage: {
      packageId: "ts-main",
      language: "typescript",
      packageHash:
        "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      sourceArchive: {
        key:
          "packages/sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/source.tar.zst",
        hash:
          "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
        contentType: sourceFetchContentType,
      },
    },
    environmentRef:
      "sha256:7777777777777777777777777777777777777777777777777777777777777777",
    input: {
      artifact: {
        key: inputKey(),
        hash:
          "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
        contentType: "application/json",
      },
      schema:
        "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    },
    output: {
      artifact: { key: outputKey(), contentType: "application/json" },
      schema:
        "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    },
    datastore: {
      kind: "s3",
      bucket: "massive-test",
      region: "us-east-1",
      endpoint: "http://127.0.0.1:9000",
      forcePathStyle: true,
    },
  });
  assertEquals(descriptor.datastore, {
    kind: "s3",
    bucket: "massive-test",
    region: "us-east-1",
    endpoint: "http://127.0.0.1:9000",
    forcePathStyle: true,
  });
  const serialized = stableStringify(descriptor);
  assertEquals(serialized.includes("credentials"), false);
  assertEquals(serialized.includes("MASSIVE_TEST_ACCESS_KEY"), false);
  assertEquals(serialized.includes("MASSIVE_TEST_SECRET_KEY"), false);
});

Deno.test("runner process reads and writes a descriptor-backed S3 datastore", async (t) => {
  const minio = await startMinIO(t);
  if (minio === undefined) return;

  const root = await Deno.makeTempDir({ prefix: "massive-runner-s3-" });
  const accessKeyEnv = minio.accessKeyEnv;
  const secretAccessKeyEnv = minio.secretAccessKeyEnv;
  const previousAccessKey = Deno.env.get(accessKeyEnv);
  const previousSecretKey = Deno.env.get(secretAccessKeyEnv);
  try {
    const bucket = `massive-runner-${crypto.randomUUID().replaceAll("-", "")}`;
    Deno.env.set(accessKeyEnv, minio.accessKey);
    Deno.env.set(secretAccessKeyEnv, minio.secretKey);
    await createBucket(minio, bucket);

    const store = new S3DatastoreClient({
      endpoint: minio.endpoint,
      forcePathStyle: true,
      bucket,
      region: "us-east-1",
      credentials: {
        kind: "environment",
        accessKeyEnv,
        secretAccessKeyEnv,
      },
    });

    const sourceRoot = fileURLToPath(new URL("./fixtures", import.meta.url));
    const sourcePointer = stableStringify({ sourceFetch: sourceRoot });
    const schemaText = stableStringify(valueSchema);
    const inputText = stableStringify({ value: 21 });
    const descriptor = await parseStepInvocationDescriptor({
      kind: "StepInvocationDescriptor",
      schemaVersion: 0,
      encoding: "json-v0",
      planHash:
        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      runId: "run-s3-runner-fixture-0001",
      nodeId: "double",
      attempt: 1,
      symbol: {
        packageId: "ts-main",
        language: "typescript",
        module: "./runner-workflow.ts",
        export: "double",
      },
      sourcePackage: {
        packageId: "ts-main",
        language: "typescript",
        packageHash:
          "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
        sourceArchive: {
          key:
            "packages/sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/source.tar.zst",
          hash: sha256RefText(sourcePointer),
          contentType: sourceFetchContentType,
        },
      },
      environmentRef:
        "sha256:7777777777777777777777777777777777777777777777777777777777777777",
      input: {
        artifact: {
          key: inputKey(),
          hash: sha256RefText(inputText),
          contentType: "application/json",
        },
        schema: sha256RefText(schemaText),
      },
      output: {
        artifact: {
          key: outputKey(),
          contentType: "application/json",
        },
        schema: sha256RefText(schemaText),
      },
      channelReads: [],
      channelWrites: [],
      datastore: {
        kind: "s3",
        bucket,
        region: "us-east-1",
        endpoint: minio.endpoint,
        forcePathStyle: true,
      },
    });

    await store.put(
      Key.parse(descriptor.sourcePackage.sourceArchive.key),
      sourcePointer,
    );
    await store.put(Key.parse(schemaKey(descriptor.input.schema)), schemaText);
    await store.put(Key.parse(descriptor.input.artifact.key), inputText);

    const descriptorPath = join(root, "descriptor.json");
    await Deno.writeTextFile(descriptorPath, stableStringify(descriptor));
    const child = await new Deno.Command("deno", {
      args: [
        "run",
        "--config",
        "deno.json",
        `--allow-read=${root},${sourceRoot}`,
        `--allow-write=${root}`,
        `--allow-net=${minio.host}`,
        "--allow-env",
        "packages/sdk/src/runner/main.ts",
        descriptorPath,
      ],
      cwd: repoRoot(),
      env: {
        AWS_ACCESS_KEY_ID: minio.accessKey,
        AWS_SECRET_ACCESS_KEY: minio.secretKey,
      },
      stdout: "piped",
      stderr: "piped",
    }).output();
    assertEquals(
      child.code,
      0,
      new TextDecoder().decode(child.stderr),
    );

    const output = await store.get(Key.parse(descriptor.output.artifact.key));
    assertEquals(
      new TextDecoder().decode(output.body),
      stableStringify({ value: 42 }),
    );
    assertEquals(output.info.contentType, "application/json");
  } finally {
    restoreEnvironment(accessKeyEnv, previousAccessKey);
    restoreEnvironment(secretAccessKeyEnv, previousSecretKey);
    await rm(root, { force: true, recursive: true });
    if (minio.container !== undefined) {
      await new Deno.Command("docker", { args: ["rm", "-f", minio.container] })
        .output()
        .catch(() => {});
    }
  }
});

async function startMinIO(t: Deno.TestContext): Promise<
  S3TestBackend | undefined
> {
  const configuredEndpoint = Deno.env.get("MASSIVE_TEST_S3_ENDPOINT");
  if (configuredEndpoint !== undefined) {
    const configuredAccessKey = Deno.env.get("MASSIVE_TEST_S3_ACCESS_KEY");
    const configuredSecretKey = Deno.env.get("MASSIVE_TEST_S3_SECRET_KEY");
    if (
      configuredAccessKey === undefined || configuredSecretKey === undefined
    ) {
      await skippedCapability(
        t,
        "MASSIVE_TEST_S3_ENDPOINT requires MASSIVE_TEST_S3_ACCESS_KEY and MASSIVE_TEST_S3_SECRET_KEY",
      );
      return undefined;
    }
    return {
      endpoint: configuredEndpoint,
      host: new URL(configuredEndpoint).host,
      accessKey: configuredAccessKey,
      secretKey: configuredSecretKey,
      accessKeyEnv: "MASSIVE_TEST_S3_ACCESS_KEY",
      secretAccessKeyEnv: "MASSIVE_TEST_S3_SECRET_KEY",
    };
  }

  const port = await freePort();
  const host = `127.0.0.1:${port}`;
  const container = `massive-runner-minio-${crypto.randomUUID()}`;
  const started = await new Deno.Command("docker", {
    args: [
      "run",
      "-d",
      "--rm",
      "--name",
      container,
      "-p",
      `${host}:9000`,
      "-e",
      `MINIO_ROOT_USER=${accessKey}`,
      "-e",
      `MINIO_ROOT_PASSWORD=${secretKey}`,
      "minio/minio",
      "server",
      "/data",
    ],
    stdout: "piped",
    stderr: "piped",
  }).output().catch(() => undefined);
  if (started === undefined || !started.success) {
    const diagnostic = started === undefined
      ? "docker is unavailable"
      : new TextDecoder().decode(started.stderr).trim();
    await skippedCapability(t, diagnostic);
    return undefined;
  }
  for (let attempts = 0; attempts < 60; attempts++) {
    const response = await fetch(`http://${host}/minio/health/live`).catch(
      () => undefined,
    );
    if (response?.ok) {
      return {
        endpoint: `http://${host}`,
        host,
        container,
        accessKey,
        secretKey,
        accessKeyEnv: dockerAccessKeyEnv,
        secretAccessKeyEnv: dockerSecretAccessKeyEnv,
      };
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  const logs = await new Deno.Command("docker", {
    args: ["logs", container],
    stdout: "piped",
    stderr: "piped",
  }).output();
  await new Deno.Command("docker", { args: ["rm", "-f", container] })
    .output()
    .catch(() => {});
  await skippedCapability(t, new TextDecoder().decode(logs.stderr));
  return undefined;
}

async function skippedCapability(
  t: Deno.TestContext,
  diagnostic: string,
): Promise<void> {
  await t.step({
    name:
      `skipped: real MinIO runner coverage unavailable: ${diagnostic.trim()}`,
    ignore: true,
    fn() {},
  });
}

async function freePort(): Promise<number> {
  const listener = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  try {
    const address = listener.addr;
    if (address.transport !== "tcp") throw new Error("expected TCP listener");
    return address.port;
  } finally {
    listener.close();
  }
}

interface S3TestBackend {
  readonly endpoint: string;
  readonly host: string;
  readonly container?: string;
  readonly accessKey: string;
  readonly secretKey: string;
  readonly accessKeyEnv: string;
  readonly secretAccessKeyEnv: string;
}

async function createBucket(
  backend: S3TestBackend,
  bucket: string,
): Promise<void> {
  const client = new S3Client({
    endpoint: backend.endpoint,
    region: "us-east-1",
    forcePathStyle: true,
    credentials: {
      accessKeyId: backend.accessKey,
      secretAccessKey: backend.secretKey,
    },
  });
  try {
    await client.send(new HeadBucketCommand({ Bucket: bucket }));
  } catch {
    await client.send(new CreateBucketCommand({ Bucket: bucket }));
  }
}

function repoRoot(): string {
  return fileURLToPath(new URL("../../../", import.meta.url));
}

function schemaKey(schema: string): string {
  return `blobs/sha256/${schema.slice("sha256:".length)}`;
}

function inputKey(): string {
  return "projects/sha256-9999999999999999999999999999999999999999999999999999999999999999/runs/run-s3-runner-fixture-0001/inputs/double.json";
}

function outputKey(): string {
  return "projects/sha256-9999999999999999999999999999999999999999999999999999999999999999/runs/run-s3-runner-fixture-0001/steps/double/1/output.json";
}

function restoreEnvironment(name: string, value: string | undefined): void {
  if (value === undefined) {
    Deno.env.delete(name);
  } else {
    Deno.env.set(name, value);
  }
}
