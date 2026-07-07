import { assert, assertEquals, assertInstanceOf } from "jsr:@std/assert";
import { rm } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { type Datastore, datastore } from "../src/datastore/index.ts";
import {
  parseStepInvocationDescriptor,
  type StepInvocationDescriptor,
} from "../src/runner/descriptor.ts";
import { executeStep } from "../src/runner/execute.ts";
import { runStep } from "../src/runner/main.ts";
import {
  DescriptorError,
  RUNNER_EXIT_CODES,
  StepExecutionError,
  StepSchemaValidationError,
  SymbolResolutionError,
} from "../src/runner/outcomes.ts";
import {
  type JsonValue,
  sha256RefText,
  stableStringify,
} from "../src/stable.ts";

const SOURCE_FETCH_CONTENT_TYPE =
  "application/vnd.massive.source-directory+json";
const PLAN_PACKAGE_HASH =
  "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd";
const VALUE_SCHEMA = {
  type: "object",
  additionalProperties: false,
  required: ["value"],
  properties: {
    value: { type: "number" },
  },
} satisfies JsonValue;

Deno.test("runner descriptor parser accepts every conformance descriptor fixture", async () => {
  const fixtures = await descriptorFixturePaths();
  assert(fixtures.length > 0, "expected at least one descriptor fixture");

  for (const path of fixtures) {
    const descriptor = await parseStepInvocationDescriptor(
      JSON.parse(await Deno.readTextFile(path)),
    );
    assertEquals(descriptor.kind, "StepInvocationDescriptor");
    assertEquals(descriptor.schemaVersion, 0);
  }
});

Deno.test("runner descriptor parser rejects malformed descriptors with a precise schema error", async () => {
  const fixture = JSON.parse(
    await Deno.readTextFile(
      new URL(
        "../../../conformance/fixtures/descriptors/linear-chain/descriptor.json",
        import.meta.url,
      ),
    ),
  ) as Record<string, unknown>;
  fixture.planHash = "not-a-hash";

  let thrown: unknown;
  try {
    await parseStepInvocationDescriptor(fixture);
  } catch (error) {
    thrown = error;
  }

  assertInstanceOf(thrown, DescriptorError);
  assertEquals(
    thrown.message,
    'StepInvocationDescriptor JSON schema violation at /planHash: must match pattern "^sha256:[0-9a-f]{64}$"',
  );
});

Deno.test("runner executes a real fixture step end to end against a temp local datastore", async () => {
  await withRunnerFixture(
    { input: { value: 21 }, stepExport: "double" },
    async ({ descriptor, descriptorPath, store }) => {
      const outcome = await runStep(descriptorPath);

      assertEquals(outcome.kind, "success");
      if (outcome.kind !== "success") return;

      const outputText = new TextDecoder().decode(
        await store.get(descriptor.output.artifact.key),
      );
      const expectedOutput = stableStringify({ value: 42 });
      assertEquals(descriptor.output.artifact.key, outputKey());
      assertEquals(outputText, expectedOutput);
      assertEquals(outcome.output, {
        key: outputKey(),
        hash: sha256RefText(expectedOutput),
        contentType: "application/json",
        schema: schemaRef(),
      });
    },
  );
});

Deno.test("runner reports descriptor/resolution failures with exit 64", async () => {
  await withRunnerFixture(
    { input: { value: 1 }, stepExport: "missing" },
    async ({ descriptor }) => {
      const outcome = await executeStep(descriptor);

      assertEquals(outcome.kind, "descriptor-resolution-failure");
      assertEquals(
        outcome.exitCode,
        RUNNER_EXIT_CODES.descriptorResolutionFailure,
      );
      if (outcome.kind !== "descriptor-resolution-failure") return;
      assertInstanceOf(outcome.error, SymbolResolutionError);
    },
  );
});

Deno.test("runner reports schema-validation failures with exit 65", async () => {
  await withRunnerFixture(
    { input: { value: "bad" }, stepExport: "double" },
    async ({ descriptor }) => {
      const outcome = await executeStep(descriptor);

      assertEquals(outcome.kind, "schema-validation-failure");
      assertEquals(outcome.exitCode, RUNNER_EXIT_CODES.schemaValidationFailure);
      if (outcome.kind !== "schema-validation-failure") return;
      assertInstanceOf(outcome.error, StepSchemaValidationError);
      assertEquals(outcome.error.role, "input");
    },
  );
});

Deno.test("runner reports step-execution failures with exit 66", async () => {
  await withRunnerFixture(
    { input: { value: 1 }, stepExport: "explode" },
    async ({ descriptor }) => {
      const outcome = await executeStep(descriptor);

      assertEquals(outcome.kind, "step-execution-failure");
      assertEquals(outcome.exitCode, RUNNER_EXIT_CODES.stepExecutionFailure);
      if (outcome.kind !== "step-execution-failure") return;
      assertInstanceOf(outcome.error, StepExecutionError);
    },
  );
});

async function withRunnerFixture(
  options: { readonly input: JsonValue; readonly stepExport: string },
  test: (fixture: {
    readonly descriptor: StepInvocationDescriptor;
    readonly descriptorPath: string;
    readonly store: Datastore;
  }) => Promise<void>,
): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-runner-" });
  try {
    const store = datastore.local({ path: join(root, "store") });
    const sourceRoot = fileURLToPath(new URL("./fixtures", import.meta.url));
    const sourcePointer = stableStringify({ sourceFetch: sourceRoot });
    const sourceHash = sha256RefText(sourcePointer);
    const inputText = stableStringify(options.input);
    const descriptor = await parseStepInvocationDescriptor({
      kind: "StepInvocationDescriptor",
      schemaVersion: 0,
      encoding: "json-v0",
      planHash:
        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      runId: "run-runner-fixture-0001",
      nodeId: "double",
      attempt: 1,
      symbol: {
        packageId: "ts-main",
        language: "typescript",
        module: "./runner-workflow.ts",
        export: options.stepExport,
      },
      sourcePackage: {
        packageId: "ts-main",
        language: "typescript",
        // packageHash (the plan's content-addressed package hash) is distinct
        // from sourceArchive.hash (the digest of the pointer artifact body);
        // the runner must not require them to be equal.
        packageHash: PLAN_PACKAGE_HASH,
        sourceArchive: {
          key: `packages/${hashPathSegment(PLAN_PACKAGE_HASH)}/source.tar.zst`,
          hash: sourceHash,
          contentType: SOURCE_FETCH_CONTENT_TYPE,
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
        schema: schemaRef(),
      },
      output: {
        artifact: {
          key: outputKey(),
          contentType: "application/json",
        },
        schema: schemaRef(),
      },
      channelReads: [],
      channelWrites: [],
      datastore: {
        kind: "local",
        path: join(root, "store"),
      },
    });

    await store.put(descriptor.sourcePackage.sourceArchive.key, sourcePointer);
    await store.put(schemaKey(schemaRef()), stableStringify(VALUE_SCHEMA));
    await store.put(descriptor.input.artifact.key, inputText);

    const descriptorPath = join(root, "descriptor.json");
    await Deno.writeTextFile(descriptorPath, stableStringify(descriptor));
    await test({ descriptor, descriptorPath, store });
  } finally {
    await rm(root, { force: true, recursive: true });
  }
}

async function descriptorFixturePaths(): Promise<string[]> {
  const root = fileURLToPath(
    new URL("../../../conformance/fixtures/descriptors", import.meta.url),
  );
  const paths: string[] = [];
  await collectJsonFiles(root, paths);
  return paths.sort();
}

async function collectJsonFiles(
  directory: string,
  paths: string[],
): Promise<void> {
  for await (const entry of Deno.readDir(directory)) {
    const path = join(directory, entry.name);
    if (entry.isDirectory) {
      await collectJsonFiles(path, paths);
    } else if (entry.isFile && entry.name.endsWith(".json")) {
      paths.push(path);
    }
  }
}

function inputKey(): string {
  return `${runPrefix()}/inputs/double.json`;
}

function outputKey(): string {
  return `${runPrefix()}/steps/double/1/output.json`;
}

function runPrefix(): string {
  return "projects/sha256-9999999999999999999999999999999999999999999999999999999999999999/runs/run-runner-fixture-0001";
}

function schemaRef(): string {
  return sha256RefText(stableStringify(VALUE_SCHEMA));
}

function schemaKey(ref: string): string {
  return `blobs/sha256/${ref.slice("sha256:".length)}`;
}

function hashPathSegment(ref: string): string {
  return ref.replace("sha256:", "sha256-");
}
