# Plan corpus

Real specs and plans that a plan-time check must accept.

| | |
|---|---|
| What a case is | The spec a human approved and the tickets the planner drafted for it, from a request that was then built |
| Where they come from | Requests on 11 open-source repositories: cobra, chi, gjson, click, humanize, mux, go-version, lo, more-itertools, cachetools, python-tabulate |
| Edited | Never. A case is copied as the factory recorded it; a check that refuses one is wrong, not the case |
| Selection | One plan for each distinct approved spec (the first in name order), which keeps every repository and every plan of more than one ticket |

## Layout

```text
<project>-<hash>/
  spec.md               the approved spec
  tickets/NNN.spec.md   the plan, as approved
  case.json             verify_command, project, tickets (the count)
```

## What reads it

| Test | Path the case takes |
|---|---|
| `TestRealApprovedPlansPassEveryPlanTimeCheck` (`internal/requestdriver`) | `AdvancePlanning` with a planner that returns the case's tickets; the request must reach `plan_review` on the first attempt with the case's ticket count |
| `TestRealApprovedPlansPassTheHandoverChecks` (`internal/requestsubmit`) | What `factoryd submit -spec-file -plan-dir` checks before recording a request |

## Limit: no repository

The repositories are not here, so both tests run against an empty
workspace. A check that looks a name up in the repository runs only in the
form it takes when the file is absent:

| Check | Not exercised |
|---|---|
| A criterion's backticked name read as a repository file (`looksLikeRepoPath`) | A name counted only because that file exists in the workspace |
| A criterion's test name qualified by its package (`packageQualified`) | A qualifier that is a directory of the workspace |
| The project's own `test_patterns` and `design_guide` (`.factory.yml`) | Any value but the defaults |

## Adding a case

Copy an approved request's `spec.md` and `tickets/` from its directory
under the data dir (`requests/<id>/`) into a new `<project>-<hash>/` here,
unchanged, and write its `case.json`. Check it names nothing private first.
A case a check refuses stays, listed in `planCorpusRefused` with the reason,
until the check is fixed.
