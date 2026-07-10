export function split(args: { readonly input: number }): number {
  return args.input;
}

export function a1(args: { readonly input: number }): number {
  return args.input + 1;
}

export function a2(args: { readonly input: number }): number {
  return args.input + 2;
}

export function b1(args: { readonly input: number }): number {
  return args.input + 3;
}

export function b2(args: { readonly input: number }): number {
  return args.input + 4;
}

export function mergeA(args: { readonly input: readonly number[] }): number {
  return args.input[0] + args.input[1];
}

export function mergeB(args: { readonly input: readonly number[] }): number {
  return args.input[0] + args.input[1];
}

export function mergeFinal(args: { readonly input: readonly number[] }): number {
  return args.input[0] + args.input[1];
}
