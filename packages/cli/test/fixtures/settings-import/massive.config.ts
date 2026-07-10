import { defineWorkflowPackage, target } from "@massive/sdk";
import { containerImage } from "./settings.ts";

// include covers only workflow.ts, so settings.ts is NOT part of the source
// package hash. A settings.ts edit is therefore invisible to the source hash and
// must be caught by the evaluated-config hash instead.
export default defineWorkflowPackage({
  include: ["workflow.ts"],
  entrypoint: "./workflow.ts",
  environment: { kind: "container", image: containerImage },
  targets: [target.local()],
});
