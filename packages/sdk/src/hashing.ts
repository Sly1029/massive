export interface HashingSpec<Recipe extends HashRecipe = HashRecipe> {
  readonly algorithm: "sha256";
  readonly canonicalization: "canonical-json-v0";
  readonly recipe: Recipe;
  readonly recipeVersion: 1;
}

export type HashRecipe =
  | "workflow-spec"
  | "workflow-plan"
  | "deployment-spec"
  | "source-package";

export const WORKFLOW_SPEC_HASHING = hashing("workflow-spec");
export const DEPLOYMENT_SPEC_HASHING = hashing("deployment-spec");
export const SOURCE_PACKAGE_HASHING = hashing("source-package");

function hashing<Recipe extends HashRecipe>(
  recipe: Recipe,
): HashingSpec<Recipe> {
  return Object.freeze({
    algorithm: "sha256",
    canonicalization: "canonical-json-v0",
    recipe,
    recipeVersion: 1,
  });
}
