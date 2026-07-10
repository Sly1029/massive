import { defineWorkflowPackage, target } from "@massive/sdk";

// One package, two named workflow exports sharing this config. include lists
// only workflow.ts, so both workflows have the same source-package hash, the
// same config hash, and the same targets — the emit cache key must distinguish
// them by entrypoint identity (the selected export) alone.
export default defineWorkflowPackage({
  include: ["workflow.ts"],
  entrypoint: "./workflow.ts#alpha",
  targets: [target.local()],
});
