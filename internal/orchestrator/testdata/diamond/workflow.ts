export function split(args: { readonly input: number }): number {
  return args.input;
}

export function left(args: { readonly input: number }): number {
  return args.input + 1;
}

export function right(args: { readonly input: number }): number {
  return args.input * 3;
}

export function merge(args: { readonly input: readonly number[] }): number {
  return args.input[0] + args.input[1];
}
