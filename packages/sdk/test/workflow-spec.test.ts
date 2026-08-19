import { assertEquals, assertRejects } from "jsr:@std/assert";
import {
  computeSpecHash,
  parseWorkflowSpec,
  parseWorkflowSpecText,
  type WorkflowSpec,
  WorkflowSpecError,
} from "../src/index.ts";

Deno.test("parseWorkflowSpecText accepts canonical WorkflowSpec emitted by Python GraphBuilder", async () => {
  const text = await Deno.readTextFile(
    new URL(
      "../../../conformance/fixtures/specs/python-linear/workflow-spec.json",
      import.meta.url,
    ),
  );

  const spec = await parseWorkflowSpecText(text.trimEnd());
  const step = spec.graph.nodes.find((node) => node.kind === "step");

  assertEquals(spec.workflow.name, "python-linear");
  assertEquals(step?.kind, "step");
  if (step?.kind !== "step") {
    throw new Error("Python fixture should emit a step node");
  }
  assertEquals(spec.symbols[step.symbolRef]?.language, "python");
  assertEquals(spec.sourcePackages["python-main"]?.language, "python");
});

Deno.test("parseWorkflowSpec accepts data-only Graph IR 0.2 routing", async () => {
  const fixture = JSON.parse(
    await Deno.readTextFile(
      new URL(
        "../../../conformance/fixtures/specs/exhaustive-decision/workflow-spec.json",
        import.meta.url,
      ),
    ),
  ) as Omit<WorkflowSpec, "specHash"> & { specHash?: string };
  const value: WorkflowSpec = {
    ...fixture,
    specHash: computeSpecHash(fixture),
  };

  const spec = await parseWorkflowSpec(value);
  const decision = spec.graph.nodes.find((node) => node.kind === "decision");
  const select = spec.graph.nodes.find((node) => node.kind === "select");

  assertEquals(decision?.kind, "decision");
  assertEquals(select?.kind, "select");
  if (decision?.kind !== "decision" || select?.kind !== "select") {
    throw new Error("fixture should preserve data-only routing nodes");
  }
  assertEquals(decision.cases.map((entry) => entry.tag), [
    "accepted",
    "rejected",
  ]);
  assertEquals(select.decisionRef, "route");
});

Deno.test("parseWorkflowSpec preserves the Graph IR 0.3 map contract", async () => {
  const fixture = JSON.parse(
    await Deno.readTextFile(
      new URL(
        "../../../conformance/fixtures/specs/finite-map/workflow-spec.json",
        import.meta.url,
      ),
    ),
  ) as Omit<WorkflowSpec, "specHash"> & { specHash?: string };
  const value: WorkflowSpec = {
    ...fixture,
    specHash: computeSpecHash(fixture),
  };

  const spec = await parseWorkflowSpec(value);
  const map = spec.graph.nodes.find((node) => node.kind === "map");
  assertEquals(spec.graph.irVersion, "0.3");
  assertEquals(map?.kind, "map");
  if (map?.kind !== "map") {
    throw new Error("fixture should preserve its finite map node");
  }
  assertEquals(map.maxConcurrency, 3);
  assertEquals(
    map.itemInputSchema,
    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
  );
});

Deno.test("parseWorkflowSpecText rejects malformed nested WorkflowSpec fields", async () => {
  const text = await emitPythonWorkflowSpec();
  const malformed = text.replace('"language":"python"', '"language":"ruby"');

  await assertRejects(
    () => parseWorkflowSpecText(malformed),
    WorkflowSpecError,
    "JSON schema violation",
  );
});

Deno.test("parseWorkflowSpecText rejects a WorkflowSpec whose hash does not match its fields", async () => {
  const text = await emitPythonWorkflowSpec();
  const corruptedHash = text.replace(
    /"specHash":"sha256:[0-9a-f]{64}"/,
    `"specHash":"sha256:${"0".repeat(64)}"`,
  );

  await assertRejects(
    () => parseWorkflowSpecText(corruptedHash),
    WorkflowSpecError,
    "specHash mismatch",
  );
});

Deno.test("parseWorkflowSpecText rejects noncanonical JSON bytes", async () => {
  const text = await emitPythonWorkflowSpec();

  await assertRejects(
    () => parseWorkflowSpecText(` ${text}`),
    WorkflowSpecError,
    "not canonical JSON",
  );
});

Deno.test("parseWorkflowSpecText rejects noncanonical numbers inside nested schemas", async () => {
  const text = await emitPythonWorkflowSpec();
  const noncanonicalNumber = text.replace(
    '"type":"integer"',
    '"examples":[1.5],"type":"integer"',
  );

  await assertRejects(
    () => parseWorkflowSpecText(noncanonicalNumber),
    WorkflowSpecError,
    "not canonical JSON",
  );
});

async function emitPythonWorkflowSpec(): Promise<string> {
  const program = String.raw`
import importlib.util
import sys
from pathlib import Path

from massive import source_package

fixture = Path("packages/python/tests/fixtures/emission_workflow.py").resolve()
module_spec = importlib.util.spec_from_file_location("workflow_spec_parser_fixture", fixture)
assert module_spec is not None
assert module_spec.loader is not None
module = importlib.util.module_from_spec(module_spec)
sys.modules[module_spec.name] = module
module_spec.loader.exec_module(module)
specification = module.graph.emit(
    source=source_package(
        root=fixture.parent,
        include=[fixture.name],
        package_id="python-main",
    )
)
sys.stdout.write(specification.to_json())
`;
  const output = await new Deno.Command("uv", {
    args: [
      "run",
      "--project",
      "packages/python",
      "--frozen",
      "python",
      "-c",
      program,
    ],
  }).output();

  if (!output.success) {
    throw new Error(new TextDecoder().decode(output.stderr));
  }

  return new TextDecoder().decode(output.stdout);
}
