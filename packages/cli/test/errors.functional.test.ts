import { assert, assertEquals, assertStringIncludes } from "jsr:@std/assert";
import {
  copyFixture,
  findRunArtifact,
  fixtureEntry,
  makeEmptyPathDir,
  makeStore,
  runCli,
  type RunCliOptions,
} from "./harness.ts";

// Error shapes (WS-6.3). Every failure is layer-attributed and ends in an
// actionable `next:` line. Step-failure exit codes are the runner's own
// (64/65/66); pre-run categories get the CLI/SDK/Go codes (2/3/70).

interface Prepared {
  readonly args: string[];
  readonly options: RunCliOptions;
  readonly storeRoot: string;
}

interface ErrorCase {
  readonly name: string;
  readonly expectedCode: number;
  readonly expectSubstring?: string;
  readonly expectFailedManifest?: {
    readonly runId: string;
    readonly firstStep: string;
  };
  prepare(): Promise<Prepared>;
}

const cases: readonly ErrorCase[] = [
  {
    // SDK owns entrypoint ambiguity.
    name: "ambiguous export -> exit 3",
    expectedCode: 3,
    expectSubstring: "specify one of",
    async prepare(): Promise<Prepared> {
      const fixture = await copyFixture("ambiguous-export");
      const storeRoot = await makeStore();
      return {
        args: [
          "run",
          fixtureEntry(fixture),
          "--input",
          "1",
          "--store",
          storeRoot,
          "--project",
          "acme/wf",
        ],
        options: {},
        storeRoot,
      };
    },
  },
  {
    // SDK refuses a deployable target for a zero-config workflow.
    name: "zero-config --target argo -> exit 3",
    expectedCode: 3,
    async prepare(): Promise<Prepared> {
      const fixture = await copyFixture("zero-config");
      const storeRoot = await makeStore();
      return {
        args: [
          "run",
          fixtureEntry(fixture),
          "--input",
          "1",
          "--store",
          storeRoot,
          "--project",
          "acme/wf",
          "--target",
          "argo",
        ],
        options: {},
        storeRoot,
      };
    },
  },
  {
    // The runner validates the output schema at the step boundary -> exit 65.
    name: "schema-invalid step -> exit 65",
    expectedCode: 65,
    expectFailedManifest: { runId: "run-schema-invalid", firstStep: "double" },
    async prepare(): Promise<Prepared> {
      const fixture = await copyFixture("schema-invalid");
      const storeRoot = await makeStore();
      return {
        args: [
          "run",
          fixtureEntry(fixture),
          "--input",
          "20",
          "--store",
          storeRoot,
          "--project",
          "acme/wf",
          "--run-id",
          "run-schema-invalid",
        ],
        options: {},
        storeRoot,
      };
    },
  },
  {
    // CLI owns malformed input JSON.
    name: "malformed --input JSON -> exit 2",
    expectedCode: 2,
    async prepare(): Promise<Prepared> {
      const fixture = await copyFixture("linear-chain");
      const storeRoot = await makeStore();
      return {
        args: [
          "run",
          fixtureEntry(fixture),
          "--input",
          "{not-json",
          "--store",
          storeRoot,
          "--project",
          "acme/wf",
        ],
        options: {},
        storeRoot,
      };
    },
  },
  {
    // CLI preflight: no `go` on PATH and no prebuilt binary -> exit 70.
    name: "missing go toolchain -> exit 70",
    expectedCode: 70,
    async prepare(): Promise<Prepared> {
      const fixture = await copyFixture("linear-chain");
      const storeRoot = await makeStore();
      const emptyPath = await makeEmptyPathDir();
      return {
        args: [
          "run",
          fixtureEntry(fixture),
          "--input",
          "20",
          "--store",
          storeRoot,
          "--project",
          "acme/wf",
        ],
        // Hide every tool (including `go`) and skip the prebuilt-binary override
        // so the CLI must fall back to building with `go`, which is absent.
        options: { env: { PATH: emptyPath }, useOrchestratorBinary: false },
        storeRoot,
      };
    },
  },
];

for (const errorCase of cases) {
  Deno.test(`error shape: ${errorCase.name}`, async () => {
    const prepared = await errorCase.prepare();
    const result = await runCli(prepared.args, prepared.options);

    assertEquals(
      result.code,
      errorCase.expectedCode,
      `stdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
    );

    // Every error ends in an actionable next-step line.
    const combined = `${result.stdout}\n${result.stderr}`;
    assertStringIncludes(combined, "next");

    if (errorCase.expectSubstring !== undefined) {
      assertStringIncludes(combined, errorCase.expectSubstring);
    }

    if (errorCase.expectFailedManifest !== undefined) {
      const manifestPath = await findRunArtifact(
        prepared.storeRoot,
        errorCase.expectFailedManifest.runId,
        "run-manifest.json",
      );
      assert(
        manifestPath !== undefined,
        "a failed run should still record a manifest",
      );
      const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
        readonly status: string;
        readonly steps: readonly {
          readonly nodeId: string;
          readonly status: string;
        }[];
      };
      assertEquals(manifest.status, "failed");
      assertEquals(
        manifest.steps[0]?.nodeId,
        errorCase.expectFailedManifest.firstStep,
      );
      assertEquals(manifest.steps[0]?.status, "failed");
    }
  });
}
