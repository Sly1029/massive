import { assertEquals, assertRejects } from "jsr:@std/assert";
import {
  ArtifactBodyConflictError,
  type ArtifactDestination,
  ArtifactIntegrityError,
  ArtifactManifestConflictError,
  type ArtifactProducer,
  ArtifactRuntime,
  ArtifactValidationError,
  JSON_CONTENT_TYPE,
  MANIFEST_CONTENT_TYPE,
} from "../src/artifact/runtime.ts";
import { blobKeySHA256Hex, Key } from "../src/datastore/key.ts";
import { LocalDatastoreClient } from "../src/datastore/local.ts";

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
      "projects/project/runs/run-1/steps/task/1/output-manifest.json",
    ),
    schema: SCHEMA_HASH,
  };
}

function producer(): ArtifactProducer {
  return {
    projectKey: "project",
    planHash: PLAN_HASH,
    runId: "run-1",
    nodeId: "task",
    attempt: 1,
  };
}
