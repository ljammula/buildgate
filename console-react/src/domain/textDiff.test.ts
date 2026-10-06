import { unifiedLineDiff } from "@/domain/textDiff";

test("marks unchanged, removed, and added lines", () => {
  expect(unifiedLineDiff("a\nb\nc", "a\nx\nc")).toBe("  a\n- b\n+ x\n  c\n");
});

test("an oversized comparison is refused, not computed", () => {
  // The (m+1) x (n+1) LCS matrix is one number per cell, allocated
  // synchronously on the UI thread. Without a bound, a large enough pair of
  // texts could allocate hundreds of megabytes and freeze the tab merely from
  // being selected for comparison: this asserts the refusal message appears
  // instead of the function attempting the full computation.
  const big = Array.from({ length: 3000 }, (_, i) => `line ${i}`).join("\n");
  const diff = unifiedLineDiff(big, big);
  expect(diff).toContain("diff not shown");
  expect(diff).not.toContain("  line 0");
});

test("a same-sized-but-under-the-cap comparison still computes normally", () => {
  expect(unifiedLineDiff("a\nb", "a\nc")).toBe("  a\n- b\n+ c\n");
});
