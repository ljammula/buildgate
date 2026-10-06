import { appendLog, maxLogBufferChars } from "@/features/run-detail/logBuffer";

test("appends below the cap", () => {
  expect(appendLog("abc", "def")).toBe("abcdef");
});

test("keeps only the most recent characters past the cap", () => {
  const text = appendLog("a".repeat(maxLogBufferChars), "xyz");
  expect(text).toHaveLength(maxLogBufferChars);
  expect(text.endsWith("axyz")).toBe(true);
});
