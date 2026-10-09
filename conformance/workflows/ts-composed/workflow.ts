// The TypeScript twin of conformance/workflows/python-composed: both emit the
// graph in conformance/fixtures/composition/composed-graph.json.
import { workflow } from "@massive/sdk";
import { z } from "zod";

export function prepare(args: { readonly input: number }): number {
  return args.input * 10;
}

export function side(args: { readonly input: number }): number {
  return args.input + 3;
}

export function normalize(args: { readonly input: number }): number {
  return args.input + 1;
}

export function total(args: { readonly input: readonly number[] }): number {
  return args.input.reduce((sum, value) => sum + value, 0);
}

export function label(args: { readonly input: number }): string {
  return `value:${args.input}`;
}

export const normalizer = workflow({ name: "normalizer", input: z.int(), output: z.int() });
normalizer.start()
  .to(normalizer.step("normalize", { input: z.int(), output: z.int(), run: normalize }))
  .to(normalizer.end());

// Its entry step receives the parent's fan-in, and it calls another workflow.
export const totals = workflow({ name: "totals", input: z.array(z.int()), output: z.int() });
totals.start()
  .to(totals.step("total", { input: z.array(z.int()), output: z.int(), run: total }))
  .to(totals.call("again", normalizer))
  .to(totals.end());

export const composed = workflow({ name: "composed", input: z.int(), output: z.string() });
const prepared = composed.step("prepare", { input: z.int(), output: z.int(), run: prepare });
const upstream = composed.call("upstream", normalizer);
const sideStep = composed.step("side", { input: z.int(), output: z.int(), run: side });
composed.start().to(prepared).to(upstream);
composed.from(prepared).to(sideStep);
// "join" sorts before "upstream", so the consumer expands before its source.
composed.merge([upstream, sideStep])
  .to(composed.call("join", totals))
  .transform("label", { output: z.string(), run: label })
  .to(composed.end());
