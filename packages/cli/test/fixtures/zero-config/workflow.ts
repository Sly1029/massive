import { workflow } from "@massive/sdk";
import { z } from "zod";

// A single-file, zero-config workflow (no massive.config.ts). It resolves for
// `--target local`, but requesting a deployable target (`--target argo`) must be
// refused by the SDK entrypoint resolver before any emission.
const flow = workflow({
  name: "zero-config",
  input: z.number(),
  output: z.number(),
});
flow.start().to(flow.end());

export default flow;
