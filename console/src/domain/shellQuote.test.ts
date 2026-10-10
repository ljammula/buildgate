import { shellJoin, shellQuote } from "@/domain/shellQuote";

test("a plain argument is left as it is", () => {
  expect(shellQuote("go")).toBe("go");
  expect(shellQuote("./internal/api/...")).toBe("./internal/api/...");
  expect(shellQuote("-run=TestX")).toBe("-run=TestX");
});

test("an argument a shell would split or expand is single-quoted", () => {
  expect(shellQuote("two words")).toBe("'two words'");
  expect(shellQuote("")).toBe("''");
  expect(shellQuote("$HOME")).toBe("'$HOME'");
  expect(shellQuote("a;b")).toBe("'a;b'");
  expect(shellQuote("it's")).toBe("'it'\\''s'");
});

test("joined arguments read back as the same arguments", () => {
  expect(shellJoin(["sh", "-c", "go test ./... && echo done"])).toBe(
    "sh -c 'go test ./... && echo done'",
  );
});
