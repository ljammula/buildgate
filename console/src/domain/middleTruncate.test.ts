import { headTruncate, middleTruncate, shortPath } from "@/domain/middleTruncate";

const long = "add-subtract-numbers-to-add-py-add-a-sub-20261005-225506";

describe("middleTruncate", () => {
  test("keeps the head and the tail and never exceeds max", () => {
    const short = middleTruncate(long, 28);
    expect(short).toHaveLength(28);
    expect(short).toBe("add-subtract-n…261005-225506");
  });

  test("returns a value within max, or a max too small to cut, unchanged", () => {
    expect(middleTruncate("req-halted", 28)).toBe("req-halted");
    expect(middleTruncate("abcdefghij", 10)).toBe("abcdefghij");
    expect(middleTruncate("abcdefghij", 4)).toBe("abcdefghij");
  });
});

describe("headTruncate", () => {
  test("cuts to the head and appends the mark", () => {
    expect(headTruncate("run-20261005-225506-abcdef", 12, "…")).toBe("run-20261005…");
  });

  test("without a mark, cuts to exactly keep characters", () => {
    expect(headTruncate("0e2df58c76e6aabbccdd", 12)).toBe("0e2df58c76e6");
  });

  test("returns a value within keep unchanged, with no mark", () => {
    expect(headTruncate("0e2df58c76e6", 12, "…")).toBe("0e2df58c76e6");
    expect(headTruncate("", 12, "…")).toBe("");
  });
});

describe("shortPath", () => {
  test("keeps the last two segments behind a leading ellipsis", () => {
    expect(shortPath("/home/op/buildgate/console-polish-walk/workspace")).toBe(
      "…/console-polish-walk/workspace",
    );
  });
  test("keeps a path that is already short as it is", () => {
    expect(shortPath("/repos/app")).toBe("/repos/app");
    expect(shortPath("app")).toBe("app");
    expect(shortPath("")).toBe("");
  });
  test("keeps the number of segments asked for", () => {
    expect(shortPath("/a/b/c/d", 1)).toBe("…/d");
  });
});
