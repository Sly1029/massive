import {
  assertEquals,
  assertRejects,
  assertThrows,
} from "jsr:@std/assert";
import { join } from "node:path";
import { z } from "zod";
import {
  emitWorkflowSpec,
  GraphValidationError,
  MassiveError,
  resolveWorkflowEntrypoint,
  workflow,
  type WorkflowBuilder,
  type WorkflowSpec,
} from "../src/index.ts";
import { stableStringify } from "../src/stable.ts";

const repository = new URL("../../../", import.meta.url).pathname;
const source = {
  root: new URL(".", import.meta.url).pathname,
  include: ["compose.test.ts"],
};

const increment = ({ input }: { readonly input: number }) => input + 1;
const sum = ({ input }: { readonly input: readonly number[] }) =>
  input.reduce((total, value) => total + value, 0);
const show = ({ input }: { readonly input: number }) => `value:${input}`;

function emit(builder: WorkflowBuilder<number, never>): Promise<WorkflowSpec> {
  return emitWorkflowSpec(builder, { source });
}

// Ids, kinds, resolved exports, merge inputs, and edges: the part of the IR both
// SDKs agree on (schema and contract refs differ between Zod and Pydantic).
function projection(spec: WorkflowSpec) {
  return {
    nodes: spec.graph.nodes.map((node) => ({
      id: node.id,
      kind: node.kind,
      ...(node.kind === "step"
        ? {
          export: spec.symbols[node.symbolRef]!.export,
          ...(node.mergeInputs === undefined
            ? {}
            : { mergeInputs: node.mergeInputs }),
        }
        : {}),
    })),
    edges: spec.graph.edges,
  };
}

function child(name: string) {
  const g = workflow({ name, input: z.int(), output: z.int() });
  g.start().to(g.step("increment", { input: z.int(), output: z.int(), run: increment }))
    .to(g.end());
  return g;
}

Deno.test("the TypeScript composed workflow emits the cross-SDK composition vector", async () => {
  const resolved = await resolveWorkflowEntrypoint(
    join(repository, "conformance/workflows/ts-composed"),
  );
  const spec = await emitWorkflowSpec(resolved.workflow, {
    source: resolved.source,
    package: resolved.package,
  });
  const vector = JSON.parse(
    await Deno.readTextFile(
      join(repository, "conformance/fixtures/composition/composed-graph.json"),
    ),
  );

  assertEquals(projection(spec), vector);
});

for (
  const [sourceId, joinId] of [["a-source", "z-join"], ["z-source", "a-join"]]
) {
  Deno.test(`a composed workflow emits its hand-inlined twin when ${sourceId} feeds ${joinId}`, async () => {
    const build = (inline: boolean) => {
      const g = workflow({ name: "parent", input: z.int(), output: z.string() });
      const split = g.step("split", { input: z.int(), output: z.int(), run: increment });
      const sibling = g.step("sibling", { input: z.int(), output: z.int(), run: increment });
      g.start().to(split).to(sibling);
      let upstream;
      let joined;
      if (inline) {
        upstream = g.step(`${sourceId}--increment`, {
          input: z.int(),
          output: z.int(),
          run: increment,
          export: "increment",
        });
        joined = g.step(`${joinId}--sum`, {
          input: z.array(z.int()),
          output: z.int(),
          run: sum,
          export: "sum",
        });
      } else {
        upstream = g.call(sourceId, child("source"));
        const totals = workflow({ name: "totals", input: z.array(z.int()), output: z.int() });
        totals.start()
          .to(totals.step("sum", { input: z.array(z.int()), output: z.int(), run: sum }))
          .to(totals.end());
        joined = g.call(joinId, totals);
      }
      g.from(split).to(upstream);
      g.merge([upstream, sibling]).to(joined)
        .transform("show", { output: z.string(), run: show })
        .to(g.end());
      return g;
    };

    const composed = await emit(build(false));

    assertEquals(stableStringify(composed), stableStringify(await emit(build(true))));
    const join = composed.graph.nodes.find((node) => node.id === `${joinId}--sum`);
    assertEquals(
      join?.kind === "step" ? join.mergeInputs : undefined,
      [`${sourceId}--increment`, "sibling"],
    );
  });
}

Deno.test("transform emits a named step whose input schema is its producer's output", async () => {
  const build = (transform: boolean) => {
    const g = workflow({ name: "transform", input: z.int(), output: z.string() });
    if (transform) {
      g.start().transform("show", { output: z.string(), run: show }).to(g.end());
    } else {
      g.start().to(g.step("show", { input: z.int(), output: z.string(), run: show }))
        .to(g.end());
    }
    return g;
  };

  assertEquals(
    stableStringify(await emit(build(true))),
    stableStringify(await emit(build(false))),
  );
});

Deno.test("a called workflow can be reused, nested, and keeps its own steps' exports", async () => {
  const inner = child("inner");
  const middle = workflow({ name: "middle", input: z.int(), output: z.int() });
  middle.start().to(middle.call("deep", inner)).to(middle.end());
  const g = workflow({ name: "outer", input: z.int(), output: z.int() });
  g.start().to(g.call("first", middle)).to(g.call("second", inner)).to(g.end());

  const spec = await emit(g);

  assertEquals(projection(spec).nodes.map((node) => [node.id, node.export]), [
    ["__start", undefined],
    ["first--deep--increment", "increment"],
    ["second--increment", "increment"],
    ["__end", undefined],
  ]);
  assertEquals(Object.keys(spec.symbols), ["ts-main:./workflow.ts#increment"]);
});

Deno.test("calls reject children without one entry and one exit", async () => {
  const twoEntries = workflow({ name: "two-entries", input: z.int(), output: z.int() });
  const first = twoEntries.step("first", { input: z.int(), output: z.int(), run: increment });
  const second = twoEntries.step("second", { input: z.int(), output: z.int(), run: increment });
  twoEntries.start().to(first).to(twoEntries.end());
  twoEntries.start().to(second).to(twoEntries.end());
  const direct = workflow({ name: "direct", input: z.int(), output: z.int() });
  direct.start().to(direct.end());

  for (const invalid of [twoEntries, direct]) {
    const g = workflow({ name: "parent", input: z.int(), output: z.int() });
    g.start().to(g.call("child", invalid)).to(g.end());
    await assertRejects(
      () => emit(g),
      GraphValidationError,
      "exactly one start successor and one end predecessor",
    );
  }
});

Deno.test("calls reject recursion, scoped id collisions, and over-long scoped ids", async () => {
  const a = workflow({ name: "a", input: z.int(), output: z.int() });
  const b = workflow({ name: "b", input: z.int(), output: z.int() });
  a.start().to(a.call("to-b", b)).to(a.end());
  b.start().to(b.call("to-a", a)).to(b.end());
  await assertRejects(() => emit(a), GraphValidationError, "Recursive workflow call");

  const colliding = workflow({ name: "colliding", input: z.int(), output: z.int() });
  colliding.start().to(colliding.call("c", child("child")))
    .to(colliding.step("c--increment", { input: z.int(), output: z.int(), run: increment }))
    .to(colliding.end());
  await assertRejects(() => emit(colliding), GraphValidationError, "duplicate scoped node id");

  for (const [length, valid] of [[117, true], [118, false]] as const) {
    const g = workflow({ name: "long", input: z.int(), output: z.int() });
    g.start().to(g.call("c".repeat(length), child("child"))).to(g.end());
    if (valid) {
      await emit(g);
      continue;
    }
    await assertRejects(
      () => emit(g),
      GraphValidationError,
      "as a 129-character id",
    );
  }
});

Deno.test("calls need one inbound edge unless they consume a merge", async () => {
  const g = workflow({ name: "two-inbound", input: z.int(), output: z.int() });
  const split = g.step("split", { input: z.int(), output: z.int(), run: increment });
  const left = g.step("left", { input: z.int(), output: z.int(), run: increment });
  const right = g.step("right", { input: z.int(), output: z.int(), run: increment });
  const called = g.call("child", child("child"));
  g.start().to(split).to(left).to(called).to(g.end());
  g.from(split).to(right).to(called);

  await assertRejects(() => emit(g), GraphValidationError, "has 2 incoming edges");
  assertThrows(
    () => g.call("bad/id", child("child")),
    GraphValidationError,
    "must be 1-128 characters",
  );
});

Deno.test("the resolver requires every called step's run function to be exported", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-compose-" });
  try {
    const sdkUrl = new URL("../src/index.ts", import.meta.url).href;
    const zodUrl = new URL("../../../node_modules/zod/index.js", import.meta.url).href;
    const file = join(root, "workflow.ts");
    await Deno.writeTextFile(
      file,
      `import { workflow } from ${JSON.stringify(sdkUrl)};
import { z } from ${JSON.stringify(zodUrl)};

const hidden = ({ input }: { readonly input: number }) => input;
const inner = workflow({ name: "inner", input: z.int(), output: z.int() });
inner.start().to(inner.step("hidden", { input: z.int(), output: z.int(), run: hidden })).to(inner.end());
export const outer = workflow({ name: "outer", input: z.int(), output: z.int() });
outer.start().to(outer.call("child", inner)).to(outer.end());
`,
    );

    await assertRejects(
      () => resolveWorkflowEntrypoint(file),
      MassiveError,
      `step "child--hidden" must use the run function exported as "hidden"`,
    );
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});
