# Status

What buildgate does today, what is opt-in, and its known limits. A change
that alters either table updates this file in the same PR. The summary is in
[`README.md`](README.md#status).

## What works

Latest release tag: **`m6`**. A tag is cut only after `make live-smoke`
passes on that commit, and other machines upgrade by tag, not from `main`.

✅ works · 🟡 partial or opt-in · ⬛ permanently out of scope, by design

| Area | | State |
|---|---|---|
| Request pipeline: spec review, plan review, sandboxed build, gates, PR | ✅ | Live-proven on several real repos and on the ChatGPT Codex route. Every build runs on Temporal, which starts automatically |
| Safety contract, containment, evidence | ✅ | `safety-contract.md` SC-001–SC-015 enforced by `CLAIMS.md` tests; no host-execution path |
| Sandbox runtime | ✅ | Every worker is launched by the OpenShell 0.1.2 gateway (images pinned by digest). Live-proven with `make live-smoke`, `make live-compose`, a two-ticket request from `submit` to its pull requests, and two builds with compose sidecars at once (their containers peak near 2.2 GB) |
| Dependencies started for you | ✅ | `submit`, `worker`, `console`, `quickstart` start whichever of Temporal, the `worker` and `serve` is missing, with a spinner and a one-line reason on failure. `FACTORYD_AUTOSTART=0` opts out (a build then needs `-temporal-address`) |
| `factoryd quickstart` onboarding | ✅ | From nothing configured to a request at its first review gate |
| Operator console (in `factoryd serve`) | ✅ | Request board, stepper, spec/plan/oracle review, edit, approve/reject |
| Model roles, routes, harness per role (`roles.<role>.harness`) | ✅ | `pi` (default) and `codex` live-proven on every role; `pifork` shares Pi's adapter; `copilot` (Copilot CLI) live-proven for the execution role on a `github-copilot` and a `chatgpt-codex` route. Requirements per harness: [USAGE.md](USAGE.md) |
| Per-request harness and model choice | ✅ | `submit -harness role=name`, `submit -model role=name` (repeatable), `planning`/`execution` only. The harness must be in the role's `allowed_harnesses`, the model in its `allowed`; `review` is never requester-selectable |
| GitHub Copilot route + Copilot CLI harness | 🟡 | Login detection, model listing and builds through the gateway on the stored GitHub login token; live-proven on one small ticket on an individual free plan. Waiting on a walk with a paid seat (see limits) |
| Compose service dependencies (Postgres, Kafka, Redis from the repo's `docker-compose.yml`) | ✅ | Allow-listed images only; a rejected compose file halts before any model spend; `compose_services_concurrency` (default 1) runs with sidecars per host; `factoryd doctor -target-repo <repo>` checks a repo before its first run |
| Handing over a finished spec and plan (`submit -spec-file`, `-plan-dir`) | ✅ | Checked against the spec and ticket skeletons at submit; reaches `spec_review`/`plan_review` with no drafting model call and gets every check a drafted document gets. A reject has the model revise your document with the feedback. [Reference](USAGE_REFERENCE.md#handing-over-a-finished-spec-and-plan-submit--spec-file--plan-dir) |
| Worker skills per role (`roles.<role>.skills`; built-ins ship in `factoryd`, `skill_dirs:` adds your own) | 🟡 | Opt-in; every harness. A host-side, hashed, read-only snapshot at `/inputs/skills`; no host harness folder is mounted. A same-name repo skill refuses the launch. [Reference](USAGE_REFERENCE.md#worker-skills-skill_dirs-rolesroleskills) |
| Team design guide (`.factory.yml` `design_guide`, session config `design_guide_dirs:`) | 🟡 | Opt-in. Its spec decisions reach spec drafting and its plan rules reach planning; name and digest recorded on the request. Not enforced on the built code. [Reference](USAGE_REFERENCE.md#design-guide-design_guide-design_guide_dirs) |
| Acceptance oracles (`submit -draft-oracles`) | 🟡 | Opt-in; Go and Python repos. A human reads and approves every drafted test |
| Conformity review (`spec_conformity`) | ✅ | An LLM opinion on the criteria, not a proof |
| Standalone AI code review (`code_review`), combined conformity + code review call | ✅ | `off`/`advisory`/`required`; CLI default `off`, `quickstart` writes `required` |
| Spec drafting: worked-example check | 🟡 | Advisory only: flags plain-integer worked examples that don't recompute |
| Cost | ✅ | Per-model prices, rollups by role × model, launch budgets (`factoryd cost`) |
| Local model-host serialization | ✅ | A single-instance host (local/private/Tailscale, keyed by the route's `upstream`) is locked host-wide (`internal/modelhost`); waiters show a `model_host_lock` event and a `waiting` chip |
| Weak-model advisory | ✅ | `quickstart` warns (never blocks) on a model known to perform poorly; one entry today, `gpt-4.1` |
| A run interrupted by sleep, stop, crash or reboot | ✅ | The lost step waits in `resume_review` for `factoryd resume` (a build continues in its kept worktree from the last completed round), `resume -from scratch` or `cancel`. No automatic retry |
| CI | 🟡 | Manual by operator decision; run `make ci` locally |
| Merge / deploy | ⬛ | Permanently human-gated: release decisions are recorded, never acted on |

## Known limits

| Limit | Detail |
|---|---|
| **Copilot is proven on a small scale only** | A `credential_mode: github-copilot` route and the Copilot CLI harness each took one small ticket to accepted through the gateway (2026-10-04), on an individual free plan with `gpt-4.1`. Unproven: a business or enterprise plan's host, a multi-ticket request on this route, and a model served only on `/responses` (the free plan lists such models and answers `model_not_supported` for each; the request does reach Copilot's `/responses` endpoint through the gateway). The route sends the GitHub login token itself as the bearer token; if GitHub stops accepting it, builds fail with the upstream's 401 and the route needs the short-lived token again, which the gateway cannot refresh inside a running sandbox |
| **Your own worker image needs a `make` built without `posix_spawn`** | OpenShell's sandbox denies every set-id system call, and a stock GNU make resets ids for each recipe line, so every recipe fails with "Operation not permitted". The image `make install` builds carries such a make; an image built on it (`make project-sandbox-image`) inherits it. Tools that switch user (`su`, `sudo`) fail the same way |
| **OpenShell 0.1.2 is young** | CPU and memory limits apply, but swap is twice the memory limit and the process limit is one value for every sandbox (1024). A worker can send a model request with a token it invents (the supervisor forwards a header the worker writes itself). The meter listens in plaintext on the Docker VM's loopback. A repository must be under your home directory. Each step on a credentialed route starts about 10 s after its sandbox, once the supervisor has installed the credential. [`containment-matrix.md`](containment-matrix.md) lists each residual |
| **A lost step is never retried automatically** | A laptop sleep, a lost worker, or a stopped, crashed or rebooted `factoryd` (`stop -force`, `upgrade`) halts the run; a lost build keeps its worktree. The request waits in `resume_review` and reminds you until you choose: `factoryd resume <id>` continues the build from the last completed round (fresh agent session plus a short handoff; refused when a container of the lost run is alive or the worktree's history changed) or reruns a lost drafting or planning step; `resume -from scratch <id>` rebuilds the ticket; `cancel <id>` withdraws it. A `worker` rebuilds the ticket from scratch at its next start. On macOS an idle Mac does not sleep mid-build (`caffeinate`); closing the lid still does |
| **Builds that need the repository alone can wait a long time** | Requests on one repository build concurrently, each in its own worktree and branch. A `-repository` run, a corrective `-on-branch` round, the daemon's reclaim and `factoryd reconcile` need every other build off the repository: they retry every 30 s without holding a job slot, and the lock has no writer preference, so while isolated builds keep overlapping they can wait indefinitely. Stranded worktrees are cleaned only when the repository is idle; `reconcile` reports it busy until then |
| Cold Go caches | Every worker container starts with an empty Go build/module cache (a worker-writable cache must never feed canonical verify), so each phase of a Go run compiles cold; on a large repo this is the main wall-clock cost |
| Narrow live coverage | Live validation covers a small fixture set (`testdata/fixtures`, todo-service, todo-kafka-service, the proving-ground corpus, the operator's own repos). Multi-module `go.work`, cgo, private modules and generated code are unproven |
| Oracles | Opt-in; default-on is undecided (`make bar`) |
| Code review off by default on the CLI | Until you opt in (or use `quickstart`, which writes `required`), correctness rests on `spec_conformity`, the deterministic gates and the human gates |
| CI | Manual-only; run `make ci` before merging a pipeline change |
