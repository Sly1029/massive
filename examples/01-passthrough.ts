import { workflow } from "@massive/sdk";
import { z } from "zod";

// The smallest valid graph has no steps. The workflow input becomes its output.
const graph = workflow({
  name: "passthrough-example",
  input: z.object({ message: z.string() }),
  output: z.object({ message: z.string() }),
});

graph.start().to(graph.end());

export default graph;
