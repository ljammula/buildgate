# Plan budget cases

Two recorded planner outputs for the per-ticket budget (`pr_budget_lines`,
default 400), each a corpus plan that was approved and built, with the
`Estimated-Changed-Lines:` header a budgeted planner writes added.

| Case | Plan | Estimates |
|---|---|---|
| `small` | one ticket | 180 |
| `large` | four tickets | 220, 310, 380, 150: each within 400 |

Layout is the plan corpus's (`spec.md`, `tickets/NNN.spec.md`, `case.json`);
the workspace file names come from `../plan_corpus/workspaces/<project>.files`.
