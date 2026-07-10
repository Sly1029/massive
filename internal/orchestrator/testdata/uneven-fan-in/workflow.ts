export function split(args: { readonly input: number }): number {
  return args.input;
}

export function short(args: { readonly input: number }): number {
  return args.input + 1;
}

export function long(args: { readonly input: number }): number {
  return args.input * 2;
}

export function longTail(args: { readonly input: number }): number {
  return args.input + 100;
}

export function merge(args: { readonly input: readonly number[] }): number {
  return args.input[0] + args.input[1];
}
