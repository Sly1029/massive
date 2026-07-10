export function double(_args: { readonly input: number }): string {
  return "not-a-number";
}

export function increment(args: { readonly input: number }): number {
  return args.input + 1;
}

export function label(args: { readonly input: number }): string {
  return `value:${args.input}`;
}
