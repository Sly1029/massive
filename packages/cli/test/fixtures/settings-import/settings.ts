// A module the config imports from. Editing this changes the EVALUATED config
// without changing massive.config.ts's own bytes — the case a bytes-based
// config hash would miss.
export const containerImage = "docker.io/library/node:20";
