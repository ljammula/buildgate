import { changedFileLines } from "@/features/run-detail/diffFiles";

test("reads each file from its diff --git header, using the new name", () => {
  expect(
    changedFileLines([
      "diff --git a/old.go b/new.go",
      "--- a/old.go",
      "+++ b/new.go",
      "@@ -1 +1 @@",
      "diff --git a/b.go b/b.go",
    ]),
  ).toEqual([
    { name: "new.go", line: 0 },
    { name: "b.go", line: 4 },
  ]);
});

test("falls back to +++ headers when there is no diff --git line", () => {
  expect(changedFileLines(["--- a/x", "+++ b/x", "@@ -1 +1 @@"])).toEqual([{ name: "x", line: 1 }]);
});

test("finds none in text without headers", () => {
  expect(changedFileLines(["+added", "-removed"])).toEqual([]);
});
