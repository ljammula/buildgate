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
workspaces/<project>.files   the project's file names
```

## What reads it

| Test | Path the case takes |
|---|---|
| `TestRealApprovedPlansPassEveryPlanTimeCheck` (`internal/requestdriver`) | `AdvancePlanning` with a planner that returns the case's tickets; the request must reach `plan_review` on the first attempt with the case's ticket count |
| `TestRealApprovedPlansPassTheHandoverChecks` (`internal/requestsubmit`) | What `factoryd submit -spec-file -plan-dir` checks before recording a request |
| The mutation tests of `plan_corpus_mutation_test.go` (`internal/requestdriver`) | Each criterion, with a clause written into it that names a token that is not a file to change, a file only referred to or left alone, a file that changes, or a commit: the criterion-files check and the commit-message stripping must read the clause as they read it alone |

## Workspace: file names only

`workspaces/<project>.files` is `git ls-files` of the commit the project's
cases were planned against: paths only, one a line, sorted. Each test builds
the case's workspace from it, every path as an empty file, so a check that
asks whether a path exists finds what the planner's workspace had.

| Check | Runs as |
|---|---|
| A criterion's backticked name read as a repository file (`looksLikeRepoPath`), a name counted only because that file exists included | In full |
| A criterion's test name qualified by its package (`packageQualified`), a qualifier that is a directory holding Go files included | In full |
| Anything that reads a file's contents: the project's `.factory.yml` (`test_patterns`, `design_guide`), a design guide | At its defaults only: contents are not reproduced |

The workspace is a plain directory, not a git repository; no plan-time
check asks for one.

## Adding a case

Copy an approved request's `spec.md` and `tickets/` from its directory
under the data dir (`requests/<id>/`) into a new `<project>-<hash>/` here,
unchanged, and write its `case.json`. Check it names nothing private first.
A case of a new project also needs `workspaces/<project>.files`
(`git ls-files | sort` at the commit it was planned against).
A case a check refuses stays, listed in `planCorpusRefused` with the reason,
until the check is fixed.
