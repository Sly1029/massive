import { workflow } from "@massive/sdk";
import { z } from "zod";

// Import sentinel probe (see cache.functional.test.ts / emit-cache-key test).
// The CLI's in-process emit imports this module with --allow-env/--allow-write
// and records one line; a cache hit must skip the import entirely, leaving the
// sentinel untouched.
try {
  const sentinel = Deno.env.get("MASSIVE_IMPORT_SENTINEL");
  if (sentinel !== undefined && sentinel !== "") {
    Deno.writeTextFileSync(sentinel, "imported\n", { append: true });
  }
} catch {
  // No env/write permission (the step runner): intentionally silent.
}

export function double(args: { readonly input: number }): number {
  return args.input * 2;
}

export function triple(args: { readonly input: number }): number {
  return args.input * 3;
}

// alpha: input 20 -> double -> 40.
const alphaFlow = workflow({
  name: "alpha",
  input: z.number(),
  output: z.number(),
});
const alphaDouble = alphaFlow.step("double", {
  input: z.number(),
  output: z.number(),
  run: double,
});
alphaFlow.start().to(alphaDouble).to(alphaFlow.end());

// beta: input 20 -> triple -> 60. A result distinct from alpha's proves run B
// executed beta's graph rather than reusing alpha's cached spec.
const betaFlow = workflow({
  name: "beta",
  input: z.number(),
  output: z.number(),
});
const betaTriple = betaFlow.step("triple", {
  input: z.number(),
  output: z.number(),
  run: triple,
});
betaFlow.start().to(betaTriple).to(betaFlow.end());

export const alpha = alphaFlow;
export const beta = betaFlow;
