export function double(args: { readonly input: number }): number {
  return args.input * 2;
}

export function increment(args: { readonly input: number }): number {
  return args.input + 1;
}

export function label(args: { readonly input: number }): string {
  return `value:${args.input}`;
}
