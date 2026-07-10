import { defineWorkflowPackage, target } from "@massive/sdk";

// include lists only workflow.ts so the source-package hash tracks the workflow
// module alone: editing it is a cache miss (new specHash); editing this config
// is not part of the hash coverage.
export default defineWorkflowPackage({
  include: ["workflow.ts"],
  entrypoint: "./workflow.ts",
  targets: [target.local()],
});
