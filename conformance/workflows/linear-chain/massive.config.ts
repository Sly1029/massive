import { defineWorkflowPackage } from "@massive/sdk";

export default defineWorkflowPackage({
  include: ["workflow.ts"],
  entrypoint: "./workflow.ts",
});
