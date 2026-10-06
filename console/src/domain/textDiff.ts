/**
 * A minimal unified-diff-style line diff for the revision compare view: no
 * hunk headers, no context collapsing. Every line of `oldText` and `newText`,
 * merged via a classic LCS backtrace, prefixed "- ", "+ " or "  " the way
 * `git diff --no-color` marks a changed or unchanged line. The server has no
 * diff route for revisions (only each file's full content, old and current),
 * so this fills that gap client-side rather than reusing a run's
 * server-computed diff text.
 */

// MAX_DIFF_CELLS caps the (m+1) x (n+1) LCS matrix unifiedLineDiff builds
// below. That matrix is one number per cell, allocated synchronously on the
// UI thread. Found in review: a rejected/current pair with several thousand
// lines each (plausible for an agent-generated plan file) could allocate
// hundreds of megabytes and freeze or crash the tab just from selecting it
// for comparison, with no size bound in place at all. A proper linear-space
// diff (Myers, Hirschberg) would remove the need for this cap entirely;
// refusing an oversized comparison with a clear message is the simpler fix
// that still makes the failure mode "you see a message," not "the tab hangs."
const MAX_DIFF_CELLS = 4_000_000;

export function unifiedLineDiff(oldText: string, newText: string): string {
  const oldLines = oldText.split("\n");
  const newLines = newText.split("\n");
  const m = oldLines.length;
  const n = newLines.length;
  if ((m + 1) * (n + 1) > MAX_DIFF_CELLS) {
    return (
      `(diff not shown: ${m} and ${n} lines is too large to compare ` +
      "here -- open each revision separately instead)\n"
    );
  }
  // dp[i * w + j] = LCS length of oldLines[i:] and newLines[j:].
  const w = n + 1;
  const dp = new Int32Array((m + 1) * w);
  const at = (i: number, j: number): number => dp[i * w + j] ?? 0;
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i * w + j] =
        oldLines[i] === newLines[j] ? at(i + 1, j + 1) + 1 : Math.max(at(i + 1, j), at(i, j + 1));
    }
  }
  let out = "";
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    if (oldLines[i] === newLines[j]) {
      out += `  ${oldLines[i]}\n`;
      i++;
      j++;
    } else if (at(i + 1, j) >= at(i, j + 1)) {
      out += `- ${oldLines[i]}\n`;
      i++;
    } else {
      out += `+ ${newLines[j]}\n`;
      j++;
    }
  }
  for (; i < m; i++) out += `- ${oldLines[i]}\n`;
  for (; j < n; j++) out += `+ ${newLines[j]}\n`;
  return out;
}
