import { safeHttpUrl } from "@/domain/safeUrl";

test("accepts absolute http and https URLs", () => {
  expect(safeHttpUrl("https://github.com/acme/app/pull/7")).toBe(
    "https://github.com/acme/app/pull/7",
  );
  expect(safeHttpUrl("http://localhost:8233")).toBe("http://localhost:8233/");
  expect(safeHttpUrl("  HTTPS://Example.com/a b ")).toBe("https://example.com/a%20b");
});

test.each([
  "javascript:alert(1)",
  "JaVaScRiPt:alert(1)",
  "\tjavascript:alert(1)",
  "data:text/html,<script>alert(1)</script>",
  "vbscript:msgbox(1)",
  "//attacker.example/x",
  "/relative/path",
  "github.com/acme/app",
  "http:/one-slash.example",
  "https://",
  "",
])("refuses %j", (value) => {
  expect(safeHttpUrl(value)).toBeNull();
});
