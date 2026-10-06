For EACH numbered criterion under "## Acceptance criteria" above, decide whether it can be
checked by a genuinely deterministic test -- one whose correct answer
you can state right now, without writing or running the implementation
first. A criterion qualifies only if you can point to one of these three
sources for the correct answer:

1. Derivable value -- the correct output for a concrete input can be
   stated directly from the spec itself or an external authority
   (arithmetic, a published table, an RFC example, a well-known
   algorithm) with no implementation needed.
2. Existing reference -- an old implementation already in this
   repository, a documented third-party API, or another already-correct
   system the change is replacing/porting, whose real output for a
   fixed set of inputs you can capture as a golden fixture.
3. Property -- a true-regardless-of-implementation invariant (a
   round-trip, sortedness, idempotence, a conservation law) that needs
   no external oracle at all, just the code's own behavior against
   itself.

If a criterion is a genuine judgment call with no independently
knowable "correct" answer (code style, clarity, "handles errors
gracefully", anything requiring your own opinion to evaluate) -- it does
NOT qualify. When in doubt, do NOT draft a test for it; a wrong or
overconfident oracle is worse than none.
