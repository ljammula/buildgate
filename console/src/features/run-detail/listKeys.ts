/**
 * React keys for a list whose members come from the data: `keyOf(item)`, and
 * for a repeat of the same value `#2`, `#3`... in order. A key from the data
 * survives a reorder or an insert, where an array index would hand one row's
 * state (an open card, a focused button) to another.
 */
export function listKeys<T>(items: readonly T[], keyOf: (item: T) => string): string[] {
  const seen = new Map<string, number>();
  return items.map((item) => {
    const base = keyOf(item);
    const n = (seen.get(base) ?? 0) + 1;
    seen.set(base, n);
    return n === 1 ? base : `${base}#${n}`;
  });
}
