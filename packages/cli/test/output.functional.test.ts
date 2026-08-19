import {
  assertEquals,
  assertRejects,
  assertStringIncludes,
} from "jsr:@std/assert";
import { dirname } from "node:path";
import { datastore } from "@massive/sdk";
import {
  readRunManifestAt,
  UnsupportedRunManifestProtocolError,
} from "../src/run.ts";
import {
  copyFixture,
  fixtureEntry,
  join,
  listStoreKeys,
  makeStore,
  runCli,
} from "./harness.ts";

// WS-6.3 — default output is author-facing (no hashes, no store paths); verbose
// reveals artifact keys + hashes; `inspect` reports a past run without
// re-executing anything.

Deno.test("default run output hides hashes and store paths", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();

  const result = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    "run-quiet",
  ]);

  assertEquals(result.code, 0, result.stderr);
  // The result value is surfaced...
  assertStringIncludes(result.stdout, "value:41");
  // ...but not digests or absolute datastore paths.
  assertEquals(
    result.stdout.includes("sha256"),
    false,
    "quiet output must not print digests",
  );
  assertEquals(
    result.stdout.includes(store),
    false,
    "quiet output must not print the absolute store path",
  );
});

Deno.test("verbose run output reveals specHash, planHash, and artifact keys", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();

  const result = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    "run-verbose",
    "--verbose",
  ]);

  assertEquals(result.code, 0, result.stderr);
  assertStringIncludes(result.stdout, "specHash");
  assertStringIncludes(result.stdout, "planHash");
  assertStringIncludes(result.stdout, "sha256");
  // A project-scoped result key is disclosed under verbose.
  assertStringIncludes(result.stdout, "result.json");
});

Deno.test("massive inspect reports a past run without re-executing", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const runId = "run-inspect";

  const run = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    runId,
  ]);
  assertEquals(run.code, 0, run.stderr);

  const before = await listStoreKeys(store);

  const inspect = await runCli([
    "inspect",
    runId,
    "--store",
    store,
    "--project",
    "acme/wf",
  ]);
  assertEquals(inspect.code, 0, inspect.stderr);

  // inspect surfaces keys/hashes for the past run...
  assertStringIncludes(inspect.stdout, "result.json");
  assertStringIncludes(inspect.stdout, "sha256");

  // ...and writes no new run artifacts (no new run dir, no step spawned).
  const after = await listStoreKeys(store);
  assertEquals(after, before, "inspect must not create datastore artifacts");
});

Deno.test("run-manifest reader refuses retired and future transports before nested fields", async (t) => {
  for (
    const protocol of [
      { schemaVersion: 1, encoding: "json-v1" },
      { schemaVersion: 3, encoding: "json-v3" },
    ]
  ) {
    await t.step(`${protocol.encoding}`, async () => {
      const store = await makeStore();
      const key =
        "projects/sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/runs/protocol-reader/run-manifest.json";
      const path = join(store, key);
      await Deno.mkdir(dirname(path), { recursive: true });
      // The deliberately misleading nested data verifies that the shared
      // reader rejects the envelope before exposing run output to `run` or
      // `inspect`.
      await Deno.writeTextFile(
        path,
        JSON.stringify({
          schemaVersion: protocol.schemaVersion,
          encoding: protocol.encoding,
          planHash: "not-read",
          status: "succeeded",
          steps: [{ nodeId: "not-read", status: "succeeded" }],
          result: { key: "not-read", hash: "not-read" },
        }),
      );

      const error = await assertRejects(
        () => readRunManifestAt(datastore.local({ path: store }), key),
        UnsupportedRunManifestProtocolError,
      );
      assertStringIncludes(
        error.message,
        `schemaVersion ${protocol.schemaVersion}`,
      );
      assertStringIncludes(error.message, `encoding \"${protocol.encoding}\"`);
    });
  }
});

Deno.test("massive inspect reports an actionable error for retired and future manifests", async (t) => {
  for (
    const protocol of [
      { schemaVersion: 1, encoding: "json-v1" },
      { schemaVersion: 3, encoding: "json-v3" },
    ]
  ) {
    await t.step(`${protocol.encoding}`, async () => {
      const store = await makeStore();
      const runId = `run-inspect-${protocol.schemaVersion}`;
      const key =
        `projects/sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/runs/${runId}/run-manifest.json`;
      const manifestPath = join(store, key);
      await Deno.mkdir(dirname(manifestPath), { recursive: true });
      // A physical on-disk run artifact, consumed through the public CLI
      // subprocess—not an in-memory substitute.
      await Deno.writeTextFile(
        manifestPath,
        JSON.stringify({
          kind: "RunManifest",
          schemaVersion: protocol.schemaVersion,
          encoding: protocol.encoding,
          planHash:
            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
          projectKey:
            "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
          runId,
          status: "succeeded",
          steps: [],
        }),
      );

      const inspect = await runCli([
        "inspect",
        runId,
        "--store",
        store,
        "--project",
        "acme/wf",
      ]);
      assertEquals(inspect.code, 4, inspect.stderr);
      assertStringIncludes(inspect.stderr, "unsupported run manifest protocol");
      assertStringIncludes(
        inspect.stderr,
        `schemaVersion ${protocol.schemaVersion}`,
      );
      assertStringIncludes(inspect.stderr, `encoding \"${protocol.encoding}\"`);
    });
  }
});

Deno.test("massive inspect reports an actionable error for malformed v2 nested data", async () => {
  const store = await makeStore();
  const runId = "run-inspect-malformed-v2";
  const key =
    `projects/sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc/runs/${runId}/run-manifest.json`;
  const manifestPath = join(store, key);
  await Deno.mkdir(dirname(manifestPath), { recursive: true });

  // This is a persisted v2 envelope, but its nested output reference is not a
  // reference. inspect must report a manifest error instead of throwing while
  // traversing the untrusted nested data.
  await Deno.writeTextFile(
    manifestPath,
    JSON.stringify({
      kind: "RunManifest",
      schemaVersion: 2,
      encoding: "json-v2",
      planHash: "sha256:" + "a".repeat(64),
      projectKey: "sha256-" + "c".repeat(64),
      runId,
      status: "succeeded",
      decisions: [],
      steps: [{
        nodeId: "step",
        status: "succeeded",
        attempts: [{
          attempt: 1,
          status: "succeeded",
          input: {
            key: "inputs/step.json",
            hash: "sha256:" + "b".repeat(64),
            contentType: "application/json",
            schema: "sha256:" + "d".repeat(64),
          },
          output: {
            manifest: null,
            body: {
              key: "blobs/sha256/" + "e".repeat(64),
              hash: "sha256:" + "e".repeat(64),
              size: 12,
              contentType: "application/json",
            },
            schema: "sha256:" + "d".repeat(64),
          },
          diagnostic: "",
        }],
      }],
    }),
  );

  const inspect = await runCli([
    "inspect",
    runId,
    "--store",
    store,
    "--project",
    "acme/wf",
  ]);

  assertEquals(inspect.code, 4, inspect.stderr);
  assertStringIncludes(inspect.stderr, "invalid run manifest");
  assertStringIncludes(inspect.stderr, "cannot inspect run");
  assertStringIncludes(inspect.stderr, "next");
});

Deno.test("run-manifest reader rejects impossible decision states", async (t) => {
  const invalidDecisions = [
    {
      name: "selected without case",
      value: { nodeId: "route", status: "selected" },
    },
    {
      name: "failed without diagnostic",
      value: { nodeId: "route", status: "failed" },
    },
    {
      name: "skipped without reason",
      value: { nodeId: "route", status: "skipped" },
    },
    {
      name: "selected with failure fields",
      value: {
        nodeId: "route",
        status: "selected",
        selectedCase: "approved",
        diagnostic: "must not coexist",
      },
    },
  ] as const;

  for (const invalid of invalidDecisions) {
    await t.step(invalid.name, async () => {
      const store = await makeStore();
      const key =
        "projects/sha256-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee/runs/invalid-decision/run-manifest.json";
      const path = join(store, key);
      await Deno.mkdir(dirname(path), { recursive: true });
      await Deno.writeTextFile(
        path,
        JSON.stringify({
          kind: "RunManifest",
          schemaVersion: 2,
          encoding: "json-v2",
          planHash: "sha256:" + "a".repeat(64),
          projectKey: "sha256-" + "e".repeat(64),
          runId: "invalid-decision",
          status: "failed",
          decisions: [invalid.value],
          steps: [],
        }),
      );

      const error = await assertRejects(
        () => readRunManifestAt(datastore.local({ path: store }), key),
        Error,
      );
      assertStringIncludes(error.message, "invalid run manifest");
      assertStringIncludes(error.message, "decisions.0");
    });
  }
});

Deno.test("run-manifest reader accepts Go pending and pre-input failure shapes", async () => {
  const store = await makeStore();
  const key =
    "projects/sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/runs/run-in-progress/run-manifest.json";
  const path = join(store, key);
  await Deno.mkdir(dirname(path), { recursive: true });
  await Deno.writeTextFile(
    path,
    JSON.stringify({
      kind: "RunManifest",
      schemaVersion: 2,
      encoding: "json-v2",
      planHash: "sha256:" + "a".repeat(64),
      projectKey:
        "sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      runId: "run-in-progress",
      status: "failed",
      decisions: [],
      steps: [
        { nodeId: "pending", status: "pending", attempts: [] },
        {
          nodeId: "failed-before-input",
          status: "failed",
          attempts: [{
            attempt: 1,
            status: "failed",
            input: { key: "", hash: "", contentType: "", schema: "" },
            diagnostic: "input could not be assembled",
          }],
        },
      ],
    }),
  );

  const manifest = await readRunManifestAt(
    datastore.local({ path: store }),
    key,
  );

  assertEquals(manifest.steps[0]?.attempts, []);
  assertEquals(manifest.steps[1]?.attempts[0]?.input.key, "");
});

Deno.test("massive inspect rejects an unsafe run id before touching the filesystem", async () => {
  const store = await makeStore();

  const result = await runCli([
    "inspect",
    "../../../../etc",
    "--store",
    store,
    "--project",
    "acme/wf",
  ]);

  // A run id that is not a single safe path segment is a usage error (exit 2),
  // caught at the entry — not interpolated into a stat path.
  assertEquals(result.code, 2, result.stderr);
  assertStringIncludes(result.stderr, "invalid run id");
  assertStringIncludes(result.stderr, "next");
});

Deno.test("massive inspect errors when a run id exists under multiple projects", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const runId = "dup-run";

  // Same run id under two different projects -> two run dirs in the store.
  for (const project of ["acme/one", "acme/two"]) {
    const run = await runCli([
      "run",
      fixtureEntry(fixture),
      "--input",
      "20",
      "--store",
      store,
      "--project",
      project,
      "--run-id",
      runId,
    ]);
    assertEquals(run.code, 0, run.stderr);
  }

  // The manifest records only the normalized project key, so --project cannot be
  // matched without reimplementing that normalization: inspect must refuse and
  // list the candidates rather than silently pick the first.
  const inspect = await runCli([
    "inspect",
    runId,
    "--store",
    store,
    "--project",
    "acme/one",
  ]);
  assertEquals(inspect.code, 4, inspect.stderr);
  assertStringIncludes(inspect.stderr, "multiple projects");
  assertStringIncludes(inspect.stderr, "next");
});
