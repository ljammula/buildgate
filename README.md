# buildgate

**Hand a coding agent a ticket. Get back a pull request with the evidence
attached. Keep the merge button.**

Buildgate (binary: `factoryd`) is an unattended agent factory. It drafts a
spec, plans tickets, builds each one inside a sealed sandbox, and refuses to
call anything done until its own gates pass. It never merges.

```text
   YOU                        BUILDGATE                               YOU
   ---                        ---------                               ---
 "Add X to Y" --> draft spec --> plan tickets --> build in a sealed --> draft PR
                      |               |           sandbox, then            |
                 [you approve]   [you approve]    policy gates        [you merge]
```

| An agent on its own | Buildgate |
|---|---|
| Runs on your machine with your network and credentials | Every build runs in a Docker container with **no network interface**, launched by the [OpenShell](https://github.com/NVIDIA/OpenShell) gateway |
| "Done, all tests pass" | Nothing advances on the agent's say-so: each stage is a factory-owned policy decision against durable evidence (test results, diff shape, hashes) |
| Spends until you notice | Model calls leave only through the sandbox's supervisor, under a per-run policy and buildgate's own spend meter |
| Starts coding from a one-line prompt | You review the spec and the plan (and, opt-in, the acceptance tests) before code is written |
| Can push and merge | Merge and deploy are **permanently human**, by standing design: the factory records a decision and may open a PR |

Go orchestration daemon (`factoryd`), a React operator console embedded in
`factoryd serve`, and vendored build scripts under `agent/` (one set for
every harness). [Apache 2.0](LICENSE).

## Install

Source-only: buildgate's Docker images are built on your machine, never
pulled from a registry. OpenShell's three images (gateway, sandbox,
supervisor) are pulled by pinned digest (`make openshell-images`).

### A new Mac, start to finish

About ten minutes from a Mac with only Homebrew, most of it image builds.

```sh
# 1. Install: the one command. It installs the tools this Mac lacks, starts
#    Docker, builds the images, installs factoryd, puts it on PATH, logs gh
#    in, asks which model and coding agent to use, and checks the result.
git clone https://github.com/ljammula/buildgate.git && cd buildgate
make install

# 2. In a new terminal, run your first request (any git checkout under $HOME)
factoryd quickstart ~/code/your-repo "Add X to Y"
```

`quickstart` takes a git checkout from nothing configured to a request
waiting at its first review gate:

- It starts Temporal, the OpenShell gateway and meter, a worker and the console, then prints the exact `factoryd approve` command.
- With a ChatGPT/Codex login (`codex login`) it picks that route and `gpt-5.6-luna` itself. Add `-route chatgpt-codex -harness codex` to run the Codex CLI as the coding agent too.
- `-issue <github issue url>` submits from an issue instead of inline text.
- Keep the repo under `$HOME`: the sandbox gateway sees only your home directory, and the Docker VM does not share `/tmp`.
- `factoryd serve` hosts the console; open the URL its startup log prints.

Each step: [USAGE.md § Quick start](USAGE.md#quick-start-existing-repo). A
20-30 minute live walkthrough with talking points (with and without
oracles, plus a failure case): [`DEMO.md`](DEMO.md).

### What `make install` does

Needs Homebrew, or the tools below already installed. Without Homebrew it
names what is missing and stops; a missing `npm` alone only costs the console.

| Step | Detail |
|---|---|
| Tools | `brew install` for what is not on `PATH`: `go`, `python`, `gh`, `git`, `node` (the console), `docker`; `docker-buildx` when `docker buildx` does not work, linked into `~/.docker/cli-plugins`; `colima` only on a machine with no `docker` at all. A tool already on `PATH` is left alone |
| Docker | When Docker does not answer and `colima` is on `PATH`: `colima start` for an existing VM, else `colima start --memory 4`. Any other Docker is yours to start. `FACTORYD_AUTOSTART=0` turns it off |
| Binary | `go install ./cmd/factoryd` into `$(go env GOPATH)/bin` (usually `~/go/bin`); prints the `export PATH=...` line if that is not on `PATH`, and warns if an older `factoryd` earlier on `PATH` shadows it |
| Images | Requires Docker, starts Temporal, builds the sandbox, meter and registry-proxy images from source (`internal/sandbox/Dockerfile` and friends), pulls OpenShell's three by digest. An image whose inputs haven't changed is not rebuilt (it is still pushed, so the recorded digest is unchanged); `FORCE_IMAGE_BUILD=1` always rebuilds |
| Config | Records buildgate's image refs in the default session config via `factoryd configure-images`. `FACTORYD_CONFIG="<config> <config>..."` re-points each listed config instead, so a second profile does not keep pinning a superseded image |
| Console | Baked in if `npm` is on `PATH` and its build succeeds, else a placeholder page and, for a failed build, a warning naming the Node and npm versions ([`console/README.md`](console/README.md)) |
| Agent skill | Installs the `buildgate` skill into `~/.agents/skills` (Copilot, Codex); refreshes `~/.claude/skills/buildgate` when it exists |
| `PATH` | Links `~/.local/bin/factoryd` to the installed binary, so shells that never read a profile (an agent, a script, an ssh command) find it wherever `~/.local/bin` is on `PATH`; a `factoryd` already there that is not this link is left alone. When `factoryd` still does not resolve: appends the `export PATH=...` line to `~/.zshrc` (`~/.bash_profile` for bash), once; a new terminal picks it up |
| GitHub | `gh auth login` when `gh` is not logged in and this is a terminal |
| Model | `factoryd setup`: asks which model (detected ChatGPT/Codex and Copilot logins first) and, on a route that can run more than one, which coding agent; everything else is defaulted. Asks nothing when the config already names a model, or with no terminal (a single detected login is then used) |
| Check | `factoryd doctor -fix`, which starts Temporal, the OpenShell gateway and the meter. Its failures do not fail the install |
| Closing list | Ends with `buildgate is installed.` and a numbered `Left for you:` list of whatever a step could not finish (open a new terminal, `gh auth login`, `factoryd setup`, failed checks), then the `quickstart` command |
| Tree | Leaves `git status` clean |

- **TLS interception:** `make install` detects a proxy that re-signs HTTPS and builds with its CA from the keychain; `BUILD_CA_BUNDLE=/path/to/ca.pem make install` overrides it ([USAGE.md § Quick start](USAGE.md#quick-start-existing-repo)).
- **Model prices** live in `internal/prices/prices.yml`; edit it and run `make install` when a provider changes prices. A model with no entry costs $0 ([USAGE_REFERENCE.md § Model prices](USAGE_REFERENCE.md#model-prices)).

### Drive it from a coding agent

The `buildgate` skill tells an agent which commands it may run, how to
submit and follow a request, and which gates stay yours. `make install`
installs it for Copilot and Codex; for Claude Code, run once:

```sh
factoryd install-skill -dir ~/.claude/skills
```

Then say "use buildgate to implement `x_spec.md` in `~/code/<repo>`". The
agent submits it (an existing repo without Buildgate's spec docs runs as
brownfield), follows it, and hands you the console and Temporal links;
every approval and the merge stay yours. `factoryd upgrade` refreshes the
skill. [USAGE.md § Drive Buildgate from a coding agent](USAGE.md#drive-buildgate-from-a-coding-agent).

Any MCP client (Claude Code, Hermes, your own agent) can do the same over
`serve`'s MCP endpoint: `factoryd mcp` turns it on and prints the endpoint
and token. Its tools read requests and runs and submit a request; none
approves, rejects or merges.
[USAGE.md § Drive Buildgate from an MCP client](USAGE.md#drive-buildgate-from-an-mcp-client).

### Troubleshooting a fresh Mac

| Symptom | Cause | Fix |
|---|---|---|
| `make install`: `Docker is required to build the ... images` | No Docker daemon is running and it is not colima's to start (Docker Desktop, another VM), or `FACTORYD_AUTOSTART=0` | Start your Docker, then `make install` again |
| `make install`: `needs these, and found no Homebrew` | Tools are missing and there is no `brew` to install them with | Install Homebrew or the named tools, then `make install` again |
| `make install`: `the --mount option requires BuildKit` or `docker buildx` missing | The `buildx` plugin is missing and Homebrew is not there to install it | Install your platform's buildx plugin; `factoryd doctor` warns about it |
| `factoryd: command not found` after install | The terminal predates the install, or the shell is neither zsh nor bash | Open a new terminal; otherwise add the `export PATH=...` line `make install` printed to your shell profile |
| An agent or script cannot find `factoryd`, a terminal can | Its shell reads no profile and `~/.local/bin` is not on its `PATH` | Put `~/.local/bin` on `PATH` where every shell reads it (`~/.zshenv` for zsh), or call `~/.local/bin/factoryd` by path |
| `doctor` warns the Docker VM shares all of `$HOME` | colima's default mounts | USAGE.md, "Data directory and colima" |
| `doctor` fails `roles.execution is not configured` | No model was chosen: the install had no terminal and found no single login | `factoryd setup` (`-route chatgpt-codex` for a Codex login) |
| Any command fails `parse .../config.yml: relay_image: deleted with the inference relay` | A `config.yml` written for the retired inference relay still has `relay_image` or `relay_upstream_timeout` | Delete that line; the error names the retired key it found |
| `quickstart` fails `the repository ... is outside your home directory`, or `mount visibility (repository reachable inside a container)` | The gateway sees only your home directory, and the Docker VM must also share the repository's path (`/tmp` is neither) | Clone or move the repo under `$HOME`; if it still fails, add it to your VM's mounts (USAGE.md, "Data directory and colima") |
| A request halts "the meter does not answer" / "sandbox runtime ... run `factoryd doctor -fix`" | The OpenShell gateway/meter is down (reboot, colima restart) or the config lost `meter_image` | `factoryd doctor -fix`; `worker` starts them too unless `FACTORYD_AUTOSTART=0` |
| A request halts `model route error: Connection error.` on a network that re-signs TLS | The gateway was started before the proxy's CA was recorded, so the sandbox's supervisor refuses the model upstream | `factoryd doctor -fix`, then `factoryd retry <id>` (USAGE.md, Troubleshooting) |
| Stop what is running / drop a request | | `factoryd stop`; `factoryd cancel <request-id>` |

### Uninstall

| Command | Removes | Keeps |
|---|---|---|
| `factoryd uninstall -dry-run` | Nothing; prints the plan | Everything |
| `factoryd uninstall` | What `make install` put on the machine, the `~/.local/bin/factoryd` link included | `~/.config/factoryd`, `~/buildgate`, the `PATH` line in your shell profile, tools Homebrew installed |
| `factoryd uninstall -purge` | Also both of those, the Temporal volumes and the OpenShell gateway's state on the Docker VM (database, keys, stored credentials); you type `purge` to confirm | Docker/colima, Go, Node, `gh`, Homebrew packages, git config |

Back up `~/.config/factoryd` and `~/buildgate` first if you want your
requests and evidence. To start over: `-purge`, then `make install`.

## How a request flows

A request (`factoryd submit` / `quickstart`) moves through these states.
`[brackets]` are the human gates; everything else is the factory.

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

Inside `building`, one ticket at a time:

```text
  ticket + spec
       |
       v
  baseline verify: the verify command on the untouched base commit,
  in a fresh sandbox, no model call
       |
       +-- fails, and the failure is not the ticket's own work --> halted
       |
       v
  agent builds in the sandbox ----- worker lost (sleep, crash, stop) ----+
       |                                                                 |
       v                                                                 v
  gates, all factory-owned:                              run halts, worktree is kept,
    canonical_verify        full_suite_verify            request waits in resume_review:
    diff_scope              spec_conformity                factoryd resume <id>
    required_files_changed  code_review       (policy)     factoryd resume -from scratch <id>
    tests_added             reference_oracle  (oracles,    factoryd cancel <id>
                                               + canary)
       |              |
     pass            fail
       |              |
       v              v
   accepted      quarantined
       |
       v
  release policy decision (recorded only)
       |
       v
  draft PR --> human merges (permanently outside this system)
```

| Topic | Behaviour |
|---|---|
| Execution | One path, live-proven: Temporal, for every command (`worker`, the single-ticket run, `live-smoke`), started with Docker when down. A run whose Temporal is unreachable halts |
| Lost steps | A lost build keeps its worktree for `factoryd resume`; nothing reruns a lost step on its own |
| Baseline verify | Before a build spends a model call, every ticket's verify command runs on the commit the ticket starts from. Passed: the build runs, unless the command left files outside the ticket's `Allowed-Files:` that the repository does not ignore (every build would be quarantined by `diff_scope`): then the run halts. Failed for a reason that is the ticket's own work (it names every failing test, or the command needs a path the ticket creates): the build runs and is told. Failed any other way: the run halts with the failing test named. [USAGE.md § Baseline verify](USAGE.md#baseline-verify-the-verify-command-runs-before-the-build) |
| `code_review` | `-code-review-policy off\|advisory\|required`; CLI default `off`, `quickstart` writes `required` for new configs |
| The PR | Title from the ticket's `## Goal`; branch `factoryd/<slug>-<shorthash>`; opens as a draft, and `factoryd` marks it ready (`gh pr ready`) once checks pass and no reviewer thread blocks it |
| Ready to merge | The deliverable. Checked on every poll: out of draft, checks pass, no review thread open, head is the commit the factory built, and that build's code review of the whole diff passed. `factoryd status` and the request page say so, or name what is missing: [STATUS.md § Ready to merge](STATUS.md#ready-to-merge). Merging is yours |
| `factoryd retry` | Re-runs a halted or quarantined request. On an accepted ticket whose PR never opened, it re-opens only the PR, pinned to the accepted commit (`ResultSHA`), after re-running the release decision; a denial falls back to a full rebuild of that ticket only |
| Following a run | `factoryd watch <run-or-request-id>`, the console's Timeline and Pipeline views, or the Temporal Web UI: [USAGE.md § Observe and control](USAGE.md#observe-and-control) |

Exact state set and invariants:
[`safety-contract.md` § State transitions](safety-contract.md#state-transitions).

## Sandbox

```text
  factoryd worker (Temporal RunWorkflow)
        |
        |  launch through the OpenShell gateway, images pinned by digest
        v
  +------------- worker container: no network interface -------------+
  |   coding agent (pi | pifork | codex | copilot)  +  repo worktree  |
  +--------------------------------+----------------------------------+
                                   |  model calls only
                                   v
                    supervisor (per-run policy) ----> meter (ceilings, usage)
                                   |
                                   v
                              model route
```

Docker sandboxing is **unconditional**: there is no host-execution path and
no built-in default image. `-sandbox-image`/`sandbox_image` must be
configured (`make install` does it) or a run is refused before any
container launches or run record is created.

| Need | How |
|---|---|
| Your own image | `-sandbox-image=<name@sha256:...>` (always wins) |
| Run from inside the target repo (`-workspace .`) | Put `-data-dir` outside the workspace, or the data dir gets mounted into the worker |
| Dependencies the worker image lacks (the sandbox has no package-registry network) | `make project-sandbox-image PROJECT_DIR=<path> BASE_IMAGE=<worker ref>` (Go, npm, pip manifests; `internal/sandbox/Dockerfile.project`), or `-registry-proxy` for a per-run read-only caching proxy over an npm/PyPI/Go allowlist (`internal/registryproxy`) |
| A Go or Python version the worker image lacks (the sandbox downloads no toolchain) | Nothing: a build reads what the repository declares (`go.mod`, `.python-version`, `pyproject.toml`) and derives an image with that version on first use. [USAGE_REFERENCE.md § Project toolchains](USAGE_REFERENCE.md#project-toolchains) |
| A model route | The default `build_app.py` is model-backed and needs `roles.execution`; a run with neither a route nor an explicit offline `-build-app-script` is rejected before any container launch or run record |
| Gateway and meter | `factoryd doctor -fix` starts both (`meter_image`, written by `make install`); a launch against a stopped gateway fails with that instruction. `factoryd stop -all` stops both, and refuses while a request or sandbox is active |
| Preflight and full suite | Runs do the pi-harness ticket preflight unless `-skip-project-check`. `-full-suite-cadence N` samples a declared `-full-suite-command` (`0`/`1` = every run) |

Every boundary, enforced vs. open: [`containment-matrix.md`](containment-matrix.md).
The runtime step by step: [`doc/designs/openshell-sandbox-runtime.md`](doc/designs/openshell-sandbox-runtime.md).

## Model routes

Three roles (`planning`, `execution`, `review`), each bound to a route, a
model and a harness. Pi is the default harness; `codex` and `copilot` are
available per role; `pifork` (a fork of Pi) is opt-in and needs its own
worker image (`make pifork-image`,
[`doc/designs/pifork-harness.md`](doc/designs/pifork-harness.md)).

| Route | `credential_mode` | Credential (host-side only) | Notes |
|---|---|---|---|
| OpenAI-compatible endpoint (local ai-stack, vLLM, Ollama, OpenAI, any gateway) | `static` | None, or an `Authorization` header | `upstream: <URL>`, `allowed_path_prefix: /v1` (must match `worker_base_path`); model `id` from `GET <URL>/v1/models`; `api: openai-responses` for a Responses-only model |
| Anthropic | `static` | `ANTHROPIC_API_KEY` | Placeholder, not live-tested. Its OpenAI-compatible endpoint, not the native Messages API |
| GitHub Copilot (Pi, a Pi fork or the Copilot CLI harness) | `github-copilot` | The route's `github_token_file`, then `GITHUB_COPILOT_TOKEN`, then `~/.pi/agent/auth.json` | One entitled model `id`. A model that can't run under the configured worker API is refused with the reason |
| ChatGPT (Codex OAuth) | `chatgpt-codex` | `~/.codex/auth.json`, read fresh per launch, never refreshed or written | Model `gpt-5.6-luna`. A launch whose token expires before the step's time budget ends is refused (fix: run any `codex` command). Upstream and paths are pinned |

`quickstart` detects an existing Codex login, Pi Copilot login or `ANTHROPIC_API_KEY` and offers it first (`-route` picks one); `factoryd doctor -list-models` lists what a route serves. Config stores file paths, never token values.

Full detail: [USAGE_REFERENCE.md § Model routes](USAGE_REFERENCE.md#model-routes),
[§ pifork harness](USAGE_REFERENCE.md#pifork-harness-opt-in),
[USAGE.md § Run a ticket](USAGE.md#run-a-ticket).

## Status

Latest release tag: **`m10`**, cut only after `make live-smoke` passes on
that commit. Full table and every known limit: [`STATUS.md`](STATUS.md).

| | Area |
|---|---|
| ✅ Works, live-proven | The request pipeline end to end (spec review, plan review, sandboxed build, gates, PR) on several real repos; every build on Temporal; the OpenShell sandbox runtime; `quickstart` onboarding; the operator console; `pi` and `codex` harnesses on every role; compose sidecars (Postgres, Kafka, Redis) |
| ✅ Works | Cost rollups by role × model and launch budgets (`factoryd cost`); resume of an interrupted build from its kept worktree; a failed attempt tells the next one (a corrective build after a check failure, `retry` continuing from the failed commit, the build agent's notes, a refused draft's reason); reviews read the instruction files the request started with, not the build's (SC-019); `.factory.yml` `setup:` and `autofix:`; gate scripts under `.factory/` mounted read-only from the commit the config was read from |
| 🟡 Opt-in or partial | Acceptance oracles (`submit -draft-oracles`, Go and Python); worker skills; team design guide; AI code review (`off` on the CLI, `required` from `quickstart`); the GitHub Copilot route and Copilot CLI harness (proven on one small ticket, free plan); CI is manual (`make ci`) |
| ⬛ Never, by design | Automatic merge or deploy; a host-execution path |

## Known limits

The ones most likely to surprise you ([all of them](STATUS.md#known-limits)):

- **A lost step is never retried automatically.** Sleep, crash or stop halts the run and keeps the work; you choose `factoryd resume`, `resume -from scratch` or `cancel`.
- **The repository must be under `$HOME`.** The sandbox gateway sees nothing else.
- **Your own worker image needs a `make` built without `posix_spawn`**; build it on the image `make install` produces (`make project-sandbox-image`). `su` and `sudo` fail in the sandbox too.
- **A gate script is protected only under `.factory/`.** That directory is mounted read-only into every build, verify and gate sandbox as the commit `.factory.yml` was read from holds it; a script there that calls files outside it runs the build's copy of them, as `verify_command: make test` does.
- **Go builds compile cold.** Every worker container starts with an empty Go cache; on a large repo this is the main wall-clock cost.
- **OpenShell 0.1.2 is young**: residual gaps are listed in [`containment-matrix.md`](containment-matrix.md).
- **A baseline failure is matched to the ticket by name.** Failing tests are read from `go test` and `pytest` output only; a verify command that fails in another runner's format halts the run even when the ticket expects the failure. A failure the log does not name as a test (a panic in `TestMain`, a second failing package behind a named one) is not seen, so such a build can still be spent on a command it cannot pass. Every build also pays for one more run of the verify command.
- **Narrow live coverage.** Multi-module `go.work`, cgo and generated code are unproven. Private Go modules are proven on one fixture (`make live-private-module`), with a file `GOPROXY` standing in for a company's.

## Operations

| To | Run |
|---|---|
| See everything waiting on you, across profiles | `factoryd inbox` |
| Follow a run, read its log | `factoryd watch <id>`, `factoryd logs [-f] <id\|queue-run\|serve>` |
| Drop a request | `factoryd cancel <request-id>` |
| Stop what factoryd started | `factoryd stop [-all] [-force]` |
| Restart the worker and console with the installed binary | `factoryd restart` (`make install` does it; refused while a request is building) |
| Switch session-config profile | `factoryd use [<name>]` |
| Upgrade to the newest release tag | `factoryd upgrade` |
| Keep the worker and console alive across reboots (macOS) | `factoryd install-service` |
| Fail closed every release for a project | `factoryd kill-switch -project <p> -state engaged -by <who> -reason <why>` |
| Clean up after a killed run | `factoryd reconcile -workspace <path> -data-dir <dir>` |

Every command and flag: [USAGE_REFERENCE.md § Command reference](USAGE_REFERENCE.md#command-reference).
Watching, costs, budgets and desktop notifications:
[USAGE.md § Observe and control](USAGE.md#observe-and-control).

## Build & test

```sh
make verify              # fmt-check, vet, go test -race ./... (cmd/factoryd as parallel shards; ~3-4 min)
make verify-live         # DOCKER_SANDBOX_LIVE=1 + Temporal-live tests, no self-skip (needs Docker; minutes)
make ci                  # fmt-check, vet, verify-live, the agent/pi Python suite, the console checks -- the local pre-merge gate
make console-test        # cd console && npm ci && npm run check
make install             # see Install
make live-smoke          # real gateway/model/git end-to-end fixtures (AGENTS.md § Live validation)
make live-compose        # one real todo-kafka-service ticket with Postgres/Kafka/Redis sidecars
make live-smoke-results  # last 20 recorded live-smoke runs
make live-smoke-test     # offline test of live-smoke.sh's result recording
make proving-ground         # fixture corpus through factoryd, auto-classified (Docker + model); PROVING_GROUND_EXTRA_FIXTURES adds your own
make proving-ground-results # recorded runs grouped by date/SHA, with the confidence bar
make proving-ground-test    # offline tests: classifier, fixtures schema, ticket specs
make baseline            # one-shot acceptance, rounds to green, repeated failures, what quarantines runs, from recorded runs (BASELINE_DIRS)
make baseline-test       # offline tests for the baseline script
make bar                 # -draft-oracles default-on bar (hours; human at oracle_review) -- see AGENTS.md
make bar-test            # offline tests for bar's verdict logic
```

Sharding, live-test and proving-ground notes: [AGENTS.md § Build, test, verify](AGENTS.md#build-test-verify).

- **CI** (`.github/workflows/ci.yml`) is manual-only by operator decision: `gh workflow run ci.yml --ref <branch>`. Push-to-`main` and nightly triggers exist but stay off unless the repository variable `SELF_HOSTED_RUNNER=true` and a self-hosted runner labelled `self-hosted`,`macOS` is registered (currently not used). That variable also retargets `verify` to the runner and enables the nightly `verify-live` job; unset it to revert.
- **Optional: graphify.** `.graphifyignore` is committed, `graphify-out/` is gitignored. `graphify update .` refreshes the code graph (no LLM). Useful for "what depends on X"; grep is better for locating code.

## Key documents

| Doc | What it's for |
|---|---|
| [`DEMO.md`](DEMO.md) | A 20-30 minute live demo script |
| [`demo/how-buildgate-works.html`](demo/how-buildgate-works.html) | Why, what and how, for an engineering team |
| [`demo/console-tour.mp4`](demo/console-tour.mp4) | A captioned recording of the operator console (1 min 55 s): board, triage, spec and plan review, editing, a build, and what a stopped request shows |
| [`STATUS.md`](STATUS.md) | What works, what is opt-in, every known limit |
| [`USAGE.md`](USAGE.md) | End-to-end walkthrough: `quickstart`, `submit`/`worker`, oracles, how Temporal runs a request and what happens to a lost step |
| [`USAGE_REFERENCE.md`](USAGE_REFERENCE.md) | Per-command flags, gotchas, harness/credential configs, session-config keys, repository-owner runs and daemons |
| [`console/README.md`](console/README.md) | The operator console |
| [`agent/pi/README.md`](agent/pi/README.md) | The in-sandbox build, drafting and review scripts and their harness adapters |
| [`doc/designs/architecture-flows.md`](doc/designs/architecture-flows.md) | Component diagram, request lifecycle, the request workflow and a run's Activity sequence |
| [`doc/designs/openshell-sandbox-runtime.md`](doc/designs/openshell-sandbox-runtime.md) | The sandbox runtime: gateway, supervisor, meter, one launch step by step, the restart guard |
| [`doc/designs/progress-contract.md`](doc/designs/progress-contract.md) | The run progress feed: file format, stage vocabulary, stall semantics |
| [`safety-contract.md`](safety-contract.md) | Trust boundaries, threats, invariants (SC-001–SC-019) |
| [`CLAIMS.md`](CLAIMS.md) | Every normative claim mapped to status + enforcing test |
| [`containment-matrix.md`](containment-matrix.md) | Sandbox/network/filesystem escape boundaries: enforced vs. open |

## License

Apache License 2.0 — see [`LICENSE`](LICENSE). Copyright 2026 Laxmi
Narsimha Reddy Jammula. Vendored and pulled-in dependencies keep their own upstream
licenses.
