# End-to-end validation plan

A repeatable checklist for driving mechanical confidence in `factoryd`
(the single-ticket run, the request pipeline and the repository-owner flows) and the operator console to as close to
100% as the system's own design allows — deliberately scoped against
what `safety-contract.md` and `CLAIMS.md` already claim, not a rewrite
of either. Run it (or the parts relevant to a change) whenever a
significant change lands; record dated results and findings from an
actual pass outside this repo, not in this file — this file
stays the reusable checklist, not a run log.

**What "100% confidence" can honestly mean here.** `CLAIMS.md`'s
"Remaining gaps" and `containment-matrix.md`'s scope section already name
the residuals this plan doesn't chase — the 24h idle-reconciliation race,
the mid-save crash-durability edge case, two load-sensitive Temporal
hard-termination tests, two `userns-remap`-dependent Filesystem residuals,
the dropped-request partial-spend window — treat those as accepted
risk, not targets. What this plan *does* target: every invariant
SC-001–SC-015 gets at least one live observation (real Docker engine,
OpenShell gateway, model — not the fake-engine doubles behind most of
`internal/sandbox`'s tests) on each flow where it depends on the flow
(traceability table at the end); every operator flow in
`USAGE.md`/`USAGE_REFERENCE.md`/the console walked end-to-end against a
real run; and the containment matrix's adversarial fault classes
exercised by hand, not just by the existing `DOCKER_SANDBOX_LIVE` tests.

## Ground rules (apply to every phase)

- **Pin the SHA.** Execute against one recorded `main` commit; a pass's
  dated results note names it plus run IDs.
  A fix landing mid-plan re-baselines the affected phase, not the whole
  plan.
- **One model consumer at a time.** Every live run here routes through
  the local ai-stack model (`AI_STACK_HOST`, OpenAI-compatible `/v1`,
  model id discovered from `GET /v1/models`, never hardcoded). Do not
  run two live phases concurrently; Phase 2.3's "two runs, one repo" is
  fine because the second serializes by design.
- **Live-test environment.** The repo's existing real-engine tests are
  gated, not absent: `DOCKER_SANDBOX_LIVE=1` (optionally
  `DOCKER_SANDBOX_IMAGE`, `DOCKER_SANDBOX_LIVE_ROOT` — on colima this
  must be under `$HOME` for mount visibility, the trap `factoryd doctor`
  checks), and the test Temporal server `make verify-live` starts for
  them (`FACTORYD_TEST_TEMPORAL_ADDRESS`; never the operator's
  `localhost:7233`). Any phase that says "re-run the live test" means
  with these set.
- **Throwaway targets only.** A fresh, disposable target repo with its
  own GitHub remote (the request pipeline's `-open-pull-request` default
  opens draft PRs). Nothing here ever runs against a real project's
  `main`. Pi is the harness under test; the pifork harness is
  opt-in, needs a private local image, and is out of scope here (see the
  non-goal below).
- **Nothing here adds code paths.** This plan adds no flag, config, or
  test hook to make validation easier. In particular no host-execution
  or "containment off" mode — `AGENTS.md` forbids it and there is no
  legitimate configuration to validate.

## Phase 0 — Baseline (fast, cheap, must pass before anything else)

Run the three toolchains as they exist today and fix anything red before
building new coverage on top of a broken baseline:

```sh
make verify                       # Go: fmt-check, vet, go test -race ./...
make console-test                 # console: typecheck, lint, format, Vitest
python3 -m pytest agent/pi/tests/
```

Then a second Go pass with the live gates open, so the real-engine tests
`CLAIMS.md` and `containment-matrix.md` cite as evidence actually execute
rather than `t.Skip`. On colima, `DOCKER_SANDBOX_LIVE_ROOT` must be set to
a path under `$HOME` (the ground rules above already say why) or every
`*LiveDocker` mount-visibility-dependent test in `internal/sandbox`/
`internal/workspace` fails on the mount-visibility probe alone, not on
anything this plan is actually trying to prove — the default macOS
`$TMPDIR` root does not satisfy this. Also set `-p 1` so the packages run sequentially, honoring the "one model
consumer at a time" ground rule (`internal/workflow`'s model-backed tests
otherwise interleave with `cmd/factoryd`'s):

```sh
mkdir -p ~/.factoryd-live-root
DOCKER_SANDBOX_LIVE_ROOT=~/.factoryd-live-root make verify-live
```

`internal/claims/*_test.go` structurally validates `safety-contract.md`/
`CLAIMS.md`/`containment-matrix.md` — a failure there means one of those
docs drifted from its checked tokens; fix the doc, not the test.

Finally `factoryd doctor` (and `doctor -temporal-address localhost:7233
-registry-proxy`) with the exact session config Phases 1–4 will use —
every later phase assumes it is green.

Acceptance:

- All four toolchain commands green, `-race` clean; record wall time per
  command.
- The live pass runs with zero `--- SKIP` in the four packages above
  (the tree has ~200 `t.Skip` calls, essentially all env-gated; with both
  gates set, a surviving skip in these packages is a missing precondition
  to fix, not a test to justify). Record any skip that remains and why.
- `factoryd doctor` green with the plan's config, output saved.

## Phase 1 — Single-ticket run and request pipeline, live end to end

Exercise the flows exactly as `USAGE.md` describes, against a real Docker
engine, the real OpenShell gateway and meter, the real local model and a local Temporal server
(every build runs on it). Two
distinct flows live here and must not be conflated: the bare
`factoryd <run>` invocation (§6) and the `submit`/`worker` request
pipeline (§11), which drafts spec and tickets itself and opens a draft PR.

1. **Intake and scaffold** (`USAGE.md` §1–4): fresh pilot dir, `factoryd
   intake` to the spec-freeze checkpoint; freeze `spec/spec.md` by hand;
   re-run the identical `intake` to the acceptance-suite checkpoint;
   `factoryd check-project` passes. Confirm both checkpoints actually
   stop (exit, ticket 001 not built) — they are permanent human gates,
   not a gap.
2. **Accepted run** via bare `factoryd <run>` (§6) for ticket 001, with
   `-spec`/`-ticket-file` both pointing at the ticket, default
   the per-run worktree, the gateway and meter running (`factoryd doctor -fix`), `-registry-proxy` on,
   through to `accepted`.
3. **Evidence audit** on `data/runs/<id>/`: `run.json` (state `accepted`;
   `base_sha` is an ancestor of `result_sha`; sandbox image digest
   recorded and equal to `docker image inspect`'s; `Sandboxed()` true;
   `project` equals the repo basename; `changed_files` is `[]`-or-list,
   never `null`; `diff_stat` present; oracle-spec and verify-surface
   hashes present and equal to `sha256` of the spec snapshot and
   `verify.log` respectively — SC-012), `events.db` (WAL, run's events
   present), `diff.patch` byte-equal to `git diff <base_sha>..<result_sha>`
   in the worktree, retained `BUILD_REPORT.md`, `BUILD_EVIDENCE.json`
   with `schema_version == run.AgentEvidenceSchemaVersion` and non-null
   `usage`, `notifications.log`, and a release decision readable via
   `GET /runs/{id}/release` (allowed or denied with reasons — either is
   fine; "no decision" is not). `factoryd status` and `GET /runs/{id}`
   agree with `run.json` field-for-field for state, ticket, reason. The
   worktree at `data/workspaces/<id>` and branch `factoryd/<id>` still
   exist (acceptance preserves them; only rejected runs auto-discard).
4. **Quarantined runs**, two classes, because they exercise different
   gates: (a) a ticket whose `Verify-Command:` genuinely fails while the
   agent's `BUILD_REPORT.md` claims success — lands in `quarantined`
   (SC-006: prose never advances state), `notifications.log` has the
   entry *before* any Discord/desktop dispatch (SC-013), worktree is
   preserved; (b) a ticket with `Allowed-Files:` where verify passes but
   the agent touches an out-of-scope file — `quarantined` by
   `diff_scope`, not accepted. Then `factoryd override` (a) to `accepted`
   with `-by`/`-reason` (the CLI's own `-state` only ever accepts
   `accepted` or `halted` — there is no `resumed` state), confirm the record carries
   operator, reason, prior/new state, time (SC-007); override (b) to
   `halted` and confirm the isolated worktree/branch are rolled back.
5. **Cancellation.** There is no API cancel route (`POST
   /runs/{id}/override` acts only on a quarantined run), so on this path
   cancellation is `SIGINT`/`SIGTERM` to the `factoryd` process
   mid-build. Acceptance: `docker ps -a` shows no worker, supervisor, or
   registry-proxy container of the run (`openshell.ai/sandbox-name` for each
   name in the run's `sandboxes.jsonl`, the `-data-dir` label for sidecars);
   `docker network ls` shows no per-run network; `run.json` is
   `halted` with the interrupted attempt recorded; a halt notification
   is logged; `factoryd reconcile` afterwards reports nothing to reap
   (SC-010, live). Repeat with `SIGKILL` (`kill -9`): now expect a
   stranded worktree/sandbox that `factoryd reconcile` (or the next run's
   startup) reaps, and the run reconstructed as ambiguous/halted, never
   `accepted` (SC-008; the containment matrix's crash-recovery row,
   observed by hand rather than only by
   `TestReconcileOrphansRemovesTheRuntimeSandboxesOfTerminalRunsOnly`).
   Also restart the gateway mid-build (`docker compose -p
   buildgate-openshell restart gateway`): the step is recorded lost, the
   worktree is untouched and the request waits in `resume_review`.
6. **Ceiling halt.** The unit tests prove the meter's refusal and
   factoryd's reading of the ledger; the live piece is a real agent build
   driven past a ceiling. Method: commit `token_ceiling:` (tighten-only)
   in the target's `.factory.yml`, or set `meter_token_ceiling` in
   session config, low enough that ticket 001 cannot finish. Acceptance:
   run halts with `HaltReasonRelayCeilingExceeded`
   (`relay_ceiling_exceeded`), consumed usage persisted in `run.json`,
   `factoryd status` shows the cost, worker container gone, and the
   sandbox's ledger under `~/buildgate/meter-ledgers/` ends with a
   `ceiling_exceeded` record.
7. **Request pipeline** (`USAGE.md` §11) against the same throwaway
   repo: `submit` → `spec_drafting` → `spec_review` (reminder appended to
   `requests/<id>/notifications.log` on entry) → `reject` once (redraft
   sees the `## Rejected` section) → `approve` → `planning` →
   `plan_review` → `approve` → `building` → run accepted → draft PR
   opened → `pr_review`. Two negative checks: edit `spec.md` after
   approving it and confirm planning refuses naming the file; stop
   `worker` mid-ticket (`factoryd stop -force`) and confirm the request
   waits in `resume_review` at the next start and `factoryd resume <id>`
   continues the ticket in its kept worktree. Acceptance: every state above observed in `request.json`,
   `factoryd status` shows `ticket i/n` and the PR URL, the PR is a
   draft on the throwaway remote, and `worker` never merged it.

Acceptance for the phase: one full accepted run, two quarantined runs
(verify-failure and diff-scope), one attributed override each way, one
clean `SIGINT` cancellation, one `SIGKILL` reconciliation, one real
ceiling halt, one request driven to `pr_review`, all with the evidence
audit in step 3 matching what `factoryd status` and the API report.

## Phase 2 — Repository-owner and daemon flows, live end to end

Same shape as Phase 1, run through the `-repository` and daemon flows of
`USAGE_REFERENCE.md` ("Temporal: repository owners, daemons, observing"),
because exclusivity, reclaim and the daemon control plane exist only there:

0. Local Temporal server up (`docker-compose.temporal.yml`); `factoryd
   doctor -temporal-address localhost:7233` green. Cheap fail-closed
   check first: stop the server and start a run with default
   a plain run — expect `halted`/`HaltConfirmed` naming
   Temporal as the cause, never a build anywhere else
   (`USAGE.md`, "The default address").
1. Plain `-temporal-address` accepted run (independent `RunWorkflow`),
   then a `-repository` run serviced by a long-lived `factoryd daemon`
   (§3–4), the latter being the path everything below uses.
2. Repeat Phase 1's accepted / two-quarantine / override / ceiling
   matrix. Cancellation on this path has two shapes, both required:
   `SIGTERM` to `factoryd daemon` mid-build (graceful drain), and
   cancelling the `RunWorkflow` from Temporal's Web UI. Same `docker
   ps`/`docker network ls`/`reconcile` acceptance as Phase 1.5.
3. **Concurrency / exclusivity (SC-011):** submit two runs against the
   same `-repository` while the first is in flight; confirm the second
   serializes behind the owner workflow (Temporal UI shows one
   `RunWorkflow` child at a time; the second worktree is created only
   after the first run reaches a terminal state). Then a plain
   `factoryd <run>` (no `-repository`) against the same repo while a repository run holds
   the flock — expect "repository is busy" refusal or bounded waiting,
   never two workers in one workspace.
4. **Hard termination + recovery**, three shapes: (a) `SIGKILL` the
   daemon mid-build, restart it, confirm the daemon's startup reclaim
   finds the run and either rolls back or resumes it under the
   ownership markers, with no stranded worktree left after it settles;
   (b) restart the Temporal *server* mid-run (`down` then `up`, keeping
   the volume) and confirm the workflow resumes and the run's attempts
   are recovered from the checkpoint journal, not duplicated — this is
   the durability Temporal is there for; (c) `TerminateWorkflow` from the UI, confirm the
   isolated worktree is rolled back (the 3-review-round story in
   `CLAIMS.md`).
5. **Flake characterization**: run
   `TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout` and
   `TestIntegrationIsolateWorkspaceRollsBackOnHardTerminationViaTemporal`
   standalone (`-run`, `-count=5`, otherwise idle server). Record
   pass/fail count. 5/5 confirms the accepted "only flakes under shared
   load" characterization; any standalone failure is a real bug, not
   the accepted class. Do not chase the flake itself.
6. **Daemon control plane** (`USAGE_REFERENCE.md`, "Daemons and lifecycle control"): `factoryd serve
   -daemon-temporal-address`, `GET /daemons`, `POST /daemons/stop` then
   `start` for the repository, confirm the child is replaced and a run
   submitted meanwhile is picked up after restart.
7. Idle-owner reconciliation race: accepted risk per `CLAIMS.md`, not
   reconstructed here (see non-goals).

Acceptance: same evidence bar as Phase 1, plus a demonstrated exclusivity
*prevention* (3), all three hard-termination recoveries (4) with zero
stranded worktrees/containers after each settles, and 5/5 on step 5.

## Phase 3 — Chaos / fault injection (both flows unless noted)

Deliberate fault injection beyond Phases 1–2's "kill it and see," aimed
at the specific rows of `containment-matrix.md` and the SC invariants.
Worker-side probes run from a custom offline `-build-app-script` (an
explicitly supplied script may stay networkless, so this needs no new
code path) or from a ticket instructing the agent to attempt them; the
expected outcome is the same either way.

| Fault | What it proves | Expected safe outcome |
|---|---|---|
| Pre-launch free space below `minFreeBytesForLaunch` on the workspace or `-data-dir` mount (fill with a large file first) | The launch guard in the Filesystem row refuses before any container starts | No container launched; run halted with a reason naming the guard; nothing to reap |
| Disk fills *mid-run* (between saves) | Characterizes the accepted mid-save residual (`CLAIMS.md`: structurally unrecordable), nothing more | Never an `accepted` record; whatever is left is reconciled to halted/ambiguous by `factoryd reconcile` after space is freed. Record what happened; do not open a ticket unless the outcome is a false `accepted` |
| Docker engine (colima) stopped mid-build, then restarted | SC-008/SC-009: an infrastructure failure is classified as such, never as success | Attempt recorded as infrastructure failure or the run halted; after `colima start`, `reconcile` leaves nothing stranded |
| Model host unreachable mid-build (firewall the upstream, or stop ai-stack) | SC-009: bounded retries, infrastructure classification | Retries bounded by `build_app_max_attempts`; run halts/quarantines with a reason naming the route; each retry's ceilings are the configured ones minus the spend already in the run's ledgers |
| `-timeout-minutes` set below the build's real duration | SC-009/SC-010 forced teardown and the `docker exec` permission reclaim | Attempt recorded with the timeout; worker gone; worker-created paths still writable by factoryd for rollback |
| factoryd killed mid-request (SIGKILL while the worker is mid-call) | The meter's ledger outlives factoryd; a request with no completion record is charged its estimate | Sandbox reaped by the next reconciliation; run never `accepted`; the ledger holds the admitted request, and a resume lowers its ceilings by it |
| Worker network probes: `curl` an internet host; dial the model upstream's IP:port directly; DNS lookup of anything; a request to the model host outside the route's admitted path; a request to the model host from a binary the policy does not name; `CONNECT`/`POST` through the registry proxy; reach a sibling container by name | SC-005 deny-by-default against an actively adversarial worker, plus the supervisor's policy and the proxy's path allowlist | Every probe fails (the supervisor answers 403 `policy_denied`; the proxy logs its own rejections); no host or upstream reached except allow-listed GET/HEAD via the proxy and the admitted model path via the supervisor |
| Worker filesystem probes: write to `/`, to `/workspace/..`, to the Git common dir, rewrite the linked worktree's `.git` pointer file, set `core.hooksPath`/a `filter` in the worktree's git config (the git-hooks host-RCE shape), symlink out of the worktree, `chmod 000` a created dir, look for `-data-dir` | SC-004 confinement and the closed git-hooks host-RCE path, against deliberate attempts | Every write outside the worktree fails; a later factoryd-side `git` call in that worktree runs no attacker-authored config; the `chmod`'d path is reclaimed on teardown; `-data-dir` is not visible inside the container |
| Worker credential/Docker probes: dump `env`; look for `/var/run/docker.sock`; run `docker ps` | SC-002/SC-003 | `env` contains only the fixed `--env` allowlist — no `ANTHROPIC_API_KEY`, no GitHub/Copilot token, no `FACTORYD_*` token; no socket; no Docker control |
| Worker rewrites its own history (`git reset --hard <older>` in the worktree) mid-run | T-12: base revision must remain an ancestor | Run halts before gates are evaluated, nothing committed by the safety net |
| Worker replaces `BUILD_EVIDENCE.json`/`BUILD_REPORT.md` with a symlink to a host-shaped path, a FIFO, and an oversized file (one run each) | The hostile-evidence read/retain path stays closed | Each refused; the run's outcome is unaffected (evidence is never a gate input); factoryd does not block or archive a foreign file |
| Worker edits the verify surface: changes the `Makefile` target `Verify-Command:` invokes to `true`; edits `.factory.yml` in the worktree | SC-012/T-01 — what the factory *can* and *cannot* catch today | Oracle-spec hash in evidence still equals the pre-run snapshot; with `Allowed-Files:` excluding the `Makefile` → `quarantined` (`diff_scope`); with `protected_paths` naming it → release denied; the `.factory.yml` edit is ignored (read from committed `HEAD`) and denied as a protected path. With neither declaration, an accepted run whose `changed_files` shows the `Makefile` is the honest current limit (T-01 is `partial`) — record it as such, do not overclaim |
| Two operators override one quarantined run near-simultaneously (`factoryd override` CLI and `POST /runs/{id}/override` in parallel) | SC-007 durable, attributable override with no lost update | Exactly one override recorded, attributed to the winner; the loser is refused ("not quarantined"); `run.json` state and the API agree |
| `factoryd kill-switch` engaged while a run is in flight; two concurrent engage/disengage invocations | SC-014/T-10: the release decision fails closed on the switch; transitions serialize | The in-flight run's own state is untouched; its decision is denied with a reason naming the switch; both transitions appear in the attributable history in order; disengaging lets the next accepted run's decision evaluate normally |

This phase is explicitly adversarial in the same sense the project's own
`adversarial-review-for-untrusted-actor-mechanisms` practice already
applies to code review — the worker is untrusted by design
(`safety-contract.md`'s trust boundaries), so prove containment against
what it could actually attempt, not just its cooperative default path.

Acceptance: every row produces its stated expected outcome on a
single-ticket run and (where the row depends on it) a `-repository` run, with
durable evidence — none produces a silent `accepted`, an orphaned
process/container/network, or an unattributed state change. Rows marked
"characterize" produce a recorded observation, not a pass/fail.

## Phase 4 — Console UX walkthrough (manual + component tests)

Every console screen, walked against a *live* `factoryd serve` reading
the same `data_dir` as Phases 1–2 (read routes are loopback-only by
default; approve/reject/override need `FACTORYD_API_OVERRIDE_TOKEN`;
`NewRunScreen` needs `-api-allowed-sandbox-images` configured), covering both a healthy run and
each edge case the screen exists to handle:

- `RunListScreen` / `RunDetailScreen` — a run from queued (`GET
  /queue`) through `accepted` and, separately, through `quarantined`;
  live status updates via the streamed `watchRun`. Restart `serve`
  mid-watch and confirm the console reconnects with backoff; rotate the
  read token and confirm the 4xx surfaces as an error rather than
  retrying forever.
- `BoardScreen` / `RequestDetailScreen` — the Phase 1.7
  request lifecycle, approve and reject from the console, the board
  sorted waiting-on-you first, 5s refresh.
- `NewRunScreen` — submit a real run via `POST /runs`; confirm it
  reaches the same preflight/gate/evidence path as CLI submission. One
  documented divergence is expected and not a finding: the registry
  proxy is not on the API path (`containment-matrix.md`, Package
  registry row), so a console-started run against a repo needing
  unbaked dependencies must fail loudly, not silently pass.
- The run page's diff tab (`GET /runs/{id}/diff`) — byte-equal to `diff.patch`
  for the Phase 1.2 run.
- `TriageScreen` / `OpsScreen` — operator triage against a real
  quarantined run, override from the console (`POST /runs/{id}/override`
  takes `by`/`reason`/`state`, mirroring `factoryd override`), and confirm
  the recorded override carries the `by` the console sent, while a
  request approve/reject from the console records `by: "api"` as
  `USAGE.md` §11 documents.
- The run page's release tab / `ProjectReleaseScreen` — read-only decision and
  kill-switch state, compared with `factoryd kill-switch -project <p>`
  output for the same project; a run with no decision renders "no
  decision", never allowed; the refresh action's stale-data warning on
  a failed refresh; switch projects rapidly on `ProjectReleaseScreen`
  and confirm no stale chip from the previous project lingers after the
  new one loads. Confirm by inspection that no control on either screen
  engages or disengages the switch, merges, pushes, or deploys.
- `ProjectListScreen` / `ProjectStatsScreen` (`GET /projects`,
  `GET /projects/{project}/stats`) — aggregates equal what `factoryd
  status -json` computes from the same run records; the derived project
  id is shown.

Acceptance: a per-screen checklist with every line above ticked against
a named run/request ID; every value the console shows equals the
corresponding `factoryd status -json` / `GET` field; no console action
reaches a backend effect the CLI/API cannot; the console exposes no
merge/deploy/kill-switch control (SC-001 applies to the console too);
and no screen shows a stale chip or value left over from a
previously-viewed project or run.

## Phase 5 — Wrap-up

- Bring `CLAIMS.md` and `containment-matrix.md` in line with Phase 1.6's
  ceiling result — per `AGENTS.md`, keep
  the checked tokens/table rows intact, edit prose around them; the
  narrative stays out of this repo.
- Anything this plan finds broken becomes a normal ticket/fix, self-
  reviewed per this project's standing practice (`/code-review`
  adversarial pass before any PR; Codex only if budget is confirmed
  available, capped at 2 rounds). A finding that touches a safety
  boundary gets the dedicated "how would the worker attack this" pass,
  not just a diff read.
- Anything this plan *characterizes but doesn't close* (the accepted
  flake class, the idle-reconciliation race, mid-run disk-fill, the
  dropped-request partial-spend window, the T-01 unscoped-`Makefile` limit) gets
  a dated note outside this repo, not a rewrite of
  `CLAIMS.md`'s existing accounting of them.
- Record each pass's results — SHA, run/request IDs, findings — as a
  dated note outside this repo, per `AGENTS.md`; this file
  stays the reusable checklist, never the record of who ran it when.

## Invariant traceability

Which phase step gives each invariant its live observation. "Absence"
means the invariant is proven by there being nothing to exercise, which
is a code-review/`grep` fact, not a run.

| Invariant | Live observation |
|---|---|
| SC-001 fail closed before release | Absence (no merge/deploy side effect exists); Phase 4 console has no such control |
| SC-002 no host credentials | Phase 3 credential/Docker probe row |
| SC-003 no Docker control | Phase 3 credential/Docker probe row |
| SC-004 selected workspace only | Phase 3 filesystem probe row |
| SC-005 network deny by default | Phase 3 network probe row |
| SC-006 external oracle only | Phase 1.4 (a) and (b), Phase 2.2 |
| SC-007 durable attributable override | Phase 1.4 overrides; Phase 3 concurrent-override row (path-independent) |
| SC-008 durable reconstruction | Phase 1.5 `SIGKILL`, Phase 2.4 (a)–(c), Phase 3 disk/engine rows |
| SC-009 bounded attempts | Phase 1.6 / 2.2 ceiling; Phase 3 timeout and unreachable-model rows |
| SC-010 complete cancellation | Phase 1.5, Phase 2.2 (both cancellation shapes), Phase 3 timeout row |
| SC-011 exclusive ownership | Phase 2.3 |
| SC-012 no oracle mutation | Phase 1.3 hash audit; Phase 3 verify-surface row |
| SC-013 asynchronous quarantine | Phase 1.4 (a) notification ordering; Phase 1.5 halt notification |
| SC-014 release evidence boundary | Phase 1.3 decision; Phase 3 kill-switch row |
| SC-015 harness/model closed presets | Absence (harness and model resolve only to compiled-in presets/configured `models:` entries; no code path accepts an executable, interpreter, upstream URL, or path prefix from a request) |

## Explicit non-goals

- Not attempting to close the two accepted-flake Temporal termination
  tests' root cause (load-sensitive, accepted) — Phase 2.5 only
  re-confirms the characterization.
- Not building a release/merge-acting mechanism to test against — SC-001
  and `safety-contract.md` make that permanently out of scope, so there
  is nothing to validate there beyond "still doesn't exist."
- Not chasing the sub-astronomically-unlikely 24h idle-owner
  reconciliation race.
- Not adding a host-execution/unsandboxed mode, a containment-off
  switch, or any validation-only flag — `AGENTS.md` forbids the escape
  hatch outright, so there is no legitimate configuration to test.
- Not re-demonstrating residuals `containment-matrix.md` accepts under
  the single-engineer scope decision: the group-write grant spanning
  factoryd's GID and colima's host-visible UID attribution both need
  daemon-level `userns-remap`; multi-tenant isolation is out of scope
  entirely.
- Not re-demonstrating the `intake` `-pilot-dir` concurrent-invocation
  lock (T-07) — closed 2026-09-16 via `acquireExclusiveLock`, covered by
  `TestIntakeRefusesConcurrentInvocationOnSamePilotDir`; a live collision
  would only reproduce what that test already proves.
- Not validating the Anthropic Messages route (`credential_mode: static`
  against `api.anthropic.com`) — no live run to date has used it; Pi
  against the local OpenAI-compatible route and ChatGPT Codex
  (`credential_mode: chatgpt-codex`) are the routes this machine
  has live-validated through the gateway, with one small ticket on a
  `github-copilot` route (see `openshell-sandbox-runtime.md`). The pifork harness specifically
  needs an operator-built local image (see `pifork-harness.md`'s image-contract
  section) and is opt-in and out of scope for the same reason.
