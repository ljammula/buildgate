# buildgate

Buildgate (binary: `factoryd`) is an **unattended agent factory**. You give
it a ticket and a spec; it drives a coding agent through a sandboxed
build/verify loop to an accepted, mergeable state.

- Every build runs in a Docker container launched by the [OpenShell](https://github.com/NVIDIA/OpenShell) gateway, with no network interface; the model is reached only through the sandbox's supervisor, under a per-run policy and buildgate's own spend meter.
- Nothing advances on the agent's own say-so: each stage is a factory-owned policy decision against durable evidence (test results, diff shape, hashes).
- Humans review the spec and the plan (and, opt-in, the acceptance tests) in the console or CLI before code is written.
- Merge and deploy are **permanently human**, by standing design: the factory records a decision and may open a PR, it never merges.
- Go orchestration daemon (`factoryd`), Flutter web operator console embedded in `factoryd serve`, vendored build scripts under `agent/` (one set for every harness).

Licensed under [Apache 2.0](LICENSE). Design and rationale:
[`doc/designs/architecture-flows.md`](doc/designs/architecture-flows.md) and
[`safety-contract.md`](safety-contract.md).

## Try it in 10 minutes

1. Install (below), start Docker, have a model route ready.
2. Run `factoryd doctor`, then `factoryd quickstart ~/code/your-repo "Add X to Y"`: [USAGE.md quickstart](USAGE.md#quick-start-existing-repo).
3. Want a live walkthrough with talking points (with and without acceptance oracles, plus a failure case)? Follow [`DEMO.md`](DEMO.md).

## The pipeline at a glance

A request (`factoryd submit` / `quickstart`) moves through these states.
Humans act at the review gates (`spec_review`, `oracle_review` if opted in,
`plan_review`, `pr_review`); everything else is the factory.

```text
submitted -> spec_drafting -> [spec_review] -----------------+
                                    |                         |
                    -draft-oracles (opt-in)                   |
                                    v                         |
                            oracle_drafting -> [oracle_review] |
                                    |                          |
                                    +------------> planning <--+
                                                       |
                                                  [plan_review]
                                                       |
                                                       v
                                        building (sandbox + gates)
                                            |              |
                                      gate fails      crash/timeout
                                            v              v
                                     quarantined        halted
                                            |
                                            v (gates pass)
                                       [pr_review] (draft PR)
                                            |
                                            v
                                   human merges (never the factory)
```

Per ticket, `building` runs the agent in the sandbox, then the gates:
`canonical_verify`, `diff_scope`, `required_files_changed`, `tests_added`,
`full_suite_verify`, `spec_conformity`, `code_review` (`-code-review-policy
off|advisory|required`, default `off` on the CLI; `quickstart` writes
`required` for new configs), and (with
oracles) `reference_oracle` with a runtime canary. One execution path,
live-proven: Temporal, for every command (`worker`, the single-ticket run,
`live-smoke`; started with Docker when down; a run whose Temporal is
unreachable halts). A lost build keeps its worktree for `factoryd resume`;
nothing reruns a lost step on its own.

The PR:

- Title comes from the ticket's `## Goal`; the branch is `factoryd/<slug>-<shorthash>`.
- Opens as a draft; `factoryd` marks it ready for review (`gh pr ready`) once checks pass and no reviewer thread blocks it.

`factoryd retry` re-runs a halted or quarantined request. On an accepted
ticket whose PR never opened, it re-opens only the PR, pinned to the
accepted commit (`ResultSHA`), after re-running the release decision; a
denial falls back to a full rebuild of that ticket only.

## What works today, what is opt-in, known limits

| Area | State |
|---|---|
| Request pipeline: spec review, plan review, sandboxed build, gates, PR | Works; live-proven on several real repos and on the ChatGPT Codex route |
| Execution | Every build runs on Temporal, live-proven; it starts automatically |
| Dependencies started for you | `submit`, `worker`, `console`, `quickstart` start whichever of Temporal, the `worker` and `serve` is missing, with a spinner and a one-line reason when one cannot start; `FACTORYD_AUTOSTART=0` opts out (and a build then needs `-temporal-address`) |
| `factoryd quickstart` onboarding | Implemented; takes a git checkout from nothing configured to a request waiting at its first review gate |
| GitHub Copilot route | Works: login detection, model listing, and builds through the gateway on the stored GitHub login token; live-proven on one small ticket with an individual free plan (see limits) |
| Sandbox runtime | Every worker is launched by the OpenShell 0.1.2 gateway (images pinned by digest); live-proven with `make live-smoke`, `make live-compose`, a two-ticket request from `submit` to its pull requests, and two builds with compose sidecars at once (their containers peak near 2.2 GB) |
| Coding-agent harness per role (`roles.<role>.harness`) | `pi` (default) and `codex` live-proven on every role; `pifork` (a fork of Pi in an operator-built image) shares Pi's adapter; `copilot` (Copilot CLI) live-proven for the execution role on a `github-copilot` route and on a `chatgpt-codex` route. Requirements per harness: [USAGE.md](USAGE.md) |
| Compose service dependencies (Postgres, Kafka, Redis from the repo's `docker-compose.yml`) | Works: allow-listed images only, a rejected compose file halts before any model spend, `compose_services_concurrency` (default 1) runs with sidecars per host; `factoryd doctor -target-repo <repo>` checks a repo before its first run |
| Operator console (in `factoryd serve`) | Works: request board, stepper, spec/plan/oracle review, edit, approve/reject |
| Per-request harness and model choice | Works: `submit -harness role=name` and `submit -model role=name` (both repeatable) are limited to the `planning`/`execution` roles; a harness must be in that role's own `roles.<role>.allowed_harnesses` and a model in its `roles.<role>.allowed` -- `review` is never requester-selectable |
| Worker skills per role (`roles.<role>.skills`; built-ins ship in `factoryd`, `skill_dirs:` adds your own) | Opt-in. Works on every harness: a host-side, hashed, read-only snapshot at `/inputs/skills`; no host harness folder is mounted. A same-name repo skill refuses the launch. [USAGE_REFERENCE.md](USAGE_REFERENCE.md#worker-skills-skill_dirs-rolesroleskills) |
| Handing over a finished spec and plan (`submit -spec-file`, `-plan-dir`) | Works: checked against the spec and ticket skeletons at submit; the spec reaches `spec_review` and the tickets `plan_review` with no drafting model call, and get every check a drafted document gets. A reject has the model revise the operator's document with the feedback. [USAGE_REFERENCE.md](USAGE_REFERENCE.md#handing-over-a-finished-spec-and-plan-submit--spec-file--plan-dir) |
| Team design guide (`.factory.yml` `design_guide`, session config `design_guide_dirs:`) | Opt-in. Works: the guide's spec decisions reach spec drafting and its plan rules reach planning; name and digest recorded on the request. Not enforced on the built code. [USAGE_REFERENCE.md](USAGE_REFERENCE.md#design-guide-design_guide-design_guide_dirs) |
| Conformity review (`spec_conformity`) | Works; an LLM opinion on the criteria, not a proof |
| Standalone AI code review (`code_review`, `-code-review-policy`) | Works; `off`/`advisory`/`required`, CLI default `off`, `quickstart` writes `required` for new configs |
| Spec drafting: worked-example check | Advisory only: flags plain-integer worked examples that don't recompute |
| Acceptance oracles (`submit -draft-oracles`) | **Opt-in**; Go and Python repos. A human reads and approves every drafted test |
| Local model-host serialization | Automatic: a single-instance host (local/private/Tailscale, keyed by the selected route's own `upstream`) is locked host-wide (`internal/modelhost`); waiters show a `model_host_lock` event and a `waiting` chip |
| Weak-model advisory | `quickstart` warns (never blocks) on a model known to perform poorly; currently one entry, `gpt-4.1` |
| CI | Manual by operator decision; run `make ci` locally |
| Merge / deploy | Permanently human; not a gap |

Known limits:

- **Copilot is proven on a small scale only.** A `credential_mode: github-copilot` route and the Copilot CLI harness each took one small ticket to accepted through the gateway (2026-10-04), on an individual free plan with `gpt-4.1`. A business or enterprise plan's host, a multi-ticket request on this route, and a model served only on `/responses` are unproven: the free plan lists such models and answers `model_not_supported` for each (the request does reach Copilot's `/responses` endpoint through the gateway). The route sends the GitHub login token itself as the bearer token (the Copilot API accepts it); if GitHub stops accepting it, builds on this route fail with the upstream's 401 and the route needs the short-lived token again, which the gateway cannot refresh inside a running sandbox.
- **Your own worker image needs a `make` built without `posix_spawn`.** OpenShell's sandbox denies every set-id system call, and a stock GNU make resets ids for each recipe line, so every recipe fails with "Operation not permitted". The worker image `make install` builds carries such a make; an image built on it (`make project-sandbox-image`) inherits it. Tools that switch user (`su`, `sudo`) fail the same way.
- The sandbox runtime is OpenShell 0.1.2, a young project: CPU and memory limits apply, but swap is twice the memory limit and the process limit is one value for every sandbox (1024); a worker can send a model request with a token it invents (the supervisor forwards a header the worker writes itself); the meter listens in plaintext on the Docker VM's loopback; a repository must be under your home directory (the gateway sees nothing else); each step on a credentialed model route starts about 10 s after its sandbox, once the supervisor has finished installing the credential. [`containment-matrix.md`](containment-matrix.md) lists each residual.
- **A lost step is never retried automatically.** A laptop sleep, a lost worker, or a stopped, crashed or rebooted `factoryd` (`stop -force`, `upgrade`) halts the run, and a lost build keeps its worktree. Under the worker the request then waits in `resume_review` and reminds you until you choose: `factoryd resume <id>` continues the build from the last completed round (fresh agent session plus a short handoff; refused when a container of the lost run is alive or the worktree's history changed), or reruns a lost drafting or planning step; `factoryd resume -from scratch <id>` rebuilds the ticket; `factoryd cancel <id>` withdraws it. A `worker` rebuilds the ticket from scratch at its next start. On macOS an idle Mac does not sleep mid-build (`caffeinate`); closing the lid still does, and a `worker`'s build then waits for your decision.
- Requests on one repository build concurrently, each in its own worktree and branch. Only a build that touches the shared checkout (`-repository`) or a corrective round on an existing branch (`-on-branch`) holds the repository alone: it waits for the other builds, retrying every 30 s without holding a job slot. Stranded worktrees are cleaned when the repository is idle.
- **Builds that need the repository alone can wait a long time.** A `-repository` run, a corrective `-on-branch` round, the daemon's reclaim and `factoryd reconcile` need every other build off the repository, and the lock has no writer preference: while isolated builds keep overlapping they can wait indefinitely. Stranded worktrees are cleaned up only when no build holds the repository, and `factoryd reconcile` reports it busy until then.
- Oracles are opt-in; default-on is undecided (`make bar`).
- Every worker container starts with an empty Go build/module cache (a worker-writable cache must never feed canonical verify), so each phase of a Go run compiles cold; on a large repo this is the main wall-clock cost.
- Live validation covers a small fixture set (the fixtures under `testdata/fixtures`, todo-service, todo-kafka-service, the proving-ground corpus, and the operator's own repos). Repo shapes outside it (multi-module `go.work`, cgo, private modules, generated code) are unproven.
- The standalone AI code-review pass (`code_review`, `-code-review-policy`) is opt-in and off by default on the CLI; until an operator opts in (or uses `quickstart`, which writes `required`), correctness rests on `spec_conformity`, the deterministic gates, and the human gates.
- CI is manual-only; run `make ci` before merging a pipeline change.

## Install

Source-only: buildgate's own Docker images are built from source on your own
machine, never pulled from a registry; OpenShell's three images (gateway,
sandbox, supervisor) are pulled by pinned digest (`make openshell-images`).

### A new Mac, start to finish

About ten minutes from a Mac with only Homebrew, most of it image builds.

```sh
# 1. Prerequisites (skip what you have). Docker needs the buildx plugin.
brew install go python gh git colima docker docker-buildx
mkdir -p ~/.docker/cli-plugins && ln -sf "$(brew --prefix)/lib/docker/cli-plugins/docker-buildx" ~/.docker/cli-plugins/docker-buildx
colima start --memory 4
gh auth login

# 2. Install (clone, build the images, install factoryd)
git clone https://github.com/ljammula/buildgate.git && cd buildgate
make install
export PATH="$PATH:$(go env GOPATH)/bin"   # also add this to ~/.zshrc

# 3. Check, then run your first request (any git checkout under $HOME)
factoryd doctor -fix
factoryd quickstart ~/code/your-repo "Add X to Y"
```

`quickstart` starts everything else (Temporal, the OpenShell gateway and
meter, a worker, the console) and stops at the first review gate,
printing the exact `factoryd approve` command. With a ChatGPT/Codex login
(`codex login`) it picks that route and `gpt-5.6-luna` itself; to run the
Codex CLI as the coding agent too, add `-route chatgpt-codex -harness
codex` ([USAGE.md § Quick start](USAGE.md#quick-start-existing-repo)).
Keep the repo under `$HOME`: the sandbox gateway sees only your home
directory, and the Docker VM does not share `/tmp`. To start over, `factoryd uninstall -purge`
(see [Uninstall](#uninstall) below), then `make install` again.

The details of `make install`:

```sh
make install
```

Prerequisites: Go, Docker with the `buildx` plugin, `gh`, `python3` (Flutter
optional, for the console). With Homebrew's `docker` + `colima`, run
`brew install docker-buildx` and link it:
`mkdir -p ~/.docker/cli-plugins && ln -sf "$(brew --prefix)/lib/docker/cli-plugins/docker-buildx" ~/.docker/cli-plugins/docker-buildx`.
`make install` checks for it first and `factoryd doctor` warns when it is
missing. `make install` puts `factoryd` in `$(go env GOPATH)/bin` (usually
`~/go/bin`); if that is not on your `PATH`, it prints the `export PATH=...`
line to add to your shell profile. `make install`:

- runs `go install ./cmd/factoryd`;
- requires Docker, starts Temporal, and builds the sandbox, meter and registry-proxy images from source (`internal/sandbox/Dockerfile` and friends) and pulls OpenShell's three images by digest, recording buildgate's own via `factoryd configure-images` -- skipping the build of an image whose inputs haven't changed (it still pushes, so the recorded digest is unchanged) (`FORCE_IMAGE_BUILD=1 make install` always rebuilds). The refs go into the default session config; `FACTORYD_CONFIG="<config> <config>..."` re-points each listed config instead, so a second config (e.g. a Luna profile) does not keep pinning a superseded image;
- bakes in the console if Flutter is on `PATH` (else serves a placeholder page; see [`console/README.md`](console/README.md));
- leaves `git status` clean;
- warns if an older `factoryd` earlier on `PATH` shadows the one just installed;
- installs the `buildgate` agent skill into `~/.agents/skills` (Copilot, Codex), and refreshes `~/.claude/skills/buildgate` when it exists.

If the host uses TLS interception, pass its PEM CA bundle as
`BUILD_CA_BUNDLE=/path/to/ca.pem make install`. The bundle is mounted only as
a BuildKit secret for host-side dependency downloads; TLS verification remains
enabled and the bundle is not copied into an image. pip (in
`make project-sandbox-image`) uses it in place of its own roots, so include any
public roots for hosts the proxy does not intercept.

Then, with Docker running:

```sh
factoryd quickstart ~/code/your-repo "Add X to Y"
```

This takes a git checkout from nothing configured to a request waiting at
its first review gate. Prerequisites and each step:
[USAGE.md § Quick start](USAGE.md#quick-start-existing-repo).
`factoryd serve` hosts the console; open the URL its startup log prints.
`-issue <github issue url>` submits straight from an issue instead of
inline text.

To drive Buildgate from a coding agent, use its `buildgate` skill: the
commands the agent may run, how to submit and follow a request, and which
gates stay yours. `make install` installs it for Copilot and Codex; for
Claude Code, run once:

```sh
factoryd install-skill -dir ~/.claude/skills
```

Then say "use buildgate to implement `x_spec.md` in `~/code/<repo>`": the
agent submits it (an existing repo without Buildgate's spec docs runs as
brownfield), follows it, and hands you the console and Temporal links;
every approval and the merge stay yours. `factoryd upgrade` refreshes the
skill. [USAGE.md § Drive Buildgate from a coding agent](USAGE.md#drive-buildgate-from-a-coding-agent).

Model prices (what a dollar cost budget/ceiling is computed from) live in
`internal/prices/prices.yml`, keyed by provider model id with source/as_of
metadata -- edit it and run `make install` when a provider changes its
prices. A model with no entry costs $0; `factoryd doctor` shows each
configured model's price and warns when it is missing or stale. See [USAGE_REFERENCE.md § Model prices](USAGE_REFERENCE.md#model-prices).

### Troubleshooting a fresh Mac

| Symptom | Cause | Fix |
|---|---|---|
| `make install`: `the --mount option requires BuildKit` or `docker buildx` missing | Homebrew CLI-only Docker has no `buildx` plugin | See Install above; `factoryd doctor` warns about it |
| `factoryd: command not found` after install | `$(go env GOPATH)/bin` is not on `PATH` | `export PATH="$PATH:$(go env GOPATH)/bin"`, and add it to your shell profile |
| `doctor` warns the Docker VM shares all of `$HOME` | colima's default mounts | USAGE.md, "Data directory and colima" |
| `doctor` fails `roles.execution is not configured` | No model route yet | `factoryd quickstart` writes one (`-route chatgpt-codex` for a Codex login) |
| `make install` (or any command) fails `parse .../config.yml: relay_image: deleted with the inference relay` | A `config.yml` written for the retired inference relay still has `relay_image` or `relay_upstream_timeout` | Delete that line; the error names the retired key it found |
| `quickstart` fails `the repository ... is outside your home directory`, or `mount visibility (repository reachable inside a container)` | The sandbox gateway sees only your home directory, and the Docker VM must also share the repository's path (`/tmp` is neither) | Clone or move the repo under `$HOME`; if it is under `$HOME` and still fails, add it to your VM's mounts (USAGE.md, "Data directory and colima") |
| A request halts "the meter does not answer" / "sandbox runtime ... run `factoryd doctor -fix`" | The OpenShell gateway/meter is down (reboot, colima restart) or the config lost `meter_image` | `factoryd doctor -fix`; `worker` starts them too unless `FACTORYD_AUTOSTART=0` |
| Stop what is running / drop a request | | `factoryd stop`; `factoryd cancel <request-id>` |

### Uninstall

`factoryd uninstall -dry-run` prints the plan; `factoryd uninstall` removes
what `make install` put on the machine and keeps `~/.config/factoryd` and
`~/buildgate`. For a from-nothing reinstall add `-purge`: it also deletes both,
the Temporal volumes and the OpenShell gateway's state on the Docker VM
(database, keys, stored credentials); you type `purge` to confirm. Back up
`~/.config/factoryd` and `~/buildgate` first if you want your requests and
evidence. Then `make install`.

## How a run flows

```text
  Ticket + spec
       |
       v
  Sandboxed build (Docker via the OpenShell gateway,
  no network; model reached only via the supervisor
  and the meter)
       |          \
       |           \-- worker lost (sleep, crash, stop): the run halts, the work
       |            \  is kept, the request waits in resume_review for
       |             \-- `factoryd resume` / `resume -from scratch` / `cancel`
       v
  Canonical verify
  + policy gates
       |        \
    pass         fail
       |           \
       v            v
   Accepted      Quarantined
       |
       v
  Release policy decision (recorded only)
       |
    allowed
       |
       v
  Human merges (permanently outside this system)
```

Follow a run with `factoryd watch <run-or-request-id>`, the console's
Timeline and Pipeline views, or the Temporal Web UI:
[USAGE.md § Observe and control](USAGE.md#observe-and-control). Exact state set and
invariants: [`safety-contract.md` § State transitions](safety-contract.md#state-transitions).

## Model routes

`factoryd quickstart` detects an existing Codex login, Pi Copilot
login, or `ANTHROPIC_API_KEY` and offers it first (`-route` picks one
explicitly: `chatgpt-codex`, `copilot`, `anthropic`, `openai`). Under
`-non-interactive`, only a single unambiguous file-based login is picked
automatically; an inherited `ANTHROPIC_API_KEY` never is. A detected Copilot
login is saved as a routes: entry's `github_token_file`, never as a token.

`factoryd doctor -list-models` lists the models the configured route serves.
A Copilot model that can't run under the configured worker API
(e.g. a `/responses`-only model with no model `api: openai-responses`, or
`policy: disabled`) is refused with the reason (`meter.CopilotModelUsable`).
`-egress-ca-bundle <pem>` (session config `egress_ca_bundle`) trusts a
corporate TLS-interception CA in the registry proxy.

| Route | What to set |
|---|---|
| OpenAI-compatible endpoint (local ai-stack, vLLM, Ollama, OpenAI, any gateway) | A `routes:` entry with `upstream: <URL>`, `allowed_path_prefix: /v1` (must match `worker_base_path`), `credential_header: Authorization` for a credentialed endpoint; a `models:` entry with `id: <id from GET <URL>/v1/models>` and, for a Responses-only model, `api: openai-responses` |
| Anthropic (placeholder, not live-tested) | A `routes:` entry with `upstream: https://api.anthropic.com`, `allowed_path_prefix: /v1`, `worker_base_path: /v1` (Anthropic's OpenAI-compatible endpoint, not its native Messages API); a `models:` entry with `id: claude-sonnet-5` and `api: openai-completions`; `ANTHROPIC_API_KEY` set |
| GitHub Copilot (Pi, a Pi fork or the Copilot CLI harness) | A `routes:` entry with `credential_mode: github-copilot`; a `models:` entry with one entitled `id` (`factoryd doctor -list-models` lists them); a host-side Pi `auth.json` (a fork's own file: the route's `github_token_file`). A Pi fork also needs `harness: pifork` on each role that should run it and a digest-pinned local worker image |
| ChatGPT (Codex OAuth) | A `routes:` entry with `credential_mode: chatgpt-codex`; a `models:` entry with `id: gpt-5.6-luna`; default harness `pi`. Reads `~/.codex/auth.json` fresh per launch and pushes the token to the gateway's credential store, never refreshes or writes the file; a launch whose token expires before the step's time budget ends is refused (fix: run any `codex` command). Upstream/path are pinned; a leftover `upstream`/`allowed_path_prefix`/`worker_base_path` on that route fails startup naming the key |

Costs on `chatgpt-codex`/`github-copilot` runs are labelled
"(API-price est.; billed to your subscription)", or "(includes
subscription-billed runs; API-price est.)" on aggregates.

Copilot credential lookup order (host-side only, never copied into the
worker): the route's `github_token_file`, `GITHUB_COPILOT_TOKEN`,
`~/.pi/agent/auth.json`.

Pi is the default harness (`roles.<role>.harness`; `codex` and `copilot` are also available per role, see [USAGE.md](USAGE.md) for what each needs). `pifork` runs a fork of Pi and is opt-in: build its worker image from your own
Dockerfile (`make pifork-image`, see
[`doc/designs/pifork-harness.md`](doc/designs/pifork-harness.md)) and check
with `factoryd doctor`
([USAGE_REFERENCE.md § pifork harness](USAGE_REFERENCE.md#pifork-harness-opt-in)).

Full flags and credential/header details: [USAGE.md](USAGE.md#run-a-ticket),
[USAGE_REFERENCE.md](USAGE_REFERENCE.md).

## Sandbox

Docker sandboxing is **unconditional**: there is no host-execution path,
and there is no built-in default image either -- `-sandbox-image`/
`sandbox_image` must be configured (`make install` builds it and every
other image from source and records them via `factoryd configure-images`)
or a run is refused before any container launches or run record is
created.

- Own image: `-sandbox-image=<name@sha256:...>` (always wins).
- Running from inside the target repo (`-workspace .`) needs `-data-dir` outside the workspace, or the data dir gets mounted into the worker.
- Dependencies the worker image lacks (no package-registry network in the sandbox): `make project-sandbox-image PROJECT_DIR=<path> BASE_IMAGE=<worker ref>` (Go, npm, pip manifests; `internal/sandbox/Dockerfile.project`), or `-registry-proxy` for a per-run read-only caching proxy over an npm/PyPI/Go allowlist (`internal/registryproxy`).
- The default `build_app.py` is model-backed, so it needs a model route (`roles.execution`); a run with neither a route nor an explicit offline `-build-app-script` is rejected before any container launch or run record.
- Workers are launched through the OpenShell gateway, which `factoryd doctor -fix` starts together with the meter (`meter_image`, written by `make install`); a launch against a stopped gateway fails with that instruction. `factoryd stop -all` stops both, and refuses while a request or sandbox is active.
- Runs do the pi-harness ticket preflight unless `-skip-project-check`. `-full-suite-cadence N` samples a declared `-full-suite-command` (`0`/`1` = every run).

## Key documents

| Doc | What it's for |
|---|---|
| [`DEMO.md`](DEMO.md) | A 20-30 minute live demo script |
| [`demo/how-buildgate-works.html`](demo/how-buildgate-works.html) | Why, what and how, for an engineering team |
| [`USAGE.md`](USAGE.md) | End-to-end walkthrough: `quickstart`, `submit`/`worker`, oracles, how Temporal runs a request and what happens to a lost step |
| [`USAGE_REFERENCE.md`](USAGE_REFERENCE.md) | Per-command flags, gotchas, harness/credential configs, session-config keys, repository-owner runs and daemons |
| [`console/README.md`](console/README.md) | The operator console |
| [`agent/pi/README.md`](agent/pi/README.md) | The in-sandbox build, drafting and review scripts and their harness adapters |
| [`doc/designs/architecture-flows.md`](doc/designs/architecture-flows.md) | Component diagram, request lifecycle, the request workflow and a run's Activity sequence |
| [`doc/designs/openshell-sandbox-runtime.md`](doc/designs/openshell-sandbox-runtime.md) | The sandbox runtime: gateway, supervisor, meter, one launch step by step, the restart guard |
| [`doc/designs/progress-contract.md`](doc/designs/progress-contract.md) | The run progress feed: file format, stage vocabulary, stall semantics |
| [`safety-contract.md`](safety-contract.md) | Trust boundaries, threats, invariants (SC-001–SC-015) |
| [`CLAIMS.md`](CLAIMS.md) | Every normative claim mapped to status + enforcing test |
| [`containment-matrix.md`](containment-matrix.md) | Sandbox/network/filesystem escape boundaries: enforced vs. open |

## Build & test

```sh
make verify              # fmt-check, vet, go test -race ./... (cmd/factoryd as 4 parallel shards; ~3-4 min)
make verify-live         # DOCKER_SANDBOX_LIVE=1 + Temporal-live tests, no self-skip (needs Docker; minutes)
make ci                  # fmt-check, vet, verify-live, the agent/pi Python suite, flutter test -- the local pre-merge gate
make console-test        # cd console && flutter test
make install             # see Install
make live-smoke          # real gateway/model/git end-to-end fixtures (AGENTS.md § Live validation)
make live-compose        # one real todo-kafka-service ticket with Postgres/Kafka/Redis sidecars
make live-smoke-results  # last 20 recorded live-smoke runs
make live-smoke-test     # offline test of live-smoke.sh's result recording
make proving-ground         # fixture corpus through factoryd, auto-classified (Docker + model); PROVING_GROUND_EXTRA_FIXTURES adds your own
make proving-ground-results # recorded runs grouped by date/SHA, with the confidence bar
make proving-ground-test    # offline tests: classifier, fixtures schema, ticket specs
make bar                 # -draft-oracles default-on bar (hours; human at oracle_review) -- see AGENTS.md
make bar-test            # offline tests for bar's verdict logic
```

- `make verify`'s tests run through `scripts/test-sharded.sh`: one `cmd/factoryd` race test binary, run as 4 processes split by `scripts/factoryd-test-shards.txt`, alongside `go test -race` over every other package. New tests land in the last shard automatically; the run fails if a test is in zero or two shards or the shard file names a missing test.
- `make verify-live` fails loudly without Docker; needs Temporal at `TEMPORAL_ADDRESS` (default `localhost:7233`) or the `temporal` CLI. On colima it roots scratch dirs under `~/buildgate` so the VM can see them.
- `make proving-ground`: fixtures in `scripts/proving-ground/fixtures.json`, each pinned to a `base_ref`, one at a time. `PROVING_GROUND_TEMPORAL=<addr>` also runs each through Temporal as a separate `path: temporal` row.

**CI** (`.github/workflows/ci.yml`) is manual-only by operator decision:
`gh workflow run ci.yml --ref <branch>`. Push-to-`main` and nightly triggers
exist but stay off unless the repository variable `SELF_HOSTED_RUNNER=true`
and a self-hosted runner labelled `self-hosted`,`macOS` is registered
(currently not used). That variable also retargets `verify` to the runner
and enables the nightly `verify-live` job; unset it to revert.

**Optional: graphify.** `.graphifyignore` is committed, `graphify-out/` is
gitignored. `graphify update .` refreshes the code graph (no LLM). Useful
for "what depends on X"; grep is better for locating code.

## Status: what's done vs remaining

Latest release tag: **`m6`**. By convention a tag is cut only after `make live-smoke` passes on that commit, and other machines upgrade by tag, not from `main`.

| Area | State |
|---|---|
| Safety contract, containment, evidence | ✅ Done: `safety-contract.md` SC-001–SC-015 enforced by `CLAIMS.md` tests; no host-execution path |
| Request pipeline (spec, oracles, plan, build, gates, PR) | ✅ Done; every build runs on Temporal |
| Model roles (`planning`/`execution`/`review`), routes, per-role harness | ✅ Done; request choice only within a role's `allowed`/`allowed_harnesses` |
| Operator skills per role | ✅ Done |
| Code review stage, combined conformity + code review call | ✅ Done |
| Cost: per-model prices, rollups by role × model, launch budgets | ✅ Done (`factoryd cost`) |
| Compose service dependencies | ✅ Done |
| Copilot route + Copilot CLI harness, live | 🟡 Waiting on a walk with a paid Copilot seat |
| A run interrupted by sleep, stop, crash or reboot | ✅ `worker` on Temporal: the lost step waits in `resume_review` for `factoryd resume` (a build continues in its kept worktree from the last completed round) or `resume -from scratch` / `cancel`; there is no automatic retry. |
| Merge/deploy | ⬛ **Permanently human-gated by design**: release decisions are recorded, never acted on |

✅ done · 🟡 partial · ⬛ permanently out of scope, by design

## Operations

| Command | What it does |
|---|---|
| `factoryd inbox [-json]` | Every request waiting on you (the four review gates, `halted`, `quarantined`) across all profiles, oldest first, each with the command to act on it and its console link |
| `factoryd cancel [-reason "..."] <request-id>` | Drops a request you no longer want: `cancelled` is terminal, and a `quarantined` or `halted` request can be dismissed this way instead of retried. See [USAGE.md](USAGE.md) for the state graph |
| `factoryd uninstall [-dry-run] [-yes] [-force] [-purge]` | Removes what `make install` put on this machine: stops `worker`/`serve`/Temporal, removes the launchd services, the Temporal containers, the local registry container, the locally built images, the `buildgate` skill and the `factoryd` binary. Lists the plan and asks first (`-yes` skips; required off a terminal; `-dry-run` only prints). Keeps `~/.config/factoryd` and `~/buildgate` (requests, runs, evidence) unless `-purge`, which also deletes the Temporal volumes and the OpenShell gateway's state on the Docker VM, and asks you to type `purge`. Never touches Docker/colima, Go, Flutter, `gh`, Homebrew packages or git config |
| `factoryd stop [-all] [-force]` | Stops the `worker` and `serve` that autostart, `quickstart` or `submit` started for this data dir (`-all`: every profile's, then Temporal). Refuses while a request is building unless `-force`: a forced stop cancels the build (the worker puts the request in `resume_review` at its next start, and `factoryd resume <id>` continues or rebuilds it); leaves launchd-supervised services to `uninstall-service` |
| `factoryd logs [-list] [-f] <id\|queue-run\|serve>` | The log being written now for a request, run, the worker (`queue-run`) or `serve`; `-list` shows every log, `-f` follows |
| `factoryd use [<name>]` | Lists the session-config profiles (`~/.config/factoryd/<name>.yml`) or switches the active one; every command and the `buildgate` skill then follows it. `FACTORYD_PROFILE` overrides per shell; `-config <name>` per command. `make install` re-points every profile at the new images |
| `factoryd upgrade [-to <tag>] [-source <checkout>] [-yes] [-wait]` | One-command upgrade to the newest release tag: plan, drain (refuses while a request builds; `-wait` waits), stop, check out, `make install` (re-points every profile's images), verify, restart what ran, refresh the skill. On failure prints the restore command |
| `factoryd install-service` / `uninstall-service` | macOS: launchd plist (`~/Library/LaunchAgents/dev.factoryd.worker.plist`) keeping `worker -temporal-address <addr>` alive across reboots, logging to `-data-dir/logs/`. Refuses when no Temporal is available. `doctor` reports its state |
| `factoryd supervise -repository ...` | One child daemon per repository; restarts unexpected exits with backoff, replaces stale/mismatched heartbeats |
| `factoryd serve -daemon-temporal-address ...` | Owns those supervisors; `GET /daemons`, `POST /daemons/start`, `POST /daemons/stop`. Without the Temporal setting those routes fail closed |
| `factoryd kill-switch -project <p> -state engaged\|disengaged -by <who> -reason <why>` | Durably engages/disengages a project's release kill switch (omit `-state` to read state + history). Engaged = every release decision fails closed. CLI-only so it works without `serve`; file-locked; no merge/deploy/run side effect |
| `factoryd reconcile -workspace <path> -data-dir <dir>` | Reaps a stranded isolation marker/worktree/branch from a killed run. Refuses if a live run holds the repo lock; preserves anything still resumable |

## Desktop notifications

On macOS, `factoryd` notifies when a run halts, is quarantined, or is
accepted (`FACTORYD_DESKTOP_NOTIFICATIONS=0` opts out).

- Default: a plain `osascript` banner; clicking it does nothing useful.
- With [`terminal-notifier`](https://github.com/julienXX/terminal-notifier) on `PATH` (`brew install terminal-notifier`): clicking opens the run's console page when `FACTORYD_CONSOLE_URL` is set, else the run directory in Finder; the banner adds a "Next: ..." line.

`terminal-notifier` shows nothing until macOS approves it:

1. Fire it once so macOS lists it: `terminal-notifier -title factoryd -message test -execute "open /tmp"`.
2. System Settings → Notifications → `terminal-notifier` (`fr.julienxx.oss.terminal-notifier`): Allow Notifications on, alert style Banners or Alerts (not None), Show in Notification Center on.
3. Re-run step 1; clicking the banner should open `/tmp`.

## License

Apache License 2.0 — see [`LICENSE`](LICENSE). Copyright 2026 Narsimha
Jammula. Vendored and pulled-in dependencies keep their own upstream
licenses.
