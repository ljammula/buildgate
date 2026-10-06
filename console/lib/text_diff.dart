/// A minimal unified-diff-style line diff for the revision compare
/// view: no hunk headers, no context collapsing -- every
/// line of [oldText] and [newText], merged via a classic LCS backtrace,
/// prefixed '- '/'+ '/'  ' the way `git diff --no-color` marks a
/// changed/unchanged line. This is deliberately as simple as
/// diff_screen.dart's own UnifiedDiffView, which doesn't colorize or fold
/// context either -- the server has no diff route for revisions (only
/// each file's full content, old and current), so this fills that gap
/// client-side rather than reusing a run's server-computed diff text.
// _maxDiffCells caps the (m+1) x (n+1) LCS matrix unifiedLineDiff builds
// below. That matrix is one int per cell, allocated synchronously on the
// UI thread -- found in review: a rejected/current pair with several
// thousand lines each (plausible for an agent-generated plan file) could
// allocate hundreds of megabytes and freeze or crash the tab just from
// selecting it for comparison, with no size bound in place at all. A
// proper linear-space diff (Myers, Hirschberg) would remove the need for
// this cap entirely; refusing an oversized comparison with a clear
// message is the simpler fix that still makes the failure mode "you see
// a message," not "the tab hangs."
const _maxDiffCells = 4000000;

String unifiedLineDiff(String oldText, String newText) {
  final oldLines = oldText.split('\n');
  final newLines = newText.split('\n');
  final m = oldLines.length;
  final n = newLines.length;
  if ((m + 1) * (n + 1) > _maxDiffCells) {
    return '(diff not shown: $m and $n lines is too large to compare '
        'here -- open each revision separately instead)\n';
  }
  // dp[i][j] = LCS length of oldLines[i:] and newLines[j:].
  final dp = List.generate(m + 1, (_) => List<int>.filled(n + 1, 0));
  for (var i = m - 1; i >= 0; i--) {
    for (var j = n - 1; j >= 0; j--) {
      dp[i][j] = oldLines[i] == newLines[j]
          ? dp[i + 1][j + 1] + 1
          : (dp[i + 1][j] > dp[i][j + 1] ? dp[i + 1][j] : dp[i][j + 1]);
    }
  }
  final buffer = StringBuffer();
  var i = 0;
  var j = 0;
  while (i < m && j < n) {
    if (oldLines[i] == newLines[j]) {
      buffer.writeln('  ${oldLines[i]}');
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      buffer.writeln('- ${oldLines[i]}');
      i++;
    } else {
      buffer.writeln('+ ${newLines[j]}');
      j++;
    }
  }
  for (; i < m; i++) {
    buffer.writeln('- ${oldLines[i]}');
  }
  for (; j < n; j++) {
    buffer.writeln('+ ${newLines[j]}');
  }
  return buffer.toString();
}
