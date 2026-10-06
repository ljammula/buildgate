import { needsHumanTabTitle, updateNeedsHumanSignal } from "@/platform/tabTitle";

test("a failed poll always shows (?), never a stale count or zero", () => {
  expect(needsHumanTabTitle(5, true)).toBe("(?) Buildgate");
  expect(needsHumanTabTitle(null, true)).toBe("(?) Buildgate");
  expect(needsHumanTabTitle(0, true)).toBe("(?) Buildgate");
});

test("a positive count shows the count", () => {
  expect(needsHumanTabTitle(3, false)).toBe("(3) Buildgate");
});

test("a zero or unknown count with a successful poll shows the plain title", () => {
  expect(needsHumanTabTitle(0, false)).toBe("Buildgate");
  expect(needsHumanTabTitle(null, false)).toBe("Buildgate");
});

test("updateNeedsHumanSignal updates the title", () => {
  document.title = "old";
  updateNeedsHumanSignal(2, false);
  expect(document.title).toBe("(2) Buildgate");
});
