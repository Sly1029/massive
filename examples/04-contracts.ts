import {
  contract,
  env,
  net,
  secret,
  workflow,
} from "@massive/sdk";
import { z } from "zod";

const baseContract = contract({
  env: env.node({
    version: "22.12.0",
    packageManager: "pnpm",
    lockfile: "pnpm-lock.yaml",
  }),
  resources: { cpu: "0.5", memory: "512Mi" },
  network: net.denyAll(),
});

export function fetchProfile(
  { input }: { readonly input: { readonly userId: number } },
) {
  // A real implementation can use the declared API host and PROFILE_TOKEN.
  return { userId: input.userId, displayName: `user-${input.userId}` };
}

const Request = z.object({ userId: z.int() });
const Profile = z.object({ userId: z.int(), displayName: z.string() });

const graph = workflow({
  name: "contracts-example",
  input: Request,
  output: Profile,
  defaults: baseContract,
});

const profile = graph.step("fetchProfile", {
  input: Request,
  output: Profile,
  contract: baseContract.extend({
    resources: { memory: "1Gi" },
    secrets: [secret.ref("PROFILE_TOKEN", "profiles/api-token")],
    network: net.allow("profiles.example.com"),
  }),
  run: fetchProfile,
});

graph.start().to(profile).to(graph.end());

export default graph;
