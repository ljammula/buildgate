import {
  decodeUtf8Escaping,
  escapeInvisible,
  segmentEscapes,
  pastesAsShown,
} from "@/domain/textEscape";

describe("decodeUtf8Escaping", () => {
  const show = (bytes: number[]): string => escapeInvisible(decodeUtf8Escaping(bytes));

  test("valid UTF-8 round-trips, including 2-, 3- and 4-byte sequences", () => {
    expect(show([0x68, 0xc3, 0xa9, 0xe6, 0x97, 0xa5, 0xf0, 0x9f, 0x98, 0x80])).toBe("hé日😀");
  });

  test("each invalid byte becomes an explicit \\xNN, never U+FFFD", () => {
    expect(show([0x80])).toBe(String.raw`\x80`);
    expect(show([0xff, 0xfe])).toBe(String.raw`\xFF\xFE`);
    // Truncated 3-byte sequence, then valid text.
    expect(show([0xe2, 0x82, 0x41])).toBe(String.raw`\xE2\x82A`);
    // Overlong NUL, surrogate (0xED 0xA0 0x80) and > U+10FFFF
    // (0xF4 0x90 0x80 0x80).
    expect(show([0xc0, 0x80])).toBe(String.raw`\xC0\x80`);
    expect(show([0xed, 0xa0, 0x80])).toBe(String.raw`\xED\xA0\x80`);
    expect(show([0xf4, 0x90, 0x80, 0x80])).toBe(String.raw`\xF4\x90\x80\x80`);
    // Lead byte at the very end of input.
    expect(show([0x41, 0xe2])).toBe(String.raw`A\xE2`);
  });

  test("a real U+FFFD and an invalid byte render differently", () => {
    expect(show([0xef, 0xbf, 0xbd])).toBe("�");
    expect(show([0xef, 0xbf])).toBe(String.raw`\xEF\xBF`);
  });

  test("the escapes are flagged as synthetic segments", () => {
    const segments = segmentEscapes(decodeUtf8Escaping([0x61, 0x80, 0x62]));
    expect(segments.map((s) => s.text)).toEqual(["a", String.raw`\x80`, "b"]);
    expect(segments.map((s) => s.escaped)).toEqual([false, true, false]);
  });
});

describe("escapeInvisible", () => {
  test("keeps ordinary text, newline and tab", () => {
    expect(escapeInvisible("a\tb\nc é 日本")).toBe("a\tb\nc é 日本");
  });

  test("escapes every Bidi_Control character", () => {
    const bidi: [number, string][] = [
      [0x061c, "61C"],
      [0x200e, "200E"],
      [0x200f, "200F"],
      [0x202a, "202A"],
      [0x202b, "202B"],
      [0x202c, "202C"],
      [0x202d, "202D"],
      [0x202e, "202E"],
      [0x2066, "2066"],
      [0x2067, "2067"],
      [0x2068, "2068"],
      [0x2069, "2069"],
    ];
    for (const [cp, want] of bidi) {
      expect(escapeInvisible(String.fromCodePoint(cp)), `U+${want}`).toBe(`\\u{${want}}`);
    }
  });

  test("escapes zero-width, format, separator and control characters", () => {
    const hidden = [
      0x00, 0x01, 0x0d, 0x1b, 0x7f, 0x85, 0x9f, 0x00ad, 0x0600, 0x180e, 0x200b, 0x200c, 0x200d,
      0x2028, 0x2029, 0x2060, 0x2064, 0x206a, 0x206f, 0x3164, 0xfeff, 0xffa0, 0xfff9, 0xe0001,
      0xe0041,
    ];
    for (const cp of hidden) {
      const segments = segmentEscapes(String.fromCodePoint(cp));
      expect(segments, `U+${cp.toString(16)}`).toHaveLength(1);
      expect(segments[0]?.escaped, `U+${cp.toString(16)}`).toBe(true);
    }
  });

  test("a hidden character inside a word is visible", () => {
    expect(escapeInvisible("go‮test")).toBe(String.raw`go\u{202E}test`);
  });
});

test("text is offered for copying only when it pastes exactly as it reads", () => {
  expect(pastesAsShown("factoryd retry req-1")).toBe(true);
  expect(pastesAsShown("https://github.com/acme/app/pull/7")).toBe(true);
  expect(pastesAsShown("")).toBe(false);
  // A zero-width space, a right-to-left override, a line break.
  expect(pastesAsShown("factoryd retry​ req-1")).toBe(false);
  expect(pastesAsShown("factoryd ‮retry")).toBe(false);
  expect(pastesAsShown("a\nrm -rf x")).toBe(false);
  expect(pastesAsShown("a\rb")).toBe(false);
});
