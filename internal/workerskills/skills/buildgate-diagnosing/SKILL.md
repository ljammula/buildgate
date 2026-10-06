---
name: buildgate-diagnosing
description: Diagnosis loop for failures in a buildgate round whose cause is not obvious. Use when the verify command, an oracle or a reviewer reports a failure the output does not already explain - build a red/green loop before changing code.
license: MIT
---

# Buildgate diagnosing

Adapted from https://github.com/mattpocock/skills for an unattended, single-agent round. Redact any secret in output you quote.

If the failure output already names the cause (a compile error, a failing assertion with its line, a lint message), fix it directly with the smallest change and rerun the failing test; use the loop below only when it does not.

1. **Feedback loop first.** Find or build one command that is red on this exact failure and green once fixed: usually the verify command named in the build prompt's checklist, narrowed to the failing test, otherwise a failing test at the nearest seam or a minimal script. Make it fast and deterministic (pin time, seed randomness). If you cannot build one, say so in your final message and change only what the evidence supports.
2. **Reproduce and minimise.** Confirm the loop shows the reported failure, not a nearby different one. Cut inputs and callers until every remaining piece matters.
3. **Hypothesise.** List 3-5 ranked, falsifiable hypotheses before testing any.
4. **Instrument.** One probe per hypothesis, one variable at a time. Tag temporary logs with a unique prefix such as `[DEBUG-x1]` so they can be removed with one search.
5. **Fix with a regression test.** Write the test that fails for this bug before the fix, at a seam that exercises the real pattern; then fix; then rerun the loop. The harness runs the full verify command after your turn.
6. **Clean up.** Remove every `[DEBUG-` log and throwaway harness. State the root cause in one line in your final message.
