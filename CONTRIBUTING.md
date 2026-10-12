# Contributing to buildgate

Thanks for helping. This page covers how a change gets in: what to set up,
which checks to run, and what a pull request needs. How the repository is
laid out, and where each kind of change goes, is in
[`AGENTS.md`](AGENTS.md); it is written for people as much as for coding
agents, so read it before your first change.

## Before you start

- **Open an issue first** for anything larger than a small fix, so the
  approach is agreed before you write it.
- **Some things will not be merged**, by design rather than as gaps to fill:

  | Not accepted | Why |
  |---|---|
  | Automatic merge or deploy, or wiring a release decision to one | Merge and deploy stay human (`safety-contract.md`, SC-001) |
  | A way to run a coding agent outside the sandbox (a host-execution mode, an "unsandboxed" flag) | Containment is unconditional on every path (`containment-matrix.md`) |
  | A second worker launcher chosen by flag or config, or runtime plugin loading | By design (AGENTS.md, "Current state") |
  | Compatibility shims for retired flags or config keys | Retired names are deleted and the docs updated |

- **Safety changes need a contract review.** A change to sandboxing,
  network egress, credentials, worker containment or the release/merge gate
  starts from the matching row of [`safety-contract.md`](safety-contract.md)
  and [`containment-matrix.md`](containment-matrix.md). A change that
  weakens an invariant is discussed in an issue first, never made as a
  quiet code edit.

## Set up

| You need | For |
|---|---|
| Go 1.26 and a C toolchain (Xcode Command Line Tools on a Mac) | The daemon, and `-race` in `make verify` |
| The `temporal` CLI | `make verify`: without it the Temporal-backed tests skip and the run still passes, so install it |
| Node 20+ and npm | The console (`console/`) |
| Python 3 with `pytest` | The build scripts (`agent/pi/`) |
| Docker (colima on a Mac without Docker Desktop) | `make verify-live`, `make ci` and every live check |
| A model route: an OpenAI-compatible endpoint (OpenAI, Ollama, vLLM), Anthropic, a ChatGPT/Codex login or a GitHub Copilot login ([README, Model routes](README.md#model-routes)) | The live checks only |

`make install` sets up a whole machine, images and services included; follow
README's [Install](README.md#install) section, whose documented path is macOS
with Homebrew. You need it only for the live checks: `make verify`, the
console checks and the Python tests run from a plain clone.

## Make the change

- **Find the code** with the package index and AGENTS.md's "Finding the
  code" table:

  ```sh
  go list -f '{{.ImportPath}}: {{.Doc}}' ./cmd/... ./internal/...
  ```
- **Follow "Where a change goes"** in AGENTS.md. A new flag, command,
  harness, gate, activity or external dependency each has a fixed list of
  places to touch, and tests check most of them.
- **A behaviour change comes with a test** that fails without it.
- **Keep to the module boundaries** and design rules in AGENTS.md (function
  complexity, no mock framework, one package comment, interfaces only at a
  process or package boundary). `make verify` enforces them.
- **Keep the normative docs' checked lines intact.** `safety-contract.md`,
  `CLAIMS.md` and `containment-matrix.md` are parsed by tests in
  `internal/claims`: add prose around the `SC-NNN —` lines, transition
  bullets and table rows, never reformat them.
- **Update the docs in the same PR.** An operator-visible behaviour goes in
  `USAGE.md` or `USAGE_REFERENCE.md`; a change to what works or a known
  limit goes in both `STATUS.md` and README's "Status" and "Known limits".
  Docs describe the current state only: no history and no changelog.

## Run the checks

Run every toolchain your change touches. Judge a check by its exit status
and its full failure list, not by a truncated view of its output.

| You changed | Run |
|---|---|
| Anything, before asking for review | `make ci` (needs Docker and the `temporal` CLI) |
| Any Go code | `make verify` while you work |
| `console/` | `make console-test` |
| `agent/pi/` | `python3 -m pytest agent/pi/tests/` |
| The build loop, a policy gate, the sandbox runtime or the request pipeline | `make live-smoke` |
| Compose sidecars | `make live-compose` |
| The Go module fetch, the registry proxy's Go route or the worker's Go environment | `make live-private-module` |
| The PR-review pipeline | `make live-round` |
| The install or first-run path | The from-nothing run in AGENTS.md's "Live validation" |

The live checks need a working install and spend your own model budget;
`make live-round` also needs a second GitHub account. If you cannot run a
live check your change needs, say so in the pull request and a maintainer
runs it before merge. The from-nothing run uninstalls `factoryd` and purges
its state: back up `~/.config/factoryd` and `~/buildgate` first.

`make checks` runs the Go, console and Python checks as one command.
`git add` new files before running `make verify`: the `internal/claims`
tests scan tracked files only. A few tests are known to fail only under
full-suite load (AGENTS.md lists them); rerun such a test alone before
calling it a flake.

CI (`.github/workflows/ci.yml`) does not run on push or on a pull request,
so the checks above are the gate: run `make ci` yourself before asking for
review.

## Open a pull request

- **One change per pull request**, from a branch of your fork based on
  `main`.
- **The title says what is now true**, in the present tense, as the history
  does: "A closed pull request ends its request", not "Fix PR handling".
  Pull requests are squash-merged, so the title becomes the commit subject.
- **The description lists the checks you ran and their results**, and for a
  live check, the fixtures and the outcome.
- **No dated plans or run write-ups** in the repository, and no citations
  of private or unpublished documents. Don't cite a review finding by its
  label or number: say what it was.
  `TestNoPrivateNotesReferences` fails on either.
- **Review:** every pull request needs an approving review from a code owner
  (`.github/CODEOWNERS`) before it can merge.

## Releases

Maintainers cut releases as `vX.Y.Z` tags on `main`, only after
`make live-smoke` passes on that commit. `factoryd upgrade` installs the
highest one. Contributors do not tag.

## Reporting a security problem

Do not describe a vulnerability in a public issue or pull request. Open an
issue that says only that you have a security report, and a maintainer will
arrange a private channel.

## License

buildgate is licensed under the Apache License 2.0 ([`LICENSE`](LICENSE)).
By contributing, you agree that your contribution is licensed under the same
terms.
