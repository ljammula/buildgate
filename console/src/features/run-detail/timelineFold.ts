import type { TimelineRow } from "@/domain/runDetail";

/** Fewer trailing pending stages than this stay listed one by one. */
export const COLLAPSE_AT = 3;

/** Where the trailing run of untouched stages begins (rows.length when there is none). */
export function firstUntouchedTail(rows: readonly TimelineRow[]): number {
  let i = rows.length;
  while (i > 0) {
    const row = rows[i - 1];
    const untouched =
      row !== undefined &&
      row.glyph === "pending" &&
      row.subtitle === null &&
      row.subRows.length === 0 &&
      row.noteLines.length === 0;
    if (!untouched) break;
    i -= 1;
  }
  return i;
}
