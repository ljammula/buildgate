# AGENTS.md

Instructions for any AI coding agent (Claude Code, GitHub Copilot, Codex,
or similar) working in this repository.

## What this is

`buildgate` is an unattended agent factory: it takes a ticket +
spec and drives a coding agent through a sandboxed build/verify loop to
an accepted, mergeable state — with merge and deploy staying permanently
human-gated by design, not a milestone still to reach. Go orchestration
daemon (`factoryd`, `cmd/factoryd` + `internal/*`), a React + TypeScript
operator console (`console/`), and vendored build scripts
(`agent/pi/`).

Read [`README.md`](README.md) first for the real architecture, what is
live-proven, and the known limits. This file is agent-operating instructions
only — it does not restate that content.

## Current state

- Latest release tag `m6`; `main` may be ahead of it. The status table and
  known limits are in [`STATUS.md`](STATUS.md), summarised in README's
  "Status" and "Known limits" sections — update both in the same PR as a
  change that alters either.
- A request runs three model **roles** (`planning`, `execution`, `review`),
  each bound in the session config to a route, a model and a coding-agent
  **harness** (`pi` default, `pifork`, `codex`, `copilot`). One execution
  path runs every build: Temporal, in a per-run worktree. A run whose Temporal
  is unreachable halts (`FACTORYD_AUTOSTART=0` with no `-temporal-address` is
  refused).
- Every worker container is created by the OpenShell gateway
  (`internal/openshell`, images pinned by digest), never by `docker run`. A
  worker has no network; its model calls leave through its sandbox's
  supervisor under a per-run policy and are counted by buildgate's meter
  (`internal/meter`, `cmd/factoryd-meter`). `factoryd doctor -fix`, `worker`
  and a single-ticket run start the gateway and the meter.
- Deliberately not built (don't add them): automatic merge/deploy, a
  host-execution path, a second worker launcher selectable by flag or config, runtime plugin loading, backward-compatibility shims
  for retired flags or config keys (delete them and update the docs).

## Before changing anything here

This repo enforces its own safety properties on itself. Two files are
**normative**, not descriptive, and are checked mechanically:

- [`safety-contract.md`](safety-contract.md) — trust boundaries, threat
  model, invariants SC-001–SC-015, the full run state graph. A change
  that weakens an invariant needs a new contract review, not a quiet
  code edit.
- [`CLAIMS.md`](CLAIMS.md) — every normative claim mapped to the test
  that enforces it.

`internal/claims/safety_contract_test.go` and
`internal/claims/claims_test.go` parse both files structurally (required
section headers, exact `SC-NNN —` invariant lines, exact `` `draft ->
product_policy_check` `` -style transition bullets in the first, every
backtick-wrapped `` `TestXxx` `` name resolving to a real Go test in the
second). If you edit either doc, keep those exact tokens intact — add
prose or diagrams around them, don't reformat the checked lines/rows
themselves. Same story for `containment-matrix.md`
(`internal/claims/containment_matrix_test.go` requires exact
`| Filesystem |`-style table rows).

If a change you're making touches sandboxing, network egress, credential
handling, worker containment, or the release/merge gate, read the
relevant row in `safety-contract.md`'s trust-boundary table and
`containment-matrix.md` first. This is the one part of the repo where
"looks like it works" is not the bar — a regression here is a real
security boundary, not a bug.

## Repo layout

| Path | What it is |
|---|---|
| `cmd/factoryd/` | The daemon's CLI entry points and subcommands |
| `internal/` | Go orchestration: run state machine, policy gates, sandbox launcher, Temporal workflows, evidence, release/kill-switch, request pipeline |
| `agent/pi/` | Vendored build-loop scripts (Python) that `factoryd` shells out to, for every harness: each job passes its role's `--harness pi\|pifork\|codex\|copilot`, which selects the adapter in `scripts/harness_adapters.py`. Has its own `README.md` and test suite |
| `console/` | React + TypeScript operator console (Vite; Node 20+ and npm). Its own `README.md` is the module map and conventions |
| `internal/operatorskill/buildgate/SKILL.md` | The `buildgate` agent skill operators install with `factoryd install-skill`: an allowlist of `factoryd` commands their Copilot/Claude Code/Codex may run. Embedded in the binary, so it must match the CLI of the same commit |
| `doc/designs/` | Living design docs for shipped subsystems (e.g. the pifork harness) |
| `data/` | **Runtime state, not source.** Queue entries, run records, workspaces, tickets. Mostly gitignored; don't treat files under here as things to "clean up" as code |
| `dist/` | Build output, gitignored |

Dated, closed-out planning/review docs (gap analyses, proving-ground
run summaries, superseded plans) do not live in this repo. If you're
about to write a dated `plan-YYYY-MM-DD-*.md` or a one-off validation
record, keep it out of here. Don't cite such docs from code or docs
either: this repo is synced to places that can't read them, and
`TestNoPrivateNotesReferences` fails on a reference. State the reason
inline instead.

## Build, test, verify

Three separate toolchains, not unified under one command:

```sh
make verify        # Go: fmt-check, vet, go test -race ./...
make console-test   # console: cd console && npm ci && npm run check
python3 -m pytest agent/pi/tests/       # Python, agent/pi (every harness adapter)
```

Run whichever toolchain(s) your change actually touches. `make verify`
alone does not cover a console or Python change, and vice versa.
`.github/workflows/ci.yml` is manual-only (`workflow_dispatch`) by
operator decision — it does not run automatically on push to `main` or on
a PR push unless the optional, currently-unused self-hosted runner is
registered (see README's Build & test section) — so running `make ci`
yourself before claiming a change is done is on you, not CI.

Test gotchas that have broken `main` before:

- Judge `go test`/`make verify` by exit status and the full `--- FAIL` list,
  never by a head-truncated grep of the output.
- `make verify` runs `cmd/factoryd` tests as 2 to 8 concurrent processes,
  sized from the machine's cores and memory (`scripts/test-sharded.sh`;
  `TEST_SHARDS=<n>` overrides): a fixture path, port or package global shared
  across tests needs a per-process name, or tests that swap it run
  sequentially. A test that starts a process waits for it to answer (30 s,
  `waitForServeHealthy`), never a couple of seconds: under eight shards a
  start is slow. Tests are dealt to shards on each run from the measured
  seconds in `scripts/factoryd-test-timings.txt`; a new test needs no entry,
  and `TEST_SHARDS_RECORD=1 make test` re-measures. The run fails if a test is
  in zero or two shards. On a memory-tight machine,
  `TEST_SHARDS_SEQUENTIAL=1 make verify` runs the shards one after another
  (slower, peak memory of one race binary).
- `make verify-live` fails loudly without Docker and needs Temporal at
  `TEMPORAL_ADDRESS` (default `localhost:7233`) or the `temporal` CLI. On
  colima it roots scratch dirs under `~/buildgate` so the VM can see them.
- `make proving-ground` runs the fixtures in
  `scripts/proving-ground/fixtures.json`, each pinned to a `base_ref`, one at
  a time. `PROVING_GROUND_TEMPORAL=<addr>` also runs each through Temporal as
  a separate `path: temporal` row.
- The `internal/claims` guards scan **tracked** files (`git ls-files`):
  `git add` a new file before running them. `TestNoPrivateNotesReferences`
  also rejects finding labels such as a capital letter plus a number unless
  the file is in its `ownLabelFiles`.
- Tests run builds through one test Temporal dev server, never the operator's
  `:7233` or an ambient `TEMPORAL_ADDRESS`. `scripts/test-sharded.sh` starts
  it and exports `FACTORYD_TEST_TEMPORAL_ADDRESS`; a bare `go test` starts its
  own on first use (`sharedTemporalAddress`, `cmd/factoryd/temporal_testserver_test.go`).
  Both launch it through `scripts/temporal-test-server.sh`, whose stdin is a
  pipe held by the launcher, so the server dies with its owner even on
  SIGKILL. Helpers pass `-temporal-address` explicitly (`factorydCommand`):
  `runTests` sets `FACTORYD_AUTOSTART=0` for every subprocess, under which a
  run with no address is refused. No test starts `worker` or `serve`
  on its own either; `newTestDeps` (`cmd/factoryd/deps_test.go`) gives every test fakes whose Temporal start, worker spawn and serve spawn refuse. A
  script that starts its own processes exports `FACTORYD_AUTOSTART=0` too.
- The integration tests build their `factoryd` with `-tags factorydtest`
  (`runTests`, `cmd/factoryd/integration_test.go`): that binary has no gateway runtime and launches
  workers through each test's fake `docker` script (`sandbox.Run`, kept only
  as this stand-in). No installed binary carries the tag. Code that must work
  in both takes the runtime as a value (`dp.sandbox.runtime()`, nil in the
  tagged build) and never branches on the tag itself. The gateway launch is
  unit-tested against `internal/sandbox/sandboxtest` and proven against the
  real gateway by `OPENSHELL_LIVE=1 OPENSHELL_LIVE_IMAGE=<worker ref> go test
  ./internal/openshell -run Live` (needs the running stack; an OpenShell
  upgrade repeats it with `OPENSHELL_LIVE_RESTART=1`).
- Known flakes that fail only under full-suite load and pass alone: the
  Temporal owner-signal tests (5 s timeouts),
  `TestIntegrationReferenceOracleCommandNotForwardedToBuildWithoutInLoopFlag`,
  `TestIntegrationAPIStartedRunUsesServerConfiguredReleasePolicy`,
  `TestIntegrationTemporalTimeoutStillRecoversAttemptsFromCheckpoint`, and
  `TestTemporalLiveRepositoryOwnerWorkflowProcessesRunsInOrder`.
  Rerun the one test alone before calling a failure a flake.

## Where a change goes

| Change | Touch |
|---|---|
| A new harness | One adapter class in `agent/pi/scripts/harness_adapters.py`, one entry in `internal/harness/registry.go`, one image stage in `internal/sandbox/Dockerfile`, its model executables in the registry entry's `ModelBinaries` (the route's network policy admits the union across harnesses), recorded fixtures under `agent/pi/tests/fixtures/`, one `containment-matrix.md` row. No job-launch, sandbox-runtime or config-schema change |
| A new command gate | One `CommandGate` entry in `internal/policy/gates.go`, one YAML field plus one `GateCommands` line in `internal/projectconfig`, and `USAGE_REFERENCE.md`'s gate table (see `CommandGate`'s doc comment) |
| A command that reads the session config | Resolve it through the shared resolvers (`loadSettingsForConfig`, `resolveDataDirFromSessionConfig`, `resolveEffectiveConfigPath`), which apply `sessionconfig.ResolveArg` (a `-config` profile name) and `sessionconfig.ResolvePath` (`FACTORYD_PROFILE`, `active-profile`, defaults); never read `DefaultPaths` directly |
| A session-config key | `internal/sessionconfig`, validation test, `USAGE_REFERENCE.md`'s session-config table, and `quickstart`/`init-config` if new configs should write it |
| A CLI verb, flag or request state the operator skill names | `internal/operatorskill/buildgate/SKILL.md` in the same PR |
| A built-in worker skill | One folder `internal/workerskills/skills/<name>/SKILL.md` (portable front matter, non-interactive, no scripts; `TestBuiltinSkillsFollowTheWorkerContract` enforces it), its line in `sessionconfig.Example`, and USAGE_REFERENCE's built-in skills table. Opt-in only: never add it to a default config |
| A runtime dependency a command should start for itself | `internal/hostcontrol/autostart.go` (probe, start, wait behind `internal/spinner`, one-line reason on failure), honouring `FACTORYD_AUTOSTART`; it reaches the outside through a `deps` boundary method (`cmd/factoryd/deps.go`), whose fake default in `newTestDeps` (`deps_test.go`) must not leave the test process |
| A run input (a value a build needs from a flag or the session config) | Its flag in `newRunFlags` and its resolution in the `ticketRun` stage that owns it (`cmd/factoryd/run_ticket.go`); one field on `runOptions` (`run_options.go`), set in `ticketRun.dispatch` and read in `runOptions.workflowInput()` (`run_temporal.go`) into its `RunWorkflowInput` field (`internal/workflow/workflow_types.go`). If the worker must forward it to a request's ticket build, one argument in `BuildTicketRunArgs` (`internal/requestdriver/config.go`). Add the flag to `runInputGoldenFlags` and regenerate the goldens (`FACTORYD_UPDATE_GOLDEN=1 go test ./cmd/factoryd -run WorkflowInputFromFullFlagSet`): a field that does not reach the workflow leaves the golden unchanged, which is the sign it is mis-wired. `TestWorkerEndToEndRequestReachesAcceptedRun` covers the worker's argv |
| A workflow activity | The method on `Activities` in the `internal/workflow/activities_<phase>.go` file of its phase (prepare, build, verify, review, oracle commit, evidence, rollback, sandbox), its name constant in `workflow.go`, its input and result types in `workflow_types.go`, and its call in `RunWorkflow`. Version-marker rule: a call added to, removed from or reordered in `RunWorkflow` changes workflow history, so it goes behind `GetVersion` with a new change ID (as `requireIsolatedWorkspaceChange` and `keepWorktreeWhenLostChange` do); never reuse or renumber an ID, and add a replay test (`TestHistoryRecordedBeforeTheIsolationGuardReplaysUnchanged` is the pattern). Moving code between files changes nothing |
| A new external dependency (a process, binary or service the code calls) | One method on the boundary interface it belongs to in `cmd/factoryd/deps.go` (`host`, `forge`, `temporal`, `docker`), its real body as a method of `realHost`/`realForge`/`realTemporal`/`realDocker` beside the code that uses it, and its field and method on the fake in `deps_test.go`, with a default in `newTestDeps` that cannot leave the test process. Called from `internal/hostcontrol` or `internal/requestdriver`: also one method on that package's `Deps` interface and its one-line wrapper on `*deps` (`deps_hostcontrol.go`, `deps_requestdriver.go`). Pass `dp` down; never add a package-level function variable |
| An operator-visible behaviour | `USAGE.md` (walkthrough) or `USAGE_REFERENCE.md` (flags, keys); `README.md` if it changes what works or a known limit |

## Module boundaries

`cmd/factoryd` wires, `internal/*` does the work, and no `internal` package
imports `cmd`. `internal/claims/imports_test.go` enforces the rows below
that name an allow-list.

| Package | What it does | Buildgate packages it may import |
|---|---|---|
| `cmd/factoryd` | Flags, session config, the commands, the single-ticket run path (`ticketRun` stages, `runOptions`), the worker; builds the real `deps` in `main` and passes it down | Any `internal/` package |
| `internal/hostcontrol` | Starts, finds and stops Temporal, Colima, the worker and serve | Allow-list: the module root (the embedded Temporal compose file), `consolelink`, `daemonheartbeat`, `sanitize`, `spinner`. Reaches the machine only through `hostcontrol.Deps` |
| `internal/requestdriver` | Advances a request one step: drafting, planning, ticket builds, corrective rounds, PR review | Allow-list: `api`, `codereview`, `consolelink`, `evidence`, `forge`, `notify`, `policy`, `projectconfig`, `release`, `request`, `requestsubmit`, `run`, `runner`, `sandbox`, `sessionconfig`, `ticketspec`, `workflow`, `workspace`. Never `hostcontrol`. Reaches GitHub, git push and the build entry point only through `requestdriver.Deps` |
| `internal/workflow` | The Temporal workflows and activities of a build | No allow-list. Never `cmd`, and never `requestdriver`, which imports it |
| `internal/meter` | The spend meter the OpenShell supervisor calls for every model request (`cmd/factoryd-meter`): ceilings, sliding windows, pricing, per-format usage parsers, reasoning-effort ranking, each sandbox's usage ledger (`Account`, `Ledger`); also the route host pins and the host-side Copilot model listing and token exchange | Allow-list: its own generated `middlewarepb`. Imports no other buildgate package. `internal/claims/imports_test.go` enforces it |
| `internal/openshell` | The `sandbox.Runtime` over the OpenShell gateway: turns a `sandbox.SandboxRequest` into the gateway's sandbox spec, workload template and network policy, pushes a route's credential, and reads Docker's view of a sandbox's containers. The only package that imports the OpenShell Go SDK | Allow-list: `sandbox`. Reaches the gateway through the SDK's client interface, plus its own `RouteReadiness` interface for the one call the SDK lacks, and Docker through its own `Containers` interface. Its `Live` tests run only with `OPENSHELL_LIVE=1` against a running gateway |
| `internal/sandbox/sandboxtest` | A worker-like `sandbox.Runtime` for the tests of packages that launch through one | Allow-list: `sandbox`. Imported by tests only |

Rules for a new boundary:

- An interface goes where code crosses a process or host boundary (Docker, git, gh, Temporal, launchd, the clock) or where two packages meet. A helper with one implementation and no boundary stays a plain function.
- The consumer defines the interface, beside its use, with only the methods it calls.
- A dependency is passed in (a parameter or a struct field), never reached through a package variable.
- A fake is a small hand-written struct in a `_test.go` file; no mock framework.

`make verify` checks these design rules (`internal/claims`):

| Rule | Test | When it fails |
|---|---|---|
| A Go function has at most 25 decision points | `TestFunctionComplexityStaysWithinLimit` | Split the function. One already over the limit is listed in `internal/claims/testdata/complexity_baseline.txt` and may shrink, never grow |
| No mock framework | `TestNoMockFrameworkIsImported` | Write a fake struct |
| Only listed packages import `os/exec` | `TestOnlyListedPackagesRunProcesses` | Run the process through an interface the caller declares, then add the package to `processRunningPackages` |

After splitting or moving a listed function, regenerate the baseline with
`CLAIMS_UPDATE_COMPLEXITY_BASELINE=1 go test ./internal/claims -run TestFunctionComplexityStaysWithinLimit`.
Its diff may only remove lines, lower numbers, or rename a moved function.

The tests of `internal/hostcontrol` and `internal/requestdriver` still live in `cmd/factoryd` and call them through exported names.

Operator docs describe the current state only: no history, no "used to",
no PR-by-PR changelog (git log has it). Tables and lists over prose; flows as
plain-ASCII diagrams in `text` code blocks; paths as `~/`, never a
user-specific absolute path.

## Live validation

`make verify`, code review, and unit tests all validate logic in
isolation. None of them run inside a real sandbox launched by the
OpenShell gateway, against a real model route, with real git. Every serious bug found in this
repo's build/gate/sandbox-runtime/conformity-review pipeline so far — a sandbox
mount assumption that broke commits, a policy gate that could silently
skip itself, a token budget that could overrun its own configured
ceiling — was invisible to all three and only surfaced on a real run.
Passing review is evidence the *logic* is right; it is not evidence
the *pipeline* works.

**Before merging any change that touches the build loop, a policy gate,
the sandbox runtime (`internal/openshell`, `internal/meter`, the launch
path in `internal/sandbox`), or the request/conformity-review pipeline, run:**

```sh
make live-smoke
```

This drives `factoryd`'s real single-ticket run path, on Temporal by default (no drafting, no
human-approval gate — see `scripts/live-smoke.sh`'s own doc comment) end
to end against a small, fixed set of real tickets in fresh, disposable
clones of their target repos, and reports pass/fail — not a defect
count. It needs Docker, the OpenShell gateway and meter (`factoryd doctor
-fix` starts them) and a working model route (the same setup
`factoryd doctor` checks) and takes a few minutes; it is deliberately not
part of `make verify`.

A change to the install or first-run path (`make install`, `configure-images`,
`quickstart`, `doctor -fix`, session-config loading, the OpenShell stack start)
also needs the from-nothing run, because a machine that already works hides
every first-run bug (a retired config key, an image ref a rewrite drops, a
stack nothing started): back up `~/.config/factoryd` and `~/buildgate`, then
`factoryd uninstall -purge` (it clears the gateway's VM state too),
`make install`, and `factoryd quickstart` against a repo under `$HOME` (not
`/tmp`: the gateway sees only the home directory) until its first review gate. Follow
README's "A new Mac, start to finish" literally; anything you had to know that
it does not say is a bug in the docs or the code. Keep that section, USAGE.md's
Quick start and the troubleshooting tables in step with the change.

A change to compose services (`internal/composeservices`, the compose
lifecycle, or the `BG_SERVICE_*` worker wiring) also runs `make
live-compose`: one real todo-kafka-service ticket with its Postgres,
Kafka and Redis sidecars, which `live-smoke` deliberately omits.

Track the goal here as **a live one-shot acceptance rate**, not
"spotless": what fraction of real tickets submitted through `factoryd`
reach an accepted, human-reviewable PR with zero manual intervention.
Record dated runs of the broader (not just the fast standing pair)
validation set as dated write-ups kept outside this repo — a rising number over
successive runs is the actual signal this repository's pipeline is
improving, since code review alone has no natural stopping point (a
sufficiently adversarial pass always finds one more thing).

`make bar` (`scripts/bar.sh`) is the separate, standing measurement of the
`-draft-oracles` default-on bar: `BAR_ROUNDS` (default 2) rounds of
`scripts/bar/fixtures.json`, each flag off then on, strictly one run at a time
(the local model is single-instance) on a binary built from a clean tree and
recorded by SHA. It pauses at `oracle_review` for a human (never auto-approved;
`BAR_ORACLE_HOOK` automates a hand-written oracle) and reports each of the four
criteria as MET, UNMET or UNPROVEN. Run it only when asked to re-measure the
bar, not per PR; `scripts/bar.sh --list` and `make bar-test` need no Docker or
model. Its mutation check executes agent-built code, so it runs only inside
Docker, never on the host. Record the dated result outside this repo, like the
live-validation runs.

## Conventions

- Go 1.26, module `buildgate`. Follow existing package structure
  under `internal/` rather than introducing new top-level packages for a
  small feature.
- Docs and comments in this repo are written dense and evidence-heavy on
  purpose (dates, PR numbers, "found via review X" citations) — match
  that register in normative docs (`safety-contract.md`, `CLAIMS.md`,
  `containment-matrix.md`); README-style docs can be lighter.
- Don't add a way to run a coding-agent build outside the Docker sandbox
  (a host-execution mode, an "unsandboxed" flag/config, or similar
  escape hatch) — Docker containment is unconditional across every path
  by deliberate design. See `containment-matrix.md` and
  `safety-contract.md`'s trust-boundary table.
- Merge and deploy are permanently human actions. Don't wire a release
  `Decision` to an actual merge/deploy side effect — SC-001 forbids it by
  standing design, not as a gap to close.
