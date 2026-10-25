# Plan budget cases

Two corpus plans (real approved and built plans from `../plan_corpus`) with an
`Estimated-Changed-Lines:` header added to each ticket, the header a budgeted
planner writes. The estimates were written by hand for these tests; they are
not recorded planner output.

| Case | Plan | Estimates |
|---|---|---|
| `small` | one ticket | 180 |
| `large` | four tickets | 390, 350, 380, 300: each within 400, and no two fit together in 400 |

Layout is the plan corpus's (`spec.md`, `tickets/NNN.spec.md`, `case.json`);
the workspace file names come from `../plan_corpus/workspaces/<project>.files`.
