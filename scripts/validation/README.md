# Validation scripts

The executable half of
[`doc/designs/validation-plan.md`](../../doc/designs/validation-plan.md):
baseline and Temporal end-to-end runs on a throwaway repo, plus
deterministic stand-ins so gate and state-machine mechanisms can be
exercised without a model.

Not the release smoke test. That is `make live-smoke`
([`AGENTS.md`](../../AGENTS.md), "Live validation"), which also covers the
approved-oracle fixtures. For a first look at buildgate, start with
[`README.md`](../../README.md) and [`DEMO.md`](../../DEMO.md).

## Phase runners

Real Docker, real relay, real Temporal (Phase 2); nothing mocked. Run in
order: each assumes the previous phase passed. They take no flags; edit
the constants at the top.

| Script | Plan phase | What it does |
|---|---|---|
| `phase0-baseline.sh` | 0 | `make verify`, `make console-test`, `agent/pi` pytest, the live-gated Go tests, `factoryd doctor` |
| `phase1-direct-e2e.sh` | 1 (steps 1–3) | Scaffolds a throwaway repo, runs `intake` to both checkpoints, drives ticket 001 to `accepted`, audits evidence |
| `phase2-temporal-e2e.sh` | 2 (steps 0–1) | Same scaffold, run through a local Temporal server |

## Stand-ins (no model call)

Invoked *by* `factoryd` with a fixed argv; don't run them by hand.

| Script | Plan phase | Replaces | Behaviour |
|---|---|---|---|
| `phase1-gate-build-app.sh` | 1.4/1.6, 2 | `-build-app-script` | Makes the ticket's declared change and claims success, so the ticket's own `Verify-Command:`/`Allowed-Files:` is what gets tested |
| `phase1-gate-scope-violation.sh` | 1.4 (b) | `-build-app-script` | Touches a tracked file outside `Allowed-Files:`. Separate script because container env vars never reach a sandboxed build script |
| `phase1-gate-hang.sh` | 1.5, 2.3 | `-build-app-script` | Sleeps 600s so cancellation or a second run's wait ends it |
| `phase3-probe-build-app.sh` | 3 | `-build-app-script` | Runs non-destructive adversarial probes (network, filesystem, credential, Docker, history rewrite) inside a real sandbox and logs each; a probe needing `curl`/`getent`/`docker` reports their absence |
| `phase1-mock-draft-spec.py` | 1.7 | `draft_spec.py` | Writes a `spec.md` passing `internal/request.ValidateSpecSkeleton` |
| `phase1-mock-plan-tickets.py` | 1.7 | `plan_tickets.py` | Writes one ticket passing `ValidateTicketPlan`, `TicketStructureBrownfield` and `ValidatePlanCoverage` |
| `phase1-mock-build-app.py` | 1.7 | `worker`'s build step | Python because `worker` has no `-build-app-interpreter` flag, so the `.sh` stand-in can't be used there |

The `phase1-mock-*.py` scripts run under `python3` (`worker`'s fixed
interpreter). The spec/plan mocks create their own output directories,
since the drafting scratch directory is removed before each attempt.

The stand-ins cover the default request path (`spec_review` ->
`plan_review` -> `building`). None drafts oracles; the
`-draft-oracles` stage is covered by `make live-smoke` and the Go tests.

## Not scripted, deliberately

- **Phase 1 steps 4–7, Phase 2 concurrency/termination/flake.** These
  kill processes, restart Temporal or compare timings. The stand-ins are
  the building blocks; wiring them together is done by hand each pass.
- **Phase 3 (chaos).** The plan document is the checklist. A script that
  fills a disk or restarts shared Docker/Temporal is too dangerous to
  leave lying around.
- **Phase 4 (console).** `scripts/console-walk/run.sh`
  drives one scripted operator walk; the rest of the plan's checklist is
  manual.

## Ground rules

- Throwaway target repo only, never a real project's `main`.
- One live phase at a time: all model-backed commands share the same
  local model.
- Docker/colima running; for Phase 2, the Temporal stack too:
  `docker compose -f docker-compose.temporal.yml up -d`.
