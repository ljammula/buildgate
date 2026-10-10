# Run progress feed — contract

The progress feed is what lets an operator follow a run while it executes:
`factoryd watch`, the console's Timeline, and the `GET /runs/{id}/progress`
route all read it. It is a small, append-only sidecar per run, written at
every stage boundary by the command that prepares the run and by the
workflow's Activities, plus
lines relayed from the sandboxed worker's stdout. Every Go and TypeScript comment
that cites `progress-contract.md` means this file. Shipped 2026-09-17/18
(see the `operator-follow-along` PR).

Design rule: **`run.json` is never modified for progress.** The feed is a
sidecar to the authoritative record, and a feed write failure is logged and
ignored — it must never fail or halt a run. No gate, policy, or state
transition reads the feed.

```text
factoryd (direct/Temporal)  worker stdout (FACTORY_PROGRESS ...)
        |                          |
        v                          v  (parsed by internal/sandbox/docker.go)
   progress.jsonl  <----------------+   (append-only, one JSON object/line)
        |
        +--> factoryd watch (transcript + footer)
        +--> GET /runs/{id}/progress (SSE)
        +--> GET /runs, GET /runs/{id} (tail-derived summary fields)
        +--> console Timeline
```

## File

`<data-dir>/runs/<run-id>/progress.jsonl` (`internal/progress.Path`).
One JSON object per line, never rewritten.

```json
{"ts":"2026-09-17T10:00:00.123Z","source":"factory","stage":"build","event":"start"}
```

| Field | Meaning |
|---|---|
| `ts` | RFC3339 UTC with milliseconds, stamped by factoryd. Never taken from the worker. |
| `source` | `factory` (written by factoryd Go code) or `worker` (relayed from sandbox stdout). |
| `stage` | Fixed vocabulary below. |
| `event` | `start`, `end`, or `note`. |
| `round`, `max_rounds` | Integers, omitted when 0. |
| `outcome` | `""`, `pass`, `fail`, or for `stage=finished` the terminal run state (`accepted`/`quarantined`/`halted`). |
| `detail` | Free text, truncated to 500 characters. |

## Factory stages (`source: factory`)

Emitted by `cmd/factoryd/run_ticket.go` (before the workflow starts) and by
the Activities in `internal/workflow/activities_*.go`, via the
`progressMark` helpers:

`prepare_workspace`, `preflight`, `baseline_verify` (the `end` event's
`detail` is the result: `passed`, or `failed` with the failing test named),
`build` (one `start`/`end` per attempt,
`detail` = `attempt N/M`), `post_build`, `verify`, `full_suite`, `gate`
(`detail` = gate name, e.g. `diff_scope`), `evidence`, one of
`conformity_review`/`code_review`/`review` (`internal/reviewstep.Plan`
picks exactly one per run: standalone spec-conformity, standalone code
review, or `review` when both are enabled in one combined launch),
`commit_oracles`, `post_oracle_commit_verify`, `evaluate`, `finished`.

A build attempt that resumes after a lost worker also writes one
`build` `note` line, `detail` = `build attempt N resumed after the worker
stopped; kept the earlier attempt's work (checkpoint <short sha>)`.

Each stage gets a `start` line and an `end` line carrying `outcome`. A stage
skipped by configuration never appears. `finished` is a single `end` line
written by `cmd/factoryd`'s `save()` the first time the run reaches a
terminal state, after `run.json` is saved; its `detail` is the halt or
quarantine reason when there is one. It marks the end of *execution*: a
quarantined run, or a halt not yet confirmed, still gets one, and a later
operator override or reclaim confirmation changes `run.json` (carried by
`GET /runs/{id}/events`) without appending a second `finished` line.

Two factory `note` lines exist:

- `queued` — written by the submitting process while a `-repository`-
  serialized run waits its turn: `behind N run(s) on <repository>`,
  re-emitted only when N changes (`cmd/factoryd/run_repository_owner.go`,
  from `RepositoryOwnerResult.QueuedRequestIDs`).
- `model_host_lock` — not a note: event `waiting` (`detail` = `queued
  behind <run-id>`, re-emitted when the holder changes) while a run waits
  for a single-instance model host's lock (`internal/modelhost`), then
  event `acquired` (no detail) once it holds the slot. Written by the
  request-level jobs (`cmd/factoryd/sandbox_exec.go`: spec, plan and oracle
  drafting) and by a run's Activities
  (`internal/workflow/activities_sandbox.go`).
- `compose_services_lock` — event `waiting` (same detail) while a run
  whose compose file launches sidecars waits, before its first phase, for
  the host-wide sidecar slot (`sandbox.AcquireComposeServicesGate`,
  `compose_services_concurrency`), and `acquired` whenever it takes the
  slot, waited or not; written by `cmd/factoryd/run_ticket.go` before the
  run starts, and by the daemon when it reclaims a crashed run.
- `build` note — written once after evidence is loaded, `detail` = a
  factory-formatted round summary derived from `BUILD_EVIDENCE.json`,
  e.g. `3 rounds · r1 fail (verify) · r2 fail (verify) · r3 pass · 41.2k
  tokens · $0.12` (`cmd/factoryd/round_summary.go`). Derived from
  agent-reported evidence, so informational, like `AgentEvidence` itself.

## Worker stages (`source: worker`)

`agent/pi/scripts/build_app.py` prints lines of the exact form

```
FACTORY_PROGRESS {"stage":"round","event":"start","round":2,"max_rounds":6}
FACTORY_PROGRESS {"stage":"round","event":"end","round":2,"max_rounds":6,"outcome":"fail","detail":"verify failed: go test ./..."}
FACTORY_PROGRESS {"stage":"agent","event":"note","round":2,"detail":"bash: go test ./internal/..."}
```

The prefix is the literal `FACTORY_PROGRESS ` at column 0 followed by one
JSON object on one line. The sandbox log scanner (`internal/sandbox/
docker.go`) still writes the raw line to the build log unchanged and, when
`LaunchSpec.ProgressPath` is set, appends the parsed object with
`source: worker` and its own `ts`. Only `stage` in {`round`, `agent`},
`event` in {`start`, `end`, `note`}, integer `round`/`max_rounds`,
`outcome` in {`""`, `pass`, `fail`} and a string `detail` are accepted
(`internal/progress.ParseWorkerLine`); anything else is dropped silently.
At most 5000 `agent` lines and 500 `round` lines are relayed per launch, budgeted separately so chatty notes cannot starve the round counter. Worker output is untrusted and
display-only. `source: worker` means "a process in the sandbox wrote this line": the
build script and the coding agent run as one user and both can write the output
file the host tails, so the host cannot tell the script's line from one the agent
appended (safety-contract.md, SC-018). A worker line is never recorded with
`source: factory`, with its own `ts`, or in a stage other than `round` and `agent`
(`TestAProgressLineFromTheSandboxIsRecordedAsTheWorkersNeverTheFactorys`).

## HTTP and server-computed fields

`GET /runs/{id}/progress` on `factoryd serve`: SSE, `event: progress`,
`data: <one raw JSON line>`. Sends every existing line, then follows the
file at the server's poll interval, and ends once the run is terminal and
the file is drained (with one extra drain after terminal is observed, since
`finished` lands just after `run.json`). Same read authorization as
`GET /runs/{id}/events`.

`GET /runs` and `GET /runs/{id}` add, for non-terminal runs only, fields
computed from the tail of the feed (`internal/progress.Summary`):
`last_progress_at`, `current_stage`, `current_round`, `max_rounds`,
`waiting_reason` (the latest `queued` note's or `model_host_lock`/
`compose_services_lock` `waiting` event's detail, cleared when a stage
starts or the lock is `acquired`). Consumers flag a non-terminal run as **stalled** when
`now − last_progress_at` exceeds 5 minutes (or, with no lines, `now −
created_at`).

## Related surfaces

- `factoryd watch <run-or-request-id>` (`cmd/factoryd/watch.go`) renders
  the feed as a transcript with a status footer and prints a final recap.
- The console's run Timeline (`console/src/features/run-detail/Timeline.tsx`) and
  the request Pipeline stepper (from `Request.History`, a separate
  per-request transition log in `internal/request`).
- `RunWorkflow` also answers a `run-progress` query
  and build/verify Activities heartbeat with `{stage, elapsed}`; those are
  Temporal-side views of the same stages, not a second feed.
