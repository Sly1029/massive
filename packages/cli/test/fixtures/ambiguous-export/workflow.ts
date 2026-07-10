import { workflow } from "@massive/sdk";
import { z } from "zod";

// Two exported workflows and no default export: `massive run workflow.ts` must
// fail at entrypoint resolution (SDK MassiveError) with "specify one of: alpha,
// beta" and exit 3. Zero-config on purpose (no massive.config.ts).
export const alpha = workflow({
  name: "alpha",
  input: z.number(),
  output: z.number(),
});
alpha.start().to(alpha.end());

export const beta = workflow({
  name: "beta",
  input: z.number(),
  output: z.number(),
});
beta.start().to(beta.end());
