import { digestText } from "@/domain/digest";

describe("digestText", () => {
  test("a short single-line text is returned whole", () => {
    expect(digestText("gh: not logged in")).toEqual({
      head: "gh: not logged in",
      truncated: false,
    });
  });

  test("a long text stops at its first sentence", () => {
    const text =
      "ticket 1 was accepted but its pull request could not be opened. Run `factoryd retry x` with pull requests enabled.";
    expect(digestText(text)).toEqual({
      head: "ticket 1 was accepted but its pull request could not be opened.",
      truncated: true,
    });
  });

  test("a second line is not part of the head", () => {
    expect(digestText("first line\nsecond line")).toEqual({ head: "first line", truncated: true });
  });

  test("a sentence longer than the limit is cut on a word with an ellipsis", () => {
    const text = `${"word ".repeat(60)}end`;
    const digest = digestText(text, 40);
    expect(digest.truncated).toBe(true);
    expect(digest.head.endsWith("…")).toBe(true);
    expect(digest.head.length).toBeLessThanOrEqual(41);
    expect(digest.head).not.toMatch(/wor…$/);
  });

  test("a dotted token is not a sentence break", () => {
    expect(digestText("see a.go for details").head).toBe("see a.go for details");
  });
});
