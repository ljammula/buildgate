/** A small pill beside a navigation label; nothing when the count is zero or unknown. */
export function NavCount({ count }: { readonly count: number | null }) {
  if (count === null || count === 0) return null;
  return (
    <span
      data-testid="nav-needs-you-count"
      aria-hidden="true"
      className="bg-tone-warning-soft text-tone-warning border-tone-warning-border ml-auto rounded-full border px-1.5 text-xs leading-4 font-semibold tabular-nums"
    >
      {count}
    </span>
  );
}
