import type { TimelineRow } from "@/domain/runDetail";
import { firstUntouchedTail } from "@/features/run-detail/timelineFold";

const row = (
  rowKey: string,
  glyph: TimelineRow["glyph"],
  over: Partial<TimelineRow> = {},
): TimelineRow => ({
  rowKey,
  label: rowKey,
  glyph,
  durationText: null,
  subtitle: null,
  subRows: [],
  noteLines: [],
  ...over,
});

test("the tail starts after the last stage that has anything to show", () => {
  const rows = [row("a", "passed"), row("b", "running"), row("c", "pending"), row("d", "pending")];
  expect(firstUntouchedTail(rows)).toBe(2);
});

test("a pending stage with a subtitle or sub-rows is not untouched", () => {
  expect(firstUntouchedTail([row("a", "pending", { subtitle: "waiting" })])).toBe(1);
  expect(firstUntouchedTail([row("a", "passed"), row("b", "pending")])).toBe(1);
  expect(firstUntouchedTail([row("a", "pending"), row("b", "pending")])).toBe(0);
  expect(firstUntouchedTail([])).toBe(0);
});
