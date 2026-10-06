import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// Regression: screens used to render a caught exception directly as the only
// text (`Could not load run: ${error}`), with no headline, no next step and
// no way to see the raw error apart from the summary. Every such site goes
// through ErrorCallout / describeError (@/ui/ErrorDisplay) instead. This scans
// each non-test .tsx under features, shared and app for the literal patterns,
// so a future edit cannot reintroduce one without failing here. Tighten a
// pattern rather than allow-listing a file.
const ERROR_NAME = String.raw`(?:error|err|\w*Error)`;
const BARE_PATTERNS: readonly RegExp[] = [
  new RegExp(String.raw`String\(\s*${ERROR_NAME}\s*\)`),
  new RegExp(String.raw`\$\{\s*${ERROR_NAME}(?:\.message)?\s*\}`),
  new RegExp(String.raw`\{\s*${ERROR_NAME}\.message\s*\}`),
  new RegExp(String.raw`\b${ERROR_NAME}\.message\s*\}`),
];

const ROOTS = ["src/features", "src/shared", "src/app"];

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(path);
    return path.endsWith(".tsx") && !/\.test\.tsx$/.test(path) ? [path] : [];
  });
}

test("none of the 8 screens interpolate a bare error/_error into text", () => {
  const files = ROOTS.flatMap(sourceFiles);
  expect(files.length).toBeGreaterThan(50);
  const hits = files.flatMap((file) =>
    readFileSync(file, "utf8")
      .split("\n")
      .flatMap((line, i) =>
        BARE_PATTERNS.some((pattern) => pattern.test(line))
          ? [`${file}:${i + 1}: ${line.trim()}`]
          : [],
      ),
  );
  expect(
    hits,
    "route a caught error through ErrorCallout or describeError (@/ui/ErrorDisplay), not interpolation",
  ).toEqual([]);
});
