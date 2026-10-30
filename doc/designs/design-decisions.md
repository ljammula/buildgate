# Design decisions

Why buildgate is shaped the way it is: each entry is one standing decision,
its reason, what it rules out, and what would reopen it. What the pieces are
and how a request moves through them is in
[`architecture-flows.md`](architecture-flows.md). Where
[`safety-contract.md`](../../safety-contract.md) holds the mechanism, the
entry names the invariant and does not restate it.

| Group | Decision |
|---|---|
| What it is built for | [The execution role must work on a small local model](#the-execution-role-must-work-on-a-small-local-model) |
| | [A request is planned first, then split into tickets](#a-request-is-planned-first-then-split-into-tickets) |
| Layering | [Go on the host decides, Python in the sandbox drives one model job](#go-on-the-host-decides-python-in-the-sandbox-drives-one-model-job) |
| | [The build loop runs outside the coding agent](#the-build-loop-runs-outside-the-coding-agent) |
| | [One loop for every harness, one adapter per harness](#one-loop-for-every-harness-one-adapter-per-harness) |
| | [The build's own verify is feedback; the verdict comes from a fresh sandbox](#the-builds-own-verify-is-feedback-the-verdict-comes-from-a-fresh-sandbox) |
| Temporal | [Temporal sequences and remembers; it does not retry](#temporal-sequences-and-remembers-it-does-not-retry) |
| | [A build is one Activity; its rounds are not in workflow history](#a-build-is-one-activity-its-rounds-are-not-in-workflow-history) |
| Recovery | [A lost step waits for a person](#a-lost-step-waits-for-a-person) |
| | [A failed attempt is continued, not rebuilt](#a-failed-attempt-is-continued-not-rebuilt) |
| Review | [A review is a separate session that reads the base commit's instructions](#a-review-is-a-separate-session-that-reads-the-base-commits-instructions) |
| Speed given up for safety | [Every container starts with cold Go caches](#every-container-starts-with-cold-go-caches) |
| | [One job at a time on a single-instance model host](#one-job-at-a-time-on-a-single-instance-model-host) |
| Scope | [Merge and deploy are a person's action](#merge-and-deploy-are-a-persons-action) |
| | [Every build runs in the sandbox](#every-build-runs-in-the-sandbox) |
| | [The MCP endpoint reads and submits, never decides a gate](#the-mcp-endpoint-reads-and-submits-never-decides-a-gate) |
| | [Worker skills are operator snapshots, opt-in](#worker-skills-are-operator-snapshots-opt-in) |
| Operations | [CI is manual](#ci-is-manual) |

```text
factoryd (Go, host)      which job, which sandbox, gates, accept or quarantine
  └─ build script        one build: rounds, verify, review turn, evidence
       └─ harness        one turn: model calls, tool use, file edits
            └─ model
```

## What it is built for

### The execution role must work on a small local model

| | |
|---|---|
| Decision | The build loop is designed so the execution role can be a locally hosted open-weights model with its context window capped at about 128k tokens, on the minimal `pi` harness (the default). A frontier model is used only where a role is configured with one |
| Because | Cost. A build is many turns, and a full-featured harness on a hosted frontier model pays that harness's own prompt and tool overhead on every one of them. A small harness on a local model makes those turns cheap |
| Rules out | A step that works only with a large context window or a frontier model in the execution role. A step that depends on a feature only one vendor's harness has |
| Revisit when | A local model at this context size can no longer pass the live fixture set |

### A request is planned first, then split into tickets

| | |
|---|---|
| Decision | A request becomes a spec, then a plan of tickets, and a person reviews each before any build. Planning and execution are separate roles with their own model, so the plan can come from a stronger model than the one that builds. Each ticket is built, verified, reviewed and delivered as its own pull request |
| Because | A ticket the size of one pull request fits a small model's context for the build, the verify output and the review, and is a size a person can review. A smaller model builds reliably from a precise ticket; designing the change is the part that needs the stronger one |
| Rules out | One build for a whole feature. A build that starts from a goal with no reviewed ticket |
| Revisit when | Never for the split. Ticket size follows the execution model's context window |

## Layering

### Go on the host decides, Python in the sandbox drives one model job

| | |
|---|---|
| Decision | `factoryd` (Go) owns the run state machine, the gates, the sandbox launch and the release decision. The scripts under `agent/pi/scripts/` (Python) run inside the sandbox and drive one model job each: a build, a draft, a review |
| Because | The line between them is the trust boundary: everything in the sandbox runs beside agent-written code and is untrusted, so the side that decides must be outside it. The scripts are vendored in this repository because their output contract (`BUILD_EVIDENCE.json`) must change in the same commit as the Go code that reads it. The scripts are a goal runner complete in themselves (draft, plan, build rounds, checks); `factoryd` is what runs them unattended, in a sandbox, with gates around them |
| Rules out | A gate that takes a script's report as its verdict. A script that reaches the host |
| Revisit when | Never for the boundary. The language is not part of the decision: the scripts could be Go with no change to the design |

### The build loop runs outside the coding agent

| | |
|---|---|
| Decision | `build_app.py` starts the harness, runs the repository's setup and verify commands itself after each turn, writes the next round's prompt from the real failure output, and stops at the round cap and the time budget. The first prompt ends with a fixed definition of done (verify command, file scope, tests, criteria evidence) |
| Because | The default harness is minimal by choice and has no goal runner: it runs a turn and stops. A harness ends its turn when the model decides it is done. An unattended build needs "done" to be a command's exit status, and its bounds (rounds, minutes, saved prompts) to be code, not model behaviour |
| Rules out | The round loop as a harness hook or a skill: a skill is advice the model may skip, and hook configuration sits in files the agent can edit |
| Revisit when | Never while builds are unattended |

### One loop for every harness, one adapter per harness

| | |
|---|---|
| Decision | Every model-bound script takes `--harness <name>` and reaches the coding agent only through `agent/pi/scripts/harness_adapters.py`: how to launch it, how to read its output. Rounds, prompts, review and evidence are the same on every harness |
| Because | A harness is added by one adapter class, one registry entry, one image stage, its recorded fixtures and one `containment-matrix.md` row, with no job-launch, sandbox-runtime or config-schema change. A loop written in each harness's own extension format would be re-implemented per harness and drift |
| Rules out | A script outside the adapters that names a harness. Behaviour that exists on one harness only |
| Revisit when | Only one harness is supported |

### The build's own verify is feedback; the verdict comes from a fresh sandbox

| | |
|---|---|
| Decision | The verify the build script runs between rounds tells the agent what to fix. The result that gates the run is `factoryd`'s own verify of the committed result, in a new sandbox the build never ran in |
| Because | The build's sandbox is writable by the agent: its caches, its environment and its report are all things the agent could have shaped (SC-006, SC-012) |
| Rules out | Skipping canonical verify because the build's last round passed |
| Revisit when | Never |

## Temporal

### Temporal sequences and remembers; it does not retry

| | |
|---|---|
| Decision | Every build and every request runs as a Temporal workflow. Temporal is used for the order of steps, for gates that wait hours or days on a person without holding a process, for knowing which step a crash lost, and for ordering the work that needs a repository to itself. Its automatic retries are off: job Activities run with `MaximumAttempts: 1` |
| Because | See [A lost step waits for a person](#a-lost-step-waits-for-a-person) |
| Rules out | Features that depend on Temporal rerunning a step by itself |
| Revisit when | The lost-step decision changes |

### A build is one Activity; its rounds are not in workflow history

| | |
|---|---|
| Decision | `RunBuildActivity` launches one sandbox and the script runs every round in it. The script records the last completed round and the next prompt in its own state file in the worktree, which `factoryd resume` hands back with `--resume-from-state` |
| Because | The rounds of a build share one container, one harness session and, with Compose services, one set of sidecars, under one heartbeat. A round per Activity would start a sandbox per round (a cold cache and the network's start-up delay each time) and would make every change to the loop a workflow-history change needing a version marker |
| Rules out | Per-round Temporal timeouts and per-round history. Temporal replay as the way to resume a build |
| Revisit when | Builds are often lost mid-way and resuming them by hand is a measured cost (`factoryd stats`, "halted by") |

## Recovery

### A lost step waits for a person

| | |
|---|---|
| Decision | A step lost to a laptop sleep, a lost worker or a stopped `factoryd` halts the run. The request waits in `resume_review` until a person chooses `factoryd resume`, `resume -from scratch` or `cancel` |
| Because | A silent rerun spends the model budget a second time. A container of the lost run may still be alive and writing, and a rerun would race it: `resume` is refused while one is, or when the worktree's history changed |
| Rules out | An Activity retry policy on a job. A worker that reruns a lost build without being asked |
| Revisit when | Lost steps are frequent enough that the wait costs more than an occasional double spend (`factoryd stats`, "halted by") |

### A failed attempt is continued, not rebuilt

| | |
|---|---|
| Decision | A round that fails its checks is followed by another round in the same session and worktree. A run quarantined by checks a build can fix is followed by a corrective build from the failed attempt's commit, given the factory's record of what failed. `factoryd retry` does the same; `-from scratch` is the explicit way to start over |
| Because | The work so far is in the worktree and its commits, and the failure is specific. Rebuilding from the base pays for the whole ticket again to fix one check |
| Rules out | A corrective build for a gate that fails the same way on the base commit: that failure is the repository's, not the build's |
| Revisit when | Corrective builds stop converging (`factoryd stats`, "corrective builds" ran against accepted) |

## Review

### A review is a separate session that reads the base commit's instructions

| | |
|---|---|
| Decision | A model review that gates a run launches as its own step in its own sandbox, with the repository's instruction files mounted read-only as the base commit holds them (SC-019), and is never given the record of an earlier attempt (SC-018). The review role's harness and model are not selectable per request |
| Because | A build can rewrite the files that instruct a reviewer, and a reviewer told what failed before reviews the story and not the diff. The person submitting a request must not be able to pick an easier reviewer |
| Rules out | A reviewer that is told what an earlier attempt failed on. `submit -model review=...` |
| Revisit when | Never without a contract review |

## Speed given up for safety

### Every container starts with cold Go caches

| | |
|---|---|
| Decision | Each worker container gets its own empty Go build and module cache |
| Because | A cache shared across phases lets canonical verify replay entries the build phase wrote, and a worker-writable cache must never feed the verdict (SC-006, SC-012; the Package registry row of [`containment-matrix.md`](../../containment-matrix.md)) |
| Rules out | A cache volume shared between build and verify, or between runs |
| Revisit when | A cache can be populated by a step the worker cannot write to, such as a read-only cache built from the base commit |

### One job at a time on a single-instance model host

| | |
|---|---|
| Decision | A model route whose upstream is a local, private or tailnet host is locked host-wide, keyed by that upstream. Other jobs wait and show `model_host_lock` |
| Because | Such a host serves one request at a time |
| Rules out | Parallel builds on a single local model |
| Revisit when | The route can declare how many requests its host serves at once |

## Scope

### Merge and deploy are a person's action

| | |
|---|---|
| Decision | A release decision is recorded and shown, never acted on (SC-001). What the factory delivers is a pull request that is ready to merge |
| Because | The factory's evidence is that checks passed and a model reviewed the diff. Whether the change should ship is a judgement its gates do not make |
| Rules out | Wiring a `Decision` to a merge or deploy. An MCP or API route that merges |
| Revisit when | Never: this is the design, not a milestone |

### Every build runs in the sandbox

| | |
|---|---|
| Decision | Every coding-agent job is launched by the sandbox gateway from a digest-pinned image, on every entry point. There is one launcher |
| Because | The agent runs model-chosen commands on repository code. One path with no exception is the only containment claim that can be tested ([`containment-matrix.md`](../../containment-matrix.md)) |
| Rules out | A host-execution mode, an "unsandboxed" flag or config key, a second launcher selectable by flag |
| Revisit when | Never |

### The MCP endpoint reads and submits, never decides a gate

| | |
|---|---|
| Decision | The MCP tools read requests and runs and submit a request. None approves, rejects or merges ([`safety-contract.md`](../../safety-contract.md), "MCP endpoint") |
| Because | An MCP caller is a model acting for a person. A gate is where a person, not a model, answers |
| Rules out | A tool for a gate decision |
| Revisit when | Never without a contract review |

### Worker skills are operator snapshots, opt-in

| | |
|---|---|
| Decision | A worker sees only the skills its role names in the session config, copied and hashed host-side and mounted read-only (SC-016). No default config enables one, and no request input names one |
| Because | A skill is instruction text the agent follows. What a build was told must be chosen by the operator, fixed for the job and recorded on the attempt, and a host harness folder holds other work's transcripts and credentials |
| Rules out | Mounting `~/.claude`, `~/.codex`, `~/.copilot` or `~/.pi`. A skill a requester selects. A built-in skill that is on by default |
| Revisit when | Never without a contract review |

## Operations

### CI is manual

| | |
|---|---|
| Decision | `.github/workflows/ci.yml` runs on `workflow_dispatch` only. `make ci` runs the same checks locally, and running it before a merge is on the author |
| Because | Hosted runner minutes are the constraint. The push and nightly triggers are in the workflow and turn on with a registered self-hosted runner (the `SELF_HOSTED_RUNNER` repository variable) |
| Rules out | Treating a green pull request page as evidence the checks ran |
| Revisit when | A self-hosted runner is registered |
