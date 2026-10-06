# Architecture and flows

Diagram-first companion to [`README.md`](../../README.md): what the
pieces are, what states a request moves through, and what the worker
and a run do step by step. Flags live in
[`USAGE.md`](../../USAGE.md)/[`USAGE_REFERENCE.md`](../../USAGE_REFERENCE.md);
a guided run-through is [`DEMO.md`](../../DEMO.md). Sandbox network/
filesystem containment has its own diagram in
[`containment-matrix.md`](../../containment-matrix.md#sandbox-topology) —
not duplicated here.

## 1. Components

```text
Operator
 |-- console (HTTPS+token) -> factoryd serve --+
 |-- factoryd submit / approve / reject / ... -+-> request record (request.json)
 |                                                    |  each decision wakes the workflow
 |                                                    v
 |                                    factoryd worker: RequestWorkflow, one per request
 |                                      drafting and planning (model jobs), then per ticket:
 |                                                    |
 |-- factoryd <run> (one ticket) ---------------------+
                                                      v
                                           RunWorkflow (Activities)
                                  [-repository: child of RepositoryOwnerWorkflow,
                                   one queue per repository]
                                                      v
                             Docker sandbox worker
                             (created by the OpenShell
                              gateway, no network)
                               |                  |
                               v                   v
                     supervisor + meter      policy gates
                     (only path to a       (canonical_verify,
                      model)                diff_scope, ...)
                          |                       |
                          v                  pass |   fail
       Anthropic / local LLM /                    |     |
       Copilot / ChatGPT Codex                     v     v
                                             evidence   quarantined/halted
                                                |               |
                                                v               |
                                          PR opened   <--retry--+
                                                |
                                                v
                                human merges (outside this system)
```

One execution path: every build is a `RunWorkflow` on Temporal, whether a
request's ticket under the worker or a single-ticket `factoryd <run>`. A
run whose Temporal is unreachable halts.

### Start-up: dependencies a command starts for itself

```text
factoryd submit / console / quickstart / worker / doctor -fix
   |
   |-- Temporal   healthy at localhost:7233? --no--> Docker usable and FACTORYD_AUTOSTART != 0?
   |                  | yes                            | yes: docker compose up -d (compose file
   |                  v                                |      embedded in factoryd), wait until healthy
   |              use Temporal <------ healthy --------+
   |                                                   | no / never healthy: one line why,
   |                                                   v no worker starts; a single run halts
   |-- worker  drain lock held for this data dir? --no--> start it (pid + log path printed)
   |-- serve      live <data-dir>/console-address?   --no--> start it
   v
submit prints the request id, then View: <console link> (or one line saying why not)
```

`worker` does the Temporal step (`submit` starts `worker`, which then
does it); `submit` and `console` do the `serve` step. While any of them
waits, a terminal shows one animated working line (`internal/spinner`);
piped or logged output gets one plain line per change.

## 2. Request lifecycle (console/API level)

A *request* is the console's own unit of work, not yet a sandboxed run.
`spec_review`/`plan_review` are always human stages; `-draft-oracles`
adds a third, `oracle_review`. `building` runs unattended. Merge is
never a state here — it stays a human action outside the system.

```text
submitted -> spec_drafting -> spec_review* -> planning -> plan_review*
                                                  |
                                              building -> pr_review -> done
                                                  |
                                    quarantined/halted --(retry)--> building

-draft-oracles only, inserted between spec_review and planning:
  spec_review* -> oracle_drafting -> oracle_review* -> planning
                        ^                  |
                        +--(reject)--------+

* = needs you (human review stage)
```

Rejecting at any review stage sends the request back to the matching
drafting stage with your reason attached. `cancelled` (operator
withdrawal) is a third off-ramp from any state, omitted above for space.

## 3. A request under the worker, in detail

`factoryd worker` drives every request of one data dir, each as one
`RequestWorkflow` (`internal/workflow/request_workflow.go`, workflow id
`factoryd-request-<request id>`). `request.json` stays the source of truth:
the workflow keeps no request state, and each step
(`internal/requestdriver`) saves its own outcome.

```text
loop
  LoadRequestStep            read request.json, say what is next   (light queue)
    |-- done / cancelled ---------------------------> workflow ends
    |-- submitted ----------------------------------> AdvanceRequest (light queue)
    |-- spec_drafting, oracle_drafting, planning,
    |   building, pr_review ------------------------> AdvanceRequest (jobs queue,
    |                                                 max_parallel_jobs at once):
    |                                                 one drafting job, the plan,
    |                                                 one ticket's build (a RunWorkflow,
    |                                                 section 4), or one PR poll
    |-- spec_review, oracle_review, plan_review,
    |   resume_review ------------------------------> wait for a decision;
    |                                                 RemindRequest every
    |                                                 -hitl-reminder-interval
    |-- halted, quarantined ------------------------> wait for a decision
  the step changed the state ------------------------> next iteration at once
  the step was lost (no heartbeat for 2 minutes) ----> HaltLostRequestStep:
                                                       request -> resume_review
```

- A decision (`approve`, `reject`, `retry`, `resume`, `cancel`, amend scope,
  send back; CLI or console) is saved to `request.json` first, then a
  `request-wake` signal ends the wait.
- A jobs-queue step runs once. Nothing reruns a lost model job or build: the
  request waits in `resume_review` for `factoryd resume <id>`,
  `resume -from scratch` or `cancel`. A worker restarted mid-build puts the
  requests it was building there at start.
- A build on a repository another build holds alone gives its job slot back
  and retries every 30 s; `pr_review` reads the PR at most once per
  `-pr-poll-interval`.
- The two task queues are named after a random id kept in
  `<data-dir>/worker-queue-id`.

## 4. A ticket's run, in detail

`factoryd <run>` and every ticket build of a request (Temporal at
`localhost:7233`, or `-temporal-address <addr>`). The command first prepares
the run (`cmd/factoryd/run_ticket.go`: flags, routes, sandbox settings,
project check, repository lock, spec snapshot, run record), then starts
`RunWorkflow`, where every step is a named Activity. Sequence below is the
real one, taken from `internal/workflow/workflow.go`.

```text
signal-with-start RepositoryOwnerWorkflow (-repository only)
  v
RunWorkflow starts
  v
CaptureBaseSHAActivity            fresh HEAD read
  v
PrepareIsolatedWorkspaceActivity  worktree + branch
  v
PreflightActivity                 project-bootstrap checks
  v
RunBuildActivity                  launches sandbox, runs build_app.py
  v
PostBuildActivity                 ancestor check on result commit
  v
RunVerifyActivity                 canonical verify command
  v
CollectEvidenceActivity           changed-file inventory, evidence
  v
[RunReviewStepActivity]           conditional, once per enabled step
  v                                (spec_conformity and code_review; one
                                   combined call when both are enabled)
EvaluateRunActivity               policy gates on collected evidence
  v
DisableWorkerGroupWriteActivity   revoke worker write access
  v
RunWorkflow result: accepted / quarantined / halted
```

Conditional Activities not on every run: `CheckChainSuccessorActivity`/
`ValidateSliceChainActivity` (multi-ticket chain), `RollbackIsolatedWorkspaceActivity`
(setup fails before build), `RunFullSuiteVerifyActivity`/`RunNamedGateActivity`
(declared full-suite/named-gate policy). Oracle-carrying runs add, in
order, `RunNamedGateActivity` for `reference_oracle`, `CommitOraclesActivity`
(after every gate and any conformity review passed), then
`RunPostOracleCommitVerifyActivity`; see
[`USAGE_REFERENCE.md`](../../USAGE_REFERENCE.md) "Oracle-approved runs".

Completed Activities are durable: their checkpoints are reused, never
redone. An interrupted build, verify, gate or review step is never rerun
by Temporal: the run halts with a lost build's worktree kept, and a request
run by `factoryd worker` waits in `resume_review` for a human to resume it
(a fresh agent session plus a handoff, from the last completed round),
rebuild it or cancel it; see
[`USAGE.md`](../../USAGE.md) "A lost step waits for a human". `-repository` serializes
overlapping runs against the same repo onto one task queue via
`RepositoryOwnerWorkflow`, so two submissions never race.

## 5. Where this comes from

- [`README.md`](../../README.md) — prose overview, install, status
- [`USAGE.md`](../../USAGE.md) / [`USAGE_REFERENCE.md`](../../USAGE_REFERENCE.md) — operator walkthrough and reference
- [`containment-matrix.md`](../../containment-matrix.md) — sandbox network/filesystem boundary
- [`safety-contract.md`](../../safety-contract.md) — trust boundaries, threat model, run state graph (SC-001–SC-015)
- `internal/workflow/request_workflow.go`, `internal/requestdriver` — the request loop and its steps, source for §3
- `internal/workflow/workflow.go` — `RunWorkflow`'s real Activity sequence, source for §4
