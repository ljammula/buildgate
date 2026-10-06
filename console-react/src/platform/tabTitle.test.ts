import { needsHumanTabTitle, updateNeedsHumanSignal } from "@/platform/tabTitle";

test("a failed poll always shows (?), never a stale count or zero", () => {
  expect(needsHumanTabTitle(5, true)).toBe("(?) Factory Console");
  expect(needsHumanTabTitle(null, true)).toBe("(?) Factory Console");
  expect(needsHumanTabTitle(0, true)).toBe("(?) Factory Console");
});

test("a positive count shows the count", () => {
  expect(needsHumanTabTitle(3, false)).toBe("(3) Factory Console");
});

test("a zero or unknown count with a successful poll shows the plain title", () => {
  expect(needsHumanTabTitle(0, false)).toBe("Factory Console");
  expect(needsHumanTabTitle(null, false)).toBe("Factory Console");
});

test("updateNeedsHumanSignal updates the title", () => {
  document.title = "old";
  updateNeedsHumanSignal(2, false);
  expect(document.title).toBe("(2) Factory Console");
});
