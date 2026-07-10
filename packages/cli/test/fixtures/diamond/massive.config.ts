import { defineWorkflowPackage, target } from "@massive/sdk";

export default defineWorkflowPackage({
  include: ["workflow.ts"],
  entrypoint: "./workflow.ts",
  targets: [target.local()],
});
