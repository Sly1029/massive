import { assert, assertEquals, assertStringIncludes } from "jsr:@std/assert";
import { join } from "node:path";
import {
  copyFixture,
  exists,
  fixtureEntry,
  makeStore,
  runCli,
} from "./harness.ts";

Deno.test("storage prefix flag and environment isolate bytes without changing workflow identity", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();

  const fromEnvironment = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/prefix-test",
    "--run-id",
    "env-prefix-run",
    "--json",
  ], { env: { MASSIVE_STORE_PREFIX: "tenants/environment" } });
  assertEquals(fromEnvironment.code, 0, fromEnvironment.stderr);

  const fromFlag = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--store-prefix",
    "tenants/flag",
    "--project",
    "acme/prefix-test",
    "--run-id",
    "flag-prefix-run",
    "--json",
  ], { env: { MASSIVE_STORE_PREFIX: "tenants/ignored" } });
  assertEquals(fromFlag.code, 0, fromFlag.stderr);

  assert(await exists(join(store, "tenants", "environment", "projects")));
  assert(await exists(join(store, "tenants", "flag", "projects")));
  assertEquals(await exists(join(store, "tenants", "ignored")), false);
  assertEquals(await exists(join(store, "projects")), false);
  const rootEntries: string[] = [];
  for await (const entry of Deno.readDir(store)) rootEntries.push(entry.name);
  assertEquals(rootEntries.sort(), ["tenants"]);

  const environmentOutcome = JSON.parse(fromEnvironment.stdout) as {
    keys: { specHash: string; planHash: string };
  };
  const flagOutcome = JSON.parse(fromFlag.stdout) as {
    keys: { specHash: string; planHash: string };
  };
  assert(typeof environmentOutcome.keys.specHash === "string");
  assert(environmentOutcome.keys.specHash !== "");
  assertEquals(flagOutcome.keys.specHash, environmentOutcome.keys.specHash);
  assertEquals(flagOutcome.keys.planHash, environmentOutcome.keys.planHash);
});

Deno.test("invalid storage prefixes fail before creating storage", async () => {
  for (
    const prefix of [
      "../escape",
      "/absolute",
      "a//b",
      "",
      " ",
      " leading",
      "line\nbreak",
      "C:/absolute",
      "control\u0085key",
    ]
  ) {
    const store = join(await makeStore(), "uncreated-store");
    const result = await runCli([
      "run",
      "unused.ts",
      "--store",
      store,
      "--store-prefix",
      prefix,
    ]);

    assertEquals(result.code, 2);
    assertStringIncludes(result.stderr, "invalid storage prefix");
    assertEquals(await exists(store), false);
  }
});
