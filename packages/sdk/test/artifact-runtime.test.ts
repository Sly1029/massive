import {
  assertEquals,
  assertRejects,
  assertStringIncludes,
} from "jsr:@std/assert";
import {
  ArtifactBodyConflictError,
  type ArtifactDestination,
  ArtifactIntegrityError,
  ArtifactManifestConflictError,
  ArtifactNotFoundError,
  type ArtifactProducer,
  ArtifactRuntime,
  ArtifactValidationError,
  JSON_CONTENT_TYPE,
  MANIFEST_CONTENT_TYPE,
} from "../src/artifact/runtime.ts";
import { blobKeySHA256Hex, Key } from "../src/datastore/key.ts";
import { LocalDatastoreClient } from "../src/datastore/local.ts";
import { sha256RefBytes } from "../src/stable.ts";

const decoder = new TextDecoder();

const BODY = `{"value":42}`;
const BODY_HASH =
  "sha256:dc60e632a90329ccfd34fbe904d94704dbbb6669575185e26389854ff64139c3";
const SCHEMA =
  `{"additionalProperties":false,"properties":{"value":{"type":"integer"}},"required":["value"],"type":"object"}`;
const SCHEMA_HASH =
  "sha256:cc6d2156c280bb3efad77622be3c070cf9a18fbf7ddaf4db6a7c6988a417048a";
const PLAN_HASH =
  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const PROJECT_KEY =
  "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
Deno.test("artifact runtime publishes a Go-compatible manifest and converges after body-only recovery", async () => {
  await withRuntime(async (store, runtime) => {
    const bodyKey = blobKeySHA256Hex(BODY_HASH.slice("sha256:".length));
    await store.put(bodyKey, BODY, {
      contentType: JSON_CONTENT_TYPE,
      ifAbsent: true,
    });

    const first = await runtime.publishJson(destination(), producer(), BODY);
    const second = await runtime.publishJson(destination(), producer(), BODY);

    assertEquals(first, second);
    assertEquals(first.body.hash, BODY_HASH);
    assertEquals(first.body.key, bodyKey.toString());
    assertEquals(
      decoder.decode((await store.get(destination().manifestKey)).body),
      (await Deno.readTextFile(
        new URL(
          "../../../conformance/fixtures/artifacts/canonical-json/manifest.json",
          import.meta.url,
        ),
      )).trimEnd(),
    );

    const resolved = await runtime.resolveJson(destination(), producer());
    assertEquals(resolved.published, first);
    assertEquals(decoder.decode(resolved.body), BODY);
  });
});

Deno.test("artifact runtime converges concurrent local publishers without exposing incomplete metadata", async () => {
  await withRuntime(async (store) => {
    const [first, second] = await Promise.all([
      new ArtifactRuntime(store).publishJson(destination(), producer(), BODY),
      new ArtifactRuntime(store).publishJson(destination(), producer(), BODY),
    ]);
    assertEquals(first, second);
    assertEquals(
      (await store.get(destination().manifestKey)).info.contentType,
      MANIFEST_CONTENT_TYPE,
    );
    assertEquals(
      (await store.get(blobKeySHA256Hex(BODY_HASH.slice("sha256:".length))))
        .info.contentType,
      JSON_CONTENT_TYPE,
    );
  });
});

Deno.test("artifact runtime rejects a conflicting body at its content-addressed key", async () => {
  await withRuntime(async (store, runtime) => {
    await store.put(
      blobKeySHA256Hex(BODY_HASH.slice("sha256:".length)),
      `{"value":0}`,
      { contentType: JSON_CONTENT_TYPE, ifAbsent: true },
    );

    await assertRejects(
      () => runtime.publishJson(destination(), producer(), BODY),
      ArtifactBodyConflictError,
    );
  });
});

Deno.test("artifact runtime rejects a conflicting deterministic manifest", async () => {
  await withRuntime(async (_store, runtime) => {
    await runtime.publishJson(destination(), producer(), BODY);

    await assertRejects(
      () => runtime.publishJson(destination(), producer(), `{"value":43}`),
      ArtifactManifestConflictError,
    );
  });
});

Deno.test("artifact runtime refuses a tampered published body during resolution", async () => {
  await withRuntime(async (store, runtime) => {
    await runtime.publishJson(destination(), producer(), BODY);
    await store.put(
      blobKeySHA256Hex(BODY_HASH.slice("sha256:".length)),
      `{"value":0}`,
      { contentType: JSON_CONTENT_TYPE },
    );

    await assertRejects(
      () => runtime.resolveJson(destination(), producer()),
      ArtifactIntegrityError,
    );
  });
});

Deno.test("artifact runtime refuses a tampered manifest during resolution", async () => {
  await withRuntime(async (store, runtime) => {
    await runtime.publishJson(destination(), producer(), BODY);
    await store.put(destination().manifestKey, `{"body":{}}`, {
      contentType: MANIFEST_CONTENT_TYPE,
    });

    await assertRejects(
      () => runtime.resolveJson(destination(), producer()),
      ArtifactIntegrityError,
    );
  });
});

Deno.test("artifact runtime reports an absent manifest through the artifact error taxonomy", async () => {
  await withRuntime(async (_store, runtime) => {
    await assertRejects(
      () => runtime.resolveJson(destination(), producer()),
      ArtifactNotFoundError,
    );
  });
});

Deno.test("artifact runtime ships the canonical manifest schema with the SDK", async () => {
  const packaged = await Deno.readTextFile(
    new URL(
      "../src/artifact/data-artifact-manifest.schema.json",
      import.meta.url,
    ),
  );
  const canonical = await Deno.readTextFile(
    new URL(
      "../../../conformance/schema/data-artifact-manifest.schema.json",
      import.meta.url,
    ),
  );
  assertEquals(packaged, canonical);
});

Deno.test("artifact runtime validates the canonical output against its schema before publishing", async () => {
  await withRuntime(async (_store, runtime) => {
    await assertRejects(
      () => runtime.publishJson(destination(), producer(), `{"value":"wrong"}`),
      ArtifactValidationError,
    );
  });
});

Deno.test("artifact runtime rejects noncanonical JSON before publishing", async () => {
  await withRuntime(async (_store, runtime) => {
    await assertRejects(
      () => runtime.publishJson(destination(), producer(), `{ "value": 42 }`),
      ArtifactValidationError,
    );
  });
});

Deno.test("artifact runtime rejects a schema-valid noncanonical manifest", async () => {
  await withRuntime(async (store, runtime) => {
    await runtime.publishJson(destination(), producer(), BODY);
    const manifest = await store.get(destination().manifestKey);
    const noncanonical = decoder
      .decode(manifest.body)
      .replace('{"body"', '{ "body"');
    await store.put(destination().manifestKey, noncanonical, {
      contentType: MANIFEST_CONTENT_TYPE,
    });

    const error = await assertRejects(
      () => runtime.resolveJson(destination(), producer()),
      ArtifactIntegrityError,
    );
    assertStringIncludes(error.message, "not canonical JSON");
  });
});

Deno.test("artifact runtime rejects BOM and malformed UTF-8 at value, schema, and manifest boundaries", async () => {
  await withRuntime(async (store, runtime) => {
    for (
      const bytes of [
        new Uint8Array([0xef, 0xbb, 0xbf, 0x7b, 0x7d]),
        new Uint8Array([0xff]),
      ]
    ) {
      const error = await assertRejects(
        () => runtime.publishJson(destination(), producer(), bytes),
        ArtifactValidationError,
      );
      assertStringIncludes(error.message, "canonical JSON");
    }

    const invalidSchema = new Uint8Array([0xff]);
    const invalidSchemaRef = sha256RefBytes(invalidSchema);
    await store.put(
      blobKeySHA256Hex(invalidSchemaRef.slice("sha256:".length)),
      invalidSchema,
      { contentType: JSON_CONTENT_TYPE },
    );
    await assertRejects(
      () =>
        runtime.publishJson(
          { ...destination(), schema: invalidSchemaRef },
          producer(),
          BODY,
        ),
      ArtifactValidationError,
    );

    await runtime.publishJson(destination(), producer(), BODY);
    await store.put(destination().manifestKey, new Uint8Array([0xff]), {
      contentType: MANIFEST_CONTENT_TYPE,
    });
    await assertRejects(
      () => runtime.resolveJson(destination(), producer()),
      ArtifactIntegrityError,
    );
  });
});

Deno.test("artifact runtime maps only absent schemas to validation failures", async () => {
  const root = await Deno.makeTempDir({
    prefix: "massive-artifact-schema-errors-",
  });
  try {
    const missing = new ArtifactRuntime(
      new LocalDatastoreClient({ path: root }),
    );
    await assertRejects(
      () => missing.publishJson(destination(), producer(), BODY),
      ArtifactValidationError,
    );

    const blockedPath = `${root}.blocked`;
    await Deno.writeTextFile(blockedPath, "not a datastore directory");
    let thrown: unknown;
    try {
      await new ArtifactRuntime(new LocalDatastoreClient({ path: blockedPath }))
        .publishJson(destination(), producer(), BODY);
    } catch (error) {
      thrown = error;
    }
    assertEquals(thrown instanceof ArtifactValidationError, false);
    assertEquals(thrown instanceof Error, true);
  } finally {
    await Deno.remove(root, { recursive: true });
    await Deno.remove(`${root}.blocked`);
  }
});

Deno.test("artifact runtime enforces the shared producer identity contract before any datastore write", async () => {
  const fixture = await producerIdentityFixture();
  assertEquals(fixture.version, 2);
  assertEquals(fixture.contract, "artifact-producer-v2");

  for (const testCase of fixture.valid) {
    await withRuntime(async (_store, runtime) => {
      const producer = testCase.producer as ArtifactProducer;
      const published = await runtime.publishJson(
        destinationFor(producer),
        producer,
        BODY,
      );
      assertEquals(
        published.manifest.key,
        destinationFor(producer).manifestKey.toString(),
      );
    });
  }

  for (const testCase of fixture.invalid) {
    const root = await Deno.makeTempDir({
      prefix: "massive-invalid-producer-",
    });
    try {
      const runtime = new ArtifactRuntime(
        new LocalDatastoreClient({ path: root }),
      );
      const error = await assertRejects(
        () =>
          runtime.publishJson(
            destination(),
            testCase.producer as ArtifactProducer,
            BODY,
          ),
        ArtifactValidationError,
      );
      assertStringIncludes(error.message, "Invalid producer identity");
      assertEquals(
        await directoryEntries(root),
        [],
        `${testCase.name} must not create a body, manifest, or metadata record`,
      );
    } finally {
      await Deno.remove(root, { recursive: true });
    }
  }
});

Deno.test("artifact runtime checks an otherwise valid producer's exact destination before schema access", async () => {
  const root = await Deno.makeTempDir({
    prefix: "massive-producer-destination-",
  });
  try {
    const runtime = new ArtifactRuntime(
      new LocalDatastoreClient({ path: root }),
    );
    const error = await assertRejects(
      () =>
        runtime.publishJson(
          destination(),
          { ...producer(), nodeId: "another-task" },
          BODY,
        ),
      ArtifactValidationError,
    );
    assertStringIncludes(error.message, "does not match producer slot");
    assertEquals(await directoryEntries(root), []);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});

async function withRuntime(
  run: (store: LocalDatastoreClient, runtime: ArtifactRuntime) => Promise<void>,
): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-artifact-runtime-" });
  try {
    const store = new LocalDatastoreClient({ path: root });
    await store.put(
      blobKeySHA256Hex(SCHEMA_HASH.slice("sha256:".length)),
      SCHEMA,
      { contentType: JSON_CONTENT_TYPE, ifAbsent: true },
    );
    await run(store, new ArtifactRuntime(store));
  } finally {
    await Deno.remove(root, { recursive: true });
  }
}

function destination(): ArtifactDestination {
  return {
    manifestKey: Key.parse(
      `projects/${PROJECT_KEY}/runs/run-1/steps/task/1/output-manifest.json`,
    ),
    schema: SCHEMA_HASH,
  };
}

function producer(): ArtifactProducer {
  return {
    projectKey: PROJECT_KEY,
    planHash: PLAN_HASH,
    runId: "run-1",
    nodeId: "task",
    attempt: 1,
  };
}

function destinationFor(producer: ArtifactProducer): ArtifactDestination {
  const item = producer.scope === undefined
    ? ""
    : `/scopes${producer.scope.frames.map((frame) =>
      `/maps/${frame.mapId}/items/${frame.index}`
    ).join("")}`;
  return {
    manifestKey: Key.parse(
      `projects/${producer.projectKey}/runs/${producer.runId}/steps/${producer.nodeId}${item}/${producer.attempt}/output-manifest.json`,
    ),
    schema: SCHEMA_HASH,
  };
}

Deno.test("artifact runtime gives map items distinct immutable output slots without changing node IDs", async () => {
  await withRuntime(async (_store, runtime) => {
    const first = {
      ...producer(),
      scope: { frames: [{ kind: "map-item" as const, mapId: "fanout", index: 0 }] },
    };
    const second = {
      ...first,
      scope: { frames: [{ kind: "map-item" as const, mapId: "fanout", index: 1 }] },
    };

    assertEquals(
      destinationFor(first).manifestKey.toString(),
      `projects/${PROJECT_KEY}/runs/run-1/steps/task/scopes/maps/fanout/items/0/1/output-manifest.json`,
    );
    assertEquals(first.nodeId, second.nodeId);
    assertEquals(
      destinationFor(first).manifestKey.toString() ===
        destinationFor(second).manifestKey.toString(),
      false,
    );
    await runtime.publishJson(destinationFor(first), first, BODY);
    await runtime.publishJson(destinationFor(second), second, BODY);
  });
});

interface ProducerIdentityFixture {
  readonly version: number;
  readonly contract: string;
  readonly valid: readonly ProducerIdentityCase[];
  readonly invalid: readonly ProducerIdentityCase[];
}

interface ProducerIdentityCase {
  readonly name: string;
  readonly producer: unknown;
}

async function producerIdentityFixture(): Promise<ProducerIdentityFixture> {
  return JSON.parse(
    await Deno.readTextFile(
      new URL(
        "../../../conformance/fixtures/artifacts/producer-identities.json",
        import.meta.url,
      ),
    ),
  ) as ProducerIdentityFixture;
}

async function directoryEntries(path: string): Promise<string[]> {
  const entries: string[] = [];
  for await (const entry of Deno.readDir(path)) entries.push(entry.name);
  return entries.sort();
}
