# Buildgate live demo script

A 20-30 minute live demo on a small Go repo. Numbered, copy-pasteable,
with what to say at each step. If a command here disagrees with
`factoryd <cmd> -h`, trust the binary. Anything not run end to end for this
doc is marked **unverified**. Background: [`README.md`](README.md),
[`USAGE.md`](USAGE.md), [`console/README.md`](console/README.md).

## Timing expectations

| Stage | Typical | Note |
|---|---|---|
| Spec drafting | 1-2 min | |
| Oracle drafting (`-draft-oracles`) | 4-5 min, more with a redraft | Drafted one criterion at a time, so one slow criterion doesn't sink the rest. **Start it before you start talking**, or use a pre-run request id |
| Planning | 1-3 min | |
| Build + gates | 1-5 min | A single-instance local model host is serialized automatically; a second job shows "queued behind <run>" |

Plan for: Demo 1 about 8-12 min, Demo 2 about 10-15 min, Demo 3 (optional) about 5 min.
Pre-run one full request the day before so you have a request id to fall back on.

## 0. Prerequisites checklist

- [ ] Docker running (`docker info`); `make install` already built the sandbox/meter/registry-proxy images from source and pulled OpenShell's; the gateway and meter are running (`factoryd doctor -fix`).
- [ ] A model route: an OpenAI-compatible `/v1` endpoint (e.g. a local model), `ANTHROPIC_API_KEY`, GitHub Copilot, or ChatGPT via a `codex` login. `quickstart` detects a Codex/Copilot/Anthropic login and offers it. See README's model-route table.
- [ ] `python3`, `git`, `go` on PATH. `gh auth login` only if you want a PR to open (see below).
- [ ] Temporal needs nothing from you: `make install`, `quickstart`, `submit` and `worker` start it with Docker when it is down, and a run halts if it can't start (builds run only on Temporal; [`USAGE.md`](USAGE.md#temporal-what-runs-every-build)).
- [ ] The right binary: `which -a factoryd` and `factoryd version`. A stale build earlier on PATH runs fewer `doctor` checks.
- [ ] `factoryd doctor` prints no failures.
- [ ] Repo, data dir and config under `$HOME`, not `/tmp` (Docker on macOS often doesn't share `/private/tmp`).

```sh
factoryd doctor
```

Say: "doctor checks Docker, that the sandbox image is present locally, that the OpenShell gateway and the meter answer, that the model is reachable from inside the sandbox network, and mount visibility, and prints the fix if not."

## 1. One-time setup on a disposable clone

Use a throwaway repo so the demo can never touch real work. It has no
GitHub remote, so no PR can be pushed. (On a real repo the factory opens a
*draft* PR for an accepted run and never merges.)

A tiny Go repo whose spec can name exact signatures (**unverified** end to end):

```sh
mkdir -p ~/demo && cd ~/demo && rm -rf mathops && mkdir mathops && cd mathops
git init -q && git checkout -q -b main
go mod init example.com/mathops
cat > mathops.go <<'EOF'
package mathops

// Add returns a + b.
func Add(a, b int) int { return a + b }
EOF
cat > mathops_test.go <<'EOF'
package mathops

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatal("Add(2, 3) != 5")
	}
}
EOF
git add -A && git commit -qm "initial"
```

Onboard it (scaffolds `spec/spec.md`, `spec/contract.md`, `ARCHITECTURE.md`
and writes `.factory.yml` with the detected `verify_command` and
`preflight_profile: brownfield`), check it, commit:

```sh
factoryd onboard -project mathops -root ~/demo/mathops -write-factory-yml
cat .factory.yml          # confirm verify_command is: go test ./... (edit if not)
git add -A && git commit -qm "onboard buildgate"
```

Say: "The verify command is the canonical pass/fail. The agent's own claim of success counts for nothing."

## Demo 1 (about 10 min): quickstart, no oracles

Ticket (one sentence, well specified so the spec drafts cleanly):

> Add an exported function `Multiply(a, b int) int` to package `mathops` in `mathops.go`, returning a*b, with a table-driven test.

| # | Do | Say |
|---|---|---|
| 1 | `factoryd quickstart ~/demo/mathops "Add an exported function Multiply(a, b int) int to package mathops in mathops.go, returning a*b, with a table-driven test"` | "One command: doctor checks, session config, starts Temporal, the gateway and meter, the worker and the console, submits the request, and stops at the first human gate." Answer the route/model prompts the first time. Flags go **before** `<repo-path>`. |
| 2 | Note the `-data-dir` in the `factoryd watch -data-dir ...` line it prints and run `DATA=<that path>` in each terminal you'll use. | "The factory is drafting a spec from the request and the repo's own docs." |
| 3 | The console opens in your browser on the request (lost the tab? `factoryd console -open`). | "This is the operator console. Waiting-on-you items sort first. The Pipeline stepper shows where it is and who did what." |
| 4 | Read the spec and its acceptance criteria. Optionally click Edit, change a word, Save. | "This is the human gate. Nothing is built from a spec nobody approved: approval records who and the SHA-256 of the file, and a later edit is refused." |
| 5 | Click Approve (or `factoryd approve -data-dir "$DATA" <request-id>`). Wait 1-3 min for `plan_review`. | "Now it drafts ticket plans, and checks every criterion is claimed by a ticket." |
| 6 | Review the plan (files to touch, steps, tests to add), Approve. | "Second gate. Approving the plan re-checks the spec hash first." |
| 7 | `factoryd watch -data-dir "$DATA" <request-id>` (or the console Runs view) while it builds. | "Sandboxed build: a Docker container created by the OpenShell gateway, no network, model only through its supervisor and our meter. Then verify and the gates." |
| 8 | When done: `factoryd status -data-dir "$DATA"`. Open the run in the console; show gate results and evidence. | "Accepted means the factory's own checks passed against durable evidence: canonical verify, diff scope, tests added, full suite, conformity." |
| 9 | Point out: nothing was merged, and with no remote no PR was opened (the request reads "accepted, awaiting pull request"). | "On a real repo the factory opens a *draft* PR when the release policy allows, and marks it ready for review once checks pass and no review thread blocks it. Merge and deploy are permanently human, by design." |

If the run's cost shows up on a ChatGPT or Copilot route, it's labelled
"API-price est.; billed to your subscription": an estimate, not a bill.

## Demo 2 (about 15 min): the same with `-draft-oracles`

`quickstart` has no oracle flag, so use `submit` (the daemon `quickstart`
left running serves it). **Start this before your intro talk**: drafting
takes several minutes and may need a redraft.

Oracle drafting needs a spec that names the contract (package, function
signatures, routes), or the drafter invents names the builder won't share.
This ticket does:

```sh
factoryd submit -data-dir "$DATA" -draft-oracles -verify-command "go test ./..." -preflight-profile brownfield ~/demo/mathops \
  "Add an exported function Subtract(a, b int) int to package mathops in mathops.go, returning a-b. Add a table-driven test."
factoryd status -data-dir "$DATA"
```

Always pass `-data-dir "$DATA"`: the CLI defaults to `./data`, but the
daemon only drains `$DATA`. Flags go before `<workspace>`. Add `-watch` to
stay attached.

| # | Do | Say |
|---|---|---|
| 1 | Approve the spec (console or `factoryd approve -data-dir "$DATA" <id>`). | "New stage: instead of planning next, a sandboxed drafter writes acceptance tests from the spec's criteria and the repo at the base commit. It never sees the implementation." |
| 2 | Wait for `oracle_review`. If a criterion failed or timed out: `factoryd reject -data-dir "$DATA" -reason "<what to fix>" <id>` and wait again. | "Redrafting is a normal step. That's why a human is in the loop." |
| 3 | Read the drafts: `ls -R "$DATA"/requests/<id>/oracle/`, then `MANIFEST.json` and `RUN_COMMAND.txt`. | "Every criterion maps to a file and a target path. Judgement criteria are skipped; thresholds belong in the verify command." |
| 4 | Compile-check the draft (**unverified**): `git clone ~/demo/mathops ~/demo/check`, copy the test file(s) to the `target_path` in `MANIFEST.json`, then `go vet ./...`. "undefined: Subtract" is expected before the build; a syntax error or wrong package is not. | "Read it like code review: right package, real assertions, expected values correct. The canary and syntax checks are guards, not proof." |
| 5 | Approve (`factoryd approve -data-dir "$DATA" <id>`), then approve the plan. | "Approval hash-pins every oracle file. A later edit is refused." |
| 6 | `factoryd watch -data-dir "$DATA" <id>`. | "The oracle is mounted read-only at `.oracle`, outside the agent's write reach. A runtime canary checks the tests really ran. A failing oracle blocks acceptance and its failures feed the retries." |
| 7 | After acceptance, find the result branch in the run's evidence in the console, then `git -C ~/demo/mathops log --stat -3 <branch>` and look for `.buildgate/oracles.json` (**unverified**). | "The accepted tests and `.buildgate/oracles.json` are committed by the factory host, so they keep guarding the code. Opt out per request with `-no-commit-oracles`." |

Say honestly: oracles are opt-in (Go and Python repos), default-on is still
undecided, and a human reads and approves every drafted test.

## Demo 3 (optional, about 5 min): the gates are real

An oracle asserting a wrong value quarantines the run. The live-smoke
fixture `live-smoke-mathops-oracle-reject` (oracle asserts `3*4 == 13`)
shows it. It uses the math_ops fixture in `testdata/fixtures/`, needs Docker,
and a model route:

```sh
LIVE_SMOKE_ONLY=oracle-reject make live-smoke
```

Expect (3-6 min): the run is quarantined with a failed `reference_oracle`
gate, and neither the oracle nor `.buildgate/oracles.json` reaches the
result commit. Say: "The agent can build a correct implementation and
still be rejected, because the acceptance test itself is wrong. Nothing
advances on the agent's own report."

Variants: `LIVE_SMOKE_ONLY=oracle` runs the accept and reject fixtures.
They run through Temporal; `LIVE_SMOKE_TEMPORAL=<address>` names another server.

## If something goes wrong

Commands that read a request (`status`, `watch`, `approve`, `reject`,
`retry`) need `-data-dir "$DATA"`.

| Symptom | Fix |
|---|---|
| Oracle draft failed or a criterion is missing | The other criteria are kept; the failed one is in the draft diagnostics. At `oracle_review`, `factoryd reject -reason "..." <id>` redrafts. Or show a pre-run request id |
| Halted at planning, over a cap | `factoryd retry <request-id>` (check the state with `factoryd status` first) |
| A materialization halt after oracle approval | `factoryd retry <request-id>` returns it to `oracle_review` |
| No PR opened | Expected with no remote, a release-policy denial, or `worker -open-pull-request=false`. If the run was accepted but the PR didn't open (e.g. `gh` not logged in), fix that and `factoryd retry <id>`: it re-opens only the PR, no rebuild |
| Want to be sure no PR opens | Use the remote-less demo clone, or stop the daemon (`factoryd stop -data-dir "$DATA"`) and restart it with `factoryd worker -open-pull-request=false -config <config-path> -data-dir "$DATA"` |
| "does not appear to share ... into its containers" | Docker on macOS not sharing `/private/tmp`. Put the repo, data dir and config under `$HOME` |
| `doctor` warnings or failures | Read the fix line it prints. Missing images: `factoryd doctor -fix -repo-root <buildgate checkout>` builds them locally |
| Behaviour differs from these docs | Stale `factoryd` on PATH: `which -a factoryd`; use the full path or `make install` |
| Request quarantined or halted | `factoryd inbox` (or `factoryd status`) for the reason and next step; `factoryd logs <id>` for the log behind it; `factoryd retry <id>` for another attempt |
| Build waiting, "queued behind <run>" | Another job holds the local model host. It proceeds when that job's model call finishes |

## Cleanup

```sh
factoryd stop -data-dir "$DATA"   # worker and console for the demo's data dir
factoryd stop -all                  # or: every profile's, then Temporal
rm -rf ~/demo/mathops ~/demo/check
docker ps                           # confirm no leftover worker/supervisor containers
```

Request records under `$DATA/requests/` are runtime state, not source;
delete the data dir only if you no longer need the demo history.
