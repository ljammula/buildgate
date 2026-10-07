# Using buildgate

A practical walkthrough of building an app end to end with `factoryd`, from
a rough idea to an accepted, verified run. See
[`doc/designs/architecture-flows.md`](doc/designs/architecture-flows.md)
for the architecture and rationale; this doc is just the commands. Flag
tables, session-config keys, model-route setup, harness configs, and
internal mechanics live in [`USAGE_REFERENCE.md`](USAGE_REFERENCE.md) —
this file links to it instead of repeating it.

## Start here

The shortest path from a fresh install to a reviewable pull request:

| # | Command | What it does |
|---|---|---|
| 1 | `factoryd doctor` | Checks Docker, images, model route, mounts; prints the fix for anything wrong |
| 2 | `factoryd quickstart <repo-path> "<task>"` | Configures the session, starts a `worker` and `factoryd serve`, submits the request, opens its tokenized console link, stops at `spec_review` |
| 3 | `factoryd approve <req-id>` / `factoryd reject -reason "..." <req-id>` | Your decision at each review gate (console: Approve / Request changes). Quickstart prints each command with the `-data-dir` it needs |
| 4 | `factoryd watch <req-id>` / `factoryd status` | Follow progress; `factoryd retry <req-id>` after a halt or quarantine |
| 5 | The pull request on the repo's GitHub remote | Opens as a draft, marked ready for review automatically once checks pass and no reviewer thread blocks it. The factory never merges; you do |

**Which document do I read?**

| Document | Read it for |
|---|---|
| [`README.md`](README.md) | What buildgate is, install, architecture |
| [`STATUS.md`](STATUS.md) | What works, what is opt-in, every known limit |
| [`DEMO.md`](DEMO.md) | A scripted walkthrough for showing it to someone |
| `USAGE.md` (this file) | Commands, request lifecycle, config, troubleshooting |
| [`USAGE_REFERENCE.md`](USAGE_REFERENCE.md) | Per-command flags, session-config keys, model routes, `.factory.yml`, oracle/request-driver internals |
| [`console/README.md`](console/README.md) | The operator console |

`factoryd quickstart <repo-path> "<task>"` is the whole path for an
existing repo checkout — see "Quick start" below. For a brand-new app
with no code yet, see "New app from scratch" further down.

## Request lifecycle

`factoryd submit` (or `quickstart`) creates a **request**. It moves
through these states; the factory does the work in "factory" rows, you
act in "you" rows.

| State | Who acts | What happens / what you do |
|---|---|---|
| `submitted` -> `spec_drafting` | factory | A sandboxed job drafts `spec.md`; with `submit -spec-file`, your own spec is taken as `spec.md` with no model call ([reference](USAGE_REFERENCE.md#handing-over-a-finished-spec-and-plan-submit--spec-file--plan-dir)) |
| `spec_review` | **you** | `approve` (to `planning`, or `oracle_drafting` if submitted with `-draft-oracles`) or `reject -reason "..."` (back to `spec_drafting`) |
| `oracle_drafting` | factory | Only with `-draft-oracles`: drafts executable acceptance tests from the spec |
| `oracle_review` | **you** | Read every oracle file, then `approve` (to `planning`) or `reject -reason` (back to `oracle_drafting`) |
| `planning` | factory | Drafts one or more tickets from the approved spec; with `submit -plan-dir`, your own tickets are taken with no model call and get the same checks |
| `plan_review` | **you** | `approve` (to `building`) or `reject -reason` (back to `planning`) |
| `building` | factory | One ticket at a time in the Docker sandbox, verified, gated, PR opened when release policy allows. A quarantine whose failed gates are only `spec_conformity` and/or `code_review` first gets up to `-review-corrective-rounds` (default `1`) automatic corrective builds on the same branch before quarantining. A review that gave no verdict (it timed out or returned nothing; the combined review has 20 minutes) gets no corrective build: the request quarantines as "the review gave no verdict", and `factoryd retry <id>` builds and reviews the ticket again |
| `pr_review` | **you** (on GitHub) | Trusted reviewer comments trigger corrective rounds; PR auto-marked ready once checks pass and no thread blocks it; merging is yours. Next ticket starts once this one is accepted (or, under `-advance-on pr_approved`, once reviewer-approved) |
| `resume_review` | **you** | The `worker` running a step (drafting, planning, a build, a PR-review round) stopped, and nothing reruns on its own. `factoryd resume <id>` continues a lost build in its kept worktree from the last completed round (or reruns a lost drafting/planning step), `factoryd resume -from scratch <id>` rebuilds the ticket, `factoryd cancel <id>` drops it (console: Resume / Rebuild from scratch / Cancel on the request page). `resume` is refused, naming the other two, when a container of the lost run is alive or the worktree's history changed. The request returns to the state of the lost step |
| `done` | - | Every ticket's PR merged |
| `quarantined` / `halted` | **you** | Read the reason in `factoryd status`/console, fix the cause, `factoryd retry <id>` -- or, before any ticket is accepted, send it back to redraft the plan or the spec: `factoryd reject -to plan\|spec -reason "..." <id>` (`plan`/`planning` and `spec`/`spec_drafting`) -- or, to drop the request instead, `factoryd cancel <id>` |

`cancelled` is also terminal (`factoryd cancel <id>`). Full mechanics
(reminders, ticket sequencing, PR review loop): [USAGE_REFERENCE.md § Request lifecycle](USAGE_REFERENCE.md#request-lifecycle).

```text
(<-> : forward on approve, back to drafting on request changes)

submitted -> spec_drafting <-> spec_review ------+
                                 |                |
                   -draft-oracles (opt-in)        |
                                 v                |
            oracle_drafting <-> oracle_review      |
                                 |                |
                                 +---> planning <-+
                                          <->
                                      plan_review
                                          |
                                          v
                                      building -> pr_review
                                          ^           |  \
                                          | next ticket   done
                                          +-(advance_on: pr_approved)

any active stage -> quarantined | halted | cancelled
quarantined | halted -> cancelled (dismiss instead of retrying)
retry -> building | pr_review (PR-open only, accepted w/ no PR)
       | the drafting/review stage it stopped in
quarantined | halted -> planning | spec_drafting
       (`reject -to plan|spec`, before any ticket is accepted --
        every ticket, and for spec every stage, must be re-approved)
```

**Multi-ticket requests open stacked draft PRs.** Ticket N (N>1)'s PR
opens against ticket N-1's own branch instead of the repo default branch,
so it shows only its own delta rather than repeating ticket N-1's
already-open commits; it stays draft until ticket N-1 merges, at which
point it's automatically retargeted onto ticket N-1's own base and can be
marked ready. Review each PR while it is stacked, and merge bottom-up
(ticket 1 first). With squash merges, a retargeted PR's Files tab also
shows ticket N-1's changes (it shares ticket N-1's original commit), but
merging it lands only ticket N's own change.

## The 5 flags that matter

Everything else has a working default. These 5 don't:

| Flag | What it's for | Where it belongs |
|---|---|---|
| `-verify-command` | Canonical pass/fail check for a run | `.factory.yml`'s `verify_command` — commit it once per repo |
| `routes:`/`models:`/`roles:` | The model route; nothing builds without it | Session config, one value for the whole machine — [USAGE_REFERENCE.md § Model routes](USAGE_REFERENCE.md#model-routes) |
| `-spec` (+ `-ticket`) | Which ticket to build — **`-spec`'s content, not `-ticket-file`'s, is what `build_app.py` builds from** | Stays per-invocation (`submit`/`worker` thread it for you) |
| `-data-dir` | Where run/queue state lives; wrong value silently halts on a colima mount-visibility check | Session config's `data_dir` — set once |
| `-preflight-profile` | Strict default refuses a run on a repo missing `spec/spec.md`/`spec/contract.md`/`ARCHITECTURE.md` | `.factory.yml`'s `preflight_profile: brownfield` — `factoryd onboard -write-factory-yml` writes it |

`factoryd quickstart` prompts for the model-route/data-dir pieces and
defaults `-preflight-profile` to `brownfield` for you. To pick a
different model per request (once the route above is set up), see
"Per-request model choice" below.

## Quick start (existing repo)

```sh
factoryd quickstart ~/code/your-repo "Add X to Y"
factoryd quickstart -issue https://github.com/you/repo/issues/42 ~/code/your-repo
```

`<repo-path>` must already be a git checkout (`.` if omitted). Nothing
needs starting by hand: `quickstart` (like `submit`) starts Temporal,
a `worker` and `serve` when they are missing, and prints the console link.
One command sequences everything below, prompting only where a real decision
is needed, and stops at the first human gate (`spec_review`) with the
exact `factoryd approve` command printed. `-issue <url>` (fetches the
request text from that GitHub issue, `gh` must be logged in) and
`-request-file <path>` are the same mutually-exclusive alternatives to
trailing request text `factoryd submit` takes; each must precede
`<repo-path>` on the command line.

**Prerequisites**: with Homebrew, `make install` installs the tools below
that are missing, starts colima and runs `gh auth login`. Go, Docker (colima: `colima
start --memory 4`, then narrow its mounts — "Data directory and colima" below), `python3`, `git`, `gh` logged in
(`gh auth login` — `quickstart`/`doctor` preflight this: installed, and
logged in for the repo's remote host, checked in the environment spawned
processes inherit; an SSH `Host` alias maps to its real host, an
unrecognized host gets an advisory only). Node 20+ and npm are optional, only for
building the console. A model route (any OpenAI-compatible endpoint, an
Anthropic key, GitHub Copilot, or a Codex login —
[USAGE_REFERENCE.md § Model routes](USAGE_REFERENCE.md#model-routes)).

Install, source-only (every image factoryd launches is built from source,
never pulled from a registry):

```sh
git clone https://github.com/ljammula/buildgate.git
cd buildgate
make install
```

`make install` -> `$(go env GOPATH)/bin/factoryd`, and that directory onto
`PATH` in your shell profile when a new shell would not find `factoryd`
(open a new terminal). A stale build earlier on
`PATH` silently shadows a fresh one — `which -a factoryd` after `make
install` to confirm which one resolves.

On a network that intercepts TLS (a corporate proxy that re-signs HTTPS),
`make install` needs the proxy's CA: for the image builds' downloads, and for
the console's `npm ci`, which without it cannot verify the registry and, under
some Node versions, fails with `Exit handler never called!` instead of a
certificate error. `make install` finds it:

```text
make install
  |
  BUILD_CA_BUNDLE given? -- yes --> use that file
  | no
  registry.npmjs.org signed by a root macOS ships? -- yes --> no bundle
  | no
  that root in the keychain? -- no --> warning: pass BUILD_CA_BUNDLE yourself
  | yes
  write it and macOS's roots to ~/.config/factoryd/build-ca.pem, use that
```

| Consumer | How it reads the bundle |
|---|---|
| Image builds | A BuildKit secret, mounted only for the dependency downloads and never stored in an image |
| pip (project images) | In place of its own roots, so a bundle you pass yourself must include any public roots the proxy does not cover |
| Console `npm ci` | `NODE_EXTRA_CA_CERTS`, beside Node's own roots |

`factoryd doctor` has the same lookup as its `build CA bundle` row:

| Row | Meaning |
|---|---|
| `ok ... (not intercepted, none needed)` | No bundle is used |
| `ok ... (intercepted by <CA>; make install uses this bundle)` with `use: BUILD_CA_BUNDLE=...` | The bundle is written |
| `warn ... signed by "<CA>"` | Intercepted, bundle not written yet: `make install` or `doctor -fix` writes it |
| `warn ... a CA this machine does not trust` | The proxy's CA is not in the keychain: get it as a PEM file and run `BUILD_CA_BUNDLE=/path/to/ca.pem make install` |
| no row | The registry is unreachable, or the machine is not a Mac |

**Upgrade.** `factoryd upgrade [-to <tag|ref>] [-source <checkout>] [-yes]
[-wait]` takes this machine to the newest release tag (or `-to`) in one
command. It builds from a clean checkout: `-source`, else the active
profile's `image_source_root`. It prints the plan (versions, commit count,
profiles whose images get re-pointed, what runs and will restart, skills to
refresh) and asks before touching anything; without a terminal `-yes` is
required. Then it refuses while a request is building (`-wait` polls every
10 s until every `worker` is between requests; a build is never
interrupted), stops each profile's detached `worker` and `serve`, checks
out the release, runs `make install` (which re-points every profile's
images), verifies the installed `factoryd version`, restarts what was
running with the new binary (`launchctl kickstart -k` for launchd services;
Temporal is left running) and reruns `install-skill` for
`~/.agents/skills/buildgate` and `~/.claude/skills/buildgate` unless they are
symlinks. If the install fails it prints the log tail, what is stopped, and
the exact command to restore the previous checkout and install. Already on
the target: it says so and changes nothing.

**What `quickstart` does**, and the manual command for each step:

| Step | What happens | Manual equivalent |
|---|---|---|
| Check setup | Docker, images, model route, mounts | `factoryd doctor` |
| Scaffold onboarding docs | Interactive: asks whether to write `spec/spec.md`/`spec/contract.md`/`ARCHITECTURE.md` if missing; `-non-interactive` skips the prompt and uses the `brownfield` preflight profile instead (or pass `-scaffold` to write them unconditionally) | `factoryd onboard -project <name> -root <repo> -write-factory-yml` |
| Pick a model route | Detects an existing Codex/Copilot/Anthropic login and offers it first (`-route` to pick explicitly: `openai`/`anthropic`/`copilot`/`chatgpt-codex`); lists models, picks a usable one, warns on a weak model | `factoryd init-config`, edit, `factoryd doctor -list-models` |
| Start the queue daemon | Detached, tracked for this session, survives you closing this terminal | `factoryd worker` |
| Start the console | Reuses one already running, else spawns one; opens the tokenized link (`-no-serve` to skip, `-no-open` to skip only the browser open) | `factoryd serve` |
| Submit the request | Prints the request id, the `approve` command with `-data-dir`, and a console link | `factoryd submit -verify-command "..." -preflight-profile brownfield <repo> "<task>"` |

Non-interactively (or a non-terminal stdin, detected automatically),
every prompt above requires its equivalent flag — `factoryd quickstart
-help` lists them all (`-route`, `-model-host`, `-model-id`,
`-context-window`, `-credential`, `-verify-command`, `-build-images`,
`-scaffold`, `-reconfigure`, `-egress-ca-bundle`, ...); `-issue` or
`-request-file` also satisfies the goal requirement. `quickstart` also
defaults `-pr-trusted-authors` to your own `gh` login when unset, so your
own PR review comments trigger a corrective round.

Config reuse: an existing config is reused only when it names
`roles.execution`. An images-only config (what `make install` records on
a fresh machine) is completed with `routes:`/`models:`/`roles:`, keeping
its images. `-route chatgpt-codex` with the default `gpt-5.6-luna` writes
the Luna profile: `reasoning: true`, `thinking_level_map {xhigh, max}`,
execution `medium`, planning and review `max`.

**Codex CLI as the coding agent, Luna as the model:**
`factoryd quickstart -route chatgpt-codex -harness codex <repo> "<task>"`
writes `roles.{planning,execution,review}.harness: codex` over the Luna
profile (`-harness codex` needs a Responses route such as `chatgpt-codex`,
and takes no `-sandbox-image`: the standard worker image carries the
`codex` binary). Review shares Luna with execution, so the profile sets
`allow_shared_model: true` on `roles.review`; edit that if you want an
independent reviewer. `-reconfigure` rewrites the config but keeps the
image refs and `image_source_root` that `make install` recorded
(`sandbox_image`, `meter_image`, `registry_proxy_image`).

Before it submits anything, `quickstart` checks that the repo is under your
home directory (the OpenShell gateway sees only that, on any Docker) and
that it is visible inside a container, i.e. inside what your Docker VM
shares (colima shares `$HOME` by default; "Data directory and colima" narrows
that to `~/buildgate` and your repositories). It stops with the reason if
either fails.

**A Pi fork on GitHub Copilot** (the `pifork` harness):

| Step | Command |
|---|---|
| 1. Install | `make install` |
| 2. Build the fork's worker image from your Dockerfile ([image contract](doc/designs/pifork-harness.md)); note the `name@sha256:...` it prints | `make pifork-image PIFORK_DOCKERFILE=<your Dockerfile>` |
| 3. Give `quickstart` a Copilot login: a Pi login at `~/.pi/agent/auth.json` is found by itself; for the fork's own login pass the token | `export GITHUB_COPILOT_TOKEN=<token>` |
| 4. Configure, start and submit | `factoryd quickstart -route copilot -harness pifork -sandbox-image <digest> <repo> "<task>"` |
| 5. Check | `factoryd doctor` (all green), then watch the request to a draft PR |

`-harness pifork` requires `-sandbox-image` (digest-pinned; quickstart
never builds the pifork image itself). Over an existing config whose
roles are not all on `pifork`, or that names another image, it asks for
`-reconfigure`. `quickstart -harness` takes `pi`, `codex` or `pifork`;
pick `copilot` per role in the session config or per request (see
"Per-request harness choice" below). When a step
fails, send back: the command, its full output, `factoryd doctor`
output, and `factoryd version`.

**Data directory and colima.** colima shares all of `$HOME` read-write
with its VM by default, so a worker that escapes its container could
write any file under it. Share only what a run mounts — `~/buildgate`
(data dirs, script scratch) read-write and your repositories read-only —
in `~/.colima/default/colima.yaml`, then `colima restart`:

```yaml
mounts:
  - location: ~/buildgate
    writable: true
  - location: ~/code        # wherever your target repos live
    writable: false
```

**Sizing colima.** `memory: 4` in the same file fits the default
`max_parallel_jobs: 2`. Once builds have run, the VM holds its whole
configured memory on the host (the VM process's `footprint`; `ps` RSS reads
higher because it counts shared pages) and keeps it until it stops. A build's
`sandbox_memory` is a ceiling: a Go build peaked at 1.26 GiB and a small
Python build at 0.2 GiB; Temporal (server, Postgres, UI) holds 0.3 to 0.4 GiB.

| `max_parallel_jobs` | VM memory | What the Mac holds |
|---|---|---|
| 1 | 3 GiB | about 3 GB |
| 2 | 4 GiB | about 4 GB |
| 3 | 6 GiB | about 6 GB |

`factoryd stop -all` stops the colima VM when nothing else runs in it; the
next factoryd command starts it.

| Path | Needs |
|---|---|
| data dir (a profile's default is `~/buildgate/data`; `quickstart` persists it as `data_dir`) | shared read-write |
| `<repo-path>` (its `.git` is mounted read-only) and `-reference-oracle-dir` | shared, read-only is enough |
| `<repo-path>` of a run that builds in the checkout itself (a data dir inside the repo) | shared read-write: no probe checks writability, so a read-only share fails the build's writes |
| `-config` and everything under `~/.config/factoryd` | not shared: only the host process reads it |

An unshared path halts the run with the exact fix ("does not appear to
share ... into its containers"), never a hang. `factoryd doctor` warns
while the VM still sees all of `$HOME`. If `<cwd>/data` already holds
records, `quickstart` prints a notice but never silently adopts that
path. Run `quickstart` again after any mid-sequence failure — an existing
config is reused, an already-scaffolded repo is skipped, an
already-running daemon is left alone.

**Survive a reboot.** `factoryd install-service` (macOS only) installs
two launchd agents (`dev.factoryd.worker`, `dev.factoryd.serve`;
`-no-serve` installs only the former). `factoryd doctor` reports each
one's state; `factoryd uninstall-service` removes both. `factoryd console
[-open]` reprints the tokenized console link at any later point without
re-running `quickstart`.

**Missing an image?**
`factoryd doctor -fix -repo-root ~/code/buildgate` builds any absent
sandbox/registry-proxy image locally (from source -- there is
nothing to pull, see `make install` above) and prints the ref to use.
`-fix` also starts the OpenShell gateway and the meter when either does
not answer ([The sandbox runtime](#the-sandbox-runtime-openshell-gateway-and-meter)).
`-fix` also offers (with confirmation, or `-fix -yes` non-interactively)
to write a starting `.factory.yml`, backfill missing release-policy
defaults, and repoint a `data_dir` that fails the mount-visibility probe.

## Drive Buildgate from a coding agent

`make install` installs the `buildgate` skill into `~/.agents/skills`,
which Copilot and Codex read, and refreshes `~/.claude/skills/buildgate`
when it exists. Then ask your agent in plain words.

```sh
factoryd install-skill                        # reinstall by hand (Copilot, Codex)
factoryd install-skill -dir ~/.claude/skills  # Claude Code, once
```

| You say | The agent |
|---|---|
| "Use buildgate to implement `x_spec.md` in `~/code/<repo>`" | Submits that file unchanged, follows it to the first gate, gives you the console link |
| "Use buildgate to add X to `~/code/<repo>`" | Drafts a request (goal, acceptance criteria, out of scope, verify command), shows it, submits once you confirm |
| "What's waiting on me in Buildgate?" / "Why did it halt?" | `factoryd inbox`; `watch`'s `reason:`/`next:` lines |

```text
you: "use buildgate to implement x_spec.md in ~/code/repo"
  -> factoryd status                       profile, route that will bill
  -> factoryd doctor -target-repo <repo>   environment + what submit would refuse
  -> factoryd submit -request-file x_spec.md [-preflight-profile brownfield] <repo>
       report: request id, console link, verify command used
  -> factoryd watch <id>                   stops at spec_review / plan_review / pr_review
       report: what is waiting + the approve/reject commands for you
  -> once a ticket builds: the run's Temporal UI link (status -json temporal_ui_url)
```

| Repo | What the agent does |
|---|---|
| None of `spec/spec.md`, `spec/contract.md`, `ARCHITECTURE.md` | Treats it as brownfield without asking, says so, and gives you the `.factory.yml` (`verify_command`, `preflight_profile: brownfield`) to commit so later requests need no flag |
| Some of those files, failing the check | Stops and relays the failures |
| `.factory.yml` committed | Uses its `verify_command` and `preflight_profile` |

Verify command: the one the spec file states, else `.factory.yml`'s, else
the repo's own check (Makefile, `go.mod`, `package.json`, test layout).

Yours, never the agent's: `approve`, `reject`, merging the PR, `factoryd
use`/`stop`/`upgrade`, session-config edits, and committing `.factory.yml`.
The skill is the boundary: `factoryd` accepts any command from your user,
so the agent holds to the skill's command list even when told otherwise.

## `.factory.yml` — commit per-repo defaults once

A repo owner commits `.factory.yml` at the repo's git top level so every
engineer running `factoryd <run>` against it doesn't need to know the
right flags — `internal/projectconfig.Load` fills in any flag left
unset (an explicit flag always wins).

```yaml
verify_command: "make verify"
preflight_profile: brownfield          # "" (strict, default) or "brownfield"
```

`factoryd onboard -write-factory-yml` writes a full starting file
(detected verify command, `preflight_profile: brownfield`, every
optional named gate commented out). Full key reference, named-gate
semantics, and the restrictions on `sandbox_image`/ceiling keys:
[USAGE_REFERENCE.md § `.factory.yml` reference](USAGE_REFERENCE.md#factoryyml-reference).

## New app from scratch

For a brand-new app with no code yet — a different, less common path
from `quickstart` above (which assumes an existing checkout):

| # | Command | What it does |
|---|---|---|
| 1 | `factoryd intake -spec-input "<idea>" -pilot-dir ~/code/your-app` | Runs `goal_pilot.py`: writes `spec/spec.md`, `spec/tickets/001-*.md`, `ARCHITECTURE.md`, then stops for human review (never auto-approves) |
| 2 | Edit `spec/spec.md`/`ARCHITECTURE.md`/tickets by hand, then flip `STATUS: DRAFT -- pending human review` to `STATUS: FROZEN -- reviewed` | Human review gate, non-skippable |
| 3 | Re-run the **exact same** `intake` command | `goal_pilot.py` sees `FROZEN`, drafts `spec/contract.md` + `spec/acceptance/`, stops again. Review the contract by hand — tickets 2+ cite it by name |
| 4 | `factoryd check-project -project <name> -spec <spec.md> -contract <contract.md> -architecture <ARCHITECTURE.md> -ticket <001-*.md> -ticket-number 1 -data-dir <dir>` | Validates ticket structure (`## Goal`, `## Required changes`, `## Verification` mentioning `make verify`, `## Commit`; ticket 2+ also needs "This is an existing repo."). No override — fix and re-run |
| 5 | `factoryd -workspace <pilot-dir>/workspace -spec <001-*.md> -ticket 001 -ticket-file <001-*.md> -verify-command "make verify"` | Runs the ticket — see "Run a ticket" below |
| 6 | `git merge factoryd/<run-id>` in the pilot dir | Merge the accepted run (§ below) |
| 7 | Repeat 1-6 for ticket 2+, adding `-prior-run <ticket-1-run-id>` | Each ticket's run chains from the last |

No separate `git init` needed — `intake` already committed `spec/` into
`-pilot-dir` and scaffolded an empty, gitignored `workspace/`.
Every build runs at the *whole repo's*
worktree root, not confined to `workspace/` — `workspace/` only lets
`factoryd` derive the project root. Assumes `-pilot-dir`/`-workspace`'s
parent is one git repo, the layout `intake` itself produces.

## Run a ticket

```sh
factoryd \
  -workspace ~/code/calc-app/workspace \
  -spec ~/code/calc-app/spec/tickets/001-*.md \
  -ticket 001 \
  -ticket-file ~/code/calc-app/spec/tickets/001-*.md \
  -verify-command "make verify"
```

**`-spec` is the *ticket's own* spec** — same file as `-ticket-file`,
never `spec/spec.md`. `-spec`'s content (not `-ticket-file`'s) is what's
handed to `build_app.py` as build instructions and the only file
`Verify-Command:`/`Allowed-Files:`/`Required-Changed-Files:`/
`Tests-Required:` are parsed from; `-ticket-file` is read only for the
pi-harness structure preflight. Passing the unchanging `spec/spec.md` as
`-spec` on every ticket is refused rather than silently starving the
build (`-allow-spec-ticket-scope-mismatch` is the explicit opt-out). A
bare `factoryd -h` prints only the 5 flags that matter; the rest is in
[USAGE_REFERENCE.md](USAGE_REFERENCE.md).

This runs `build_app.py` in an isolated worktree at
`data/workspaces/<run-id>/` on its own branch, `factoryd/<slug>-<shorthash>`
(`<slug>` from the ticket's Goal), then canonical verification, then records the
run's evaluation (accepted/quarantined) plus a release decision (recorded
only — nothing merges or deploys automatically). The worktree/branch are
**not** cleaned up on acceptance (only a rejected run's are auto-discarded).

**Docker containment is unconditional** — no host-execution opt-out
anywhere, and no built-in default image either: `-sandbox-image`/
`sandbox_image` must be configured (`make install`/`factoryd
configure-images`) or a run is refused before any container launches. A
target project needing a dependency the worker image doesn't bake (no
package-registry network in the sandbox): `make project-sandbox-image
PROJECT_DIR=<path> BASE_IMAGE=<worker ref>` (bakes a project-specific
image) or `-registry-proxy` (a per-run caching proxy, default-on with the
default model-backed build). A `docker-compose.yml` service
dependency (Postgres, Kafka): `-compose-services` (default-on) launches
its services as sidecars the worker reaches via `BG_SERVICE_<NAME>` env
vars (each holds the service's address, not a name: the worker resolves
no service alias), and at each port the compose file publishes (`"5433:5432"`) on its
own `localhost`, so tests that default to what `docker compose up` gives a
developer need no configuration. A compose file with any service outside the allow-list halts the
run before its build (`compose_services_rejected`, naming each service
and the fix). The list (`compose_services_allowed_registries`) is yours:
nothing adds to it because a repository's compose file names an image.
`factoryd doctor -target-repo <path> -fix` and `factoryd quickstart` name
each registry prefix the repo's images need and add them to the config
only when you answer yes (`-fix -yes` for no prompt); `factoryd doctor -target-repo <path>` prints the same
verdict (alongside what `submit` would refuse: no verify command, or the
project-bootstrap preflight under the repo's `.factory.yml` profile), each service's `BG_SERVICE_*` variables, the `localhost` ports
(failing when the sandbox image predates `bg-forward`), and a warning when
the worker's and sidecars' memory limits add up past Docker's. A ticket that edits the
compose file (adding a dependency) is verified against the base commit's
services, never its own edit; the PR's risk header says so and reads
HIGH. Sidecar images are pulled only when missing locally (`docker pull
<image>` refreshes a moving tag). `make live-compose` proves the path
against a real Postgres/Kafka/Redis ticket. Details and the exact
defaults/flags for all three: [README.md § Sandbox](README.md#sandbox);
`compose_services_*` keys: [USAGE_REFERENCE.md](USAGE_REFERENCE.md#keys-with-no-cli-flag-of-their-own).

The default `build_app.py` is model-backed and needs a model route
(`roles.execution`); leaving it unconfigured with the default build
script is rejected up front. Model-route setup (which credential, which
session-config keys): [USAGE_REFERENCE.md § Model routes](USAGE_REFERENCE.md#model-routes).

Running `factoryd` from inside the workspace itself (`-workspace .`)
needs an explicit `-data-dir <path outside -workspace>` — the default
(`data`, inside the workspace) would get mounted into the very worker
containment is meant to isolate from. `factoryd doctor -workspace <dir>
-data-dir <dir>` catches this before a real run fails closed on it.

Useful add-ons: `-sandbox-image <name@sha256:...>` (own
digest-pinned image), `-temporal-address <host:port>` (another Temporal server than the
default — [Temporal](#temporal-what-runs-every-build)), `-prior-run <id>` (chain onto
a prior accepted run).

## Merge an accepted run into `main`

`factoryd` never merges or pushes anything itself (release decisions are
recorded only). The PR title comes from the ticket's `## Goal`; the code
sits on its own isolated branch (see above), ready to merge normally —
substitute the run's actual branch name below:

```sh
cd ~/code/calc-app
git merge factoryd/<run-id>
```

Do this before running the next ticket, and before pruning any
worktrees/branches — they're this pipeline's own durable evidence trail.

## Oracles (`submit -draft-oracles`)

Executable acceptance tests, drafted from the approved spec before the
build exists, that gate acceptance. Adds two states:
`spec_review -> oracle_drafting -> oracle_review -> planning` (see the
lifecycle diagram above).

**When it's reliable.** Bug fixes and behaviour changes on existing code
are the best case: the names already exist, the test fails before the
change and passes after. A new feature is only as reliable as the
contract its spec states — the drafter invents names the builder won't
share if the spec doesn't pin the package, functions, signatures, routes
and JSON shapes.

- Bug fix or behaviour change: use `-draft-oracles`.
- New feature with a firm contract: pin every name/shape in the spec
  first, then use `-draft-oracles`.
- Exploratory new feature: skip it, rely on the verify command,
  `tests_added`, and the conformity review; add contract tests once the
  interface settles.

**Always read every drafted oracle before approving.** Live runs have
produced the wrong package or target path, code that doesn't compile,
wrong expectations, and drafts that timed out or wrote no files. The
canary and syntax checks are guards, not proof of correctness.
`-draft-oracles` stays opt-in until measurement supports a default-on
change.

Step-by-step mechanics, the drafter's self-checks, limits, and the
quarantine-triage oracle hint: [USAGE_REFERENCE.md § Oracle drafting internals](USAGE_REFERENCE.md#oracle-drafting-internals).
`-no-commit-oracles` keeps an accepted oracle out of the repo (it still
gates every build). Console review UI: [`console/README.md`](console/README.md).

**What checks a criterion — text, verify command, or oracle:**

| Mechanism | What it is | Use it for |
|---|---|---|
| Text criterion | LLM opinion (clean / flagged / unavailable) | Intent and qualities no test can state |
| Verify command / `.factory.yml` named gate | Deterministic pass/fail | Thresholds and code properties (coverage floor, complexity, lint) |
| Oracle | Executable behavioural test, read-only to the build agent | Mechanical pass/fail on behaviour |

`full_suite_verify` needs no separate `-full-suite-command`: unset, it
runs the resolved canonical verify command instead (recorded as
`full_suite_source: verify_command`); pass the literal value `none` to
leave the gate unconfigured instead.

## Five repos, zero terminals: `submit` + `worker`

Queue requests as they come to mind, across as many repos as you like;
one background process runs them one at a time.

```sh
factoryd submit ~/code/payments    "Add idempotency keys to POST /refunds"
factoryd submit ~/code/notes-demo   "Reading-time endpoint, see issue #42"
factoryd submit -issue https://github.com/acme/notes-demo/issues/42 ~/code/example-app
```

`submit` starts Temporal, a `worker` and a `serve` when they are missing (pid
and log path printed; without Temporal it reports how to start one) and
ends with `View: <console-link>`. While a `worker` is live, `submit` and every
decision (`approve`, `reject`, `retry`, `amend-scope`, `cancel`, from the CLI
or the console) wake the request's workflow at once. `FACTORYD_AUTOSTART=0`
turns that off: it then only warns when no `worker` is running.

**From the console**: the request board's "New request" button (or
`<console-url>/requests/new`) opens the same form
(`internal/requestsubmit.Submit`, shared with `submit` itself). Unlike
`submit`, an HTTP caller can't name an arbitrary host path — the
workspace must already be a real git repo root and appear in session
config's `workspaces:` list or be the workspace of an existing request.

**Per-request harness choice**: `submit -harness role=name` (repeatable,
e.g. `-harness execution=pifork`) or POST /requests' `"harnesses":
{"execution": "pifork"}` body field picks the coding-agent harness for the
`planning` and/or `execution` role, validated at submit time against that
role's own `roles.<role>.allowed_harnesses` (`review` is never
requester-selectable). The harness is otherwise `roles.<role>.harness`
(default `pi`).

`roles.<role>.harness` values, each running inside the same Docker sandbox
and reaching the model only through its sandbox's supervisor:

| Value | Coding agent | Needs |
|---|---|---|
| `pi` (default) | Pi | any route api |
| `pifork` | A fork of Pi | a digest-pinned `sandbox_image`; any route api |
| `codex` | Codex CLI | a model on an `openai-responses` route (`api: openai-responses`, or a `chatgpt-codex` route); config validation refuses it on completions or anthropic-messages |
| `copilot` | Copilot CLI (bring-your-own-key, no GitHub login) | any route api |

Session config (`allowed_harnesses` is what a request may pick from;
`harness` must be in it):

```yaml
roles:
  execution: { model: luna, harness: codex, allowed_harnesses: [codex, pi] }
```

Full example and per-harness detail:
[USAGE_REFERENCE.md § Codex and Copilot harnesses](USAGE_REFERENCE.md#codex-and-copilot-harnesses-opt-in).

`codex` and `copilot` are baked into the standard worker image (no
`sandbox_image` needed) and take the role's `models.<m>.id` as their model;
`models.<m>.extra_json` (the Pi model entry's extra keys) applies to `pi`/`pifork` only. Copilot
reports no token counts of its own, so a Copilot round's per-round usage
is empty and the meter's figure is the one to read.

**Per-request model choice**: `submit -model role=model` (repeatable,
e.g. `-model execution=sonnet -model planning=opus`) or POST /requests'
`"models": {"execution": "sonnet"}` body field lets a request pick the
model for the `planning` and/or `execution` role, but only within that
role's own `roles.<role>.allowed` (session config's `routes:`/`models:`/
`roles:` block) -- a choice outside `allowed` is refused at submit time,
naming the rejected model and the allowed list. `review` is never
requester-selectable, and the factory still owns every other per-role
setting (routes, thinking). `factoryd run -execution-model <name>` is the
same choice for a bare `factoryd` run, and is refused outright in legacy (no
`routes:`/`models:`/`roles:`) mode, which has no `allowed` list to
validate against. See USAGE_REFERENCE.md § Model routes.

Full command reference for `submit`/`watch`/`status`/`approve`/`reject`/
`retry`/`cancel`/`worker` (flags, `-data-dir`/`-config` resolution;
every request verb takes `-config`):
[USAGE_REFERENCE.md § Command reference](USAGE_REFERENCE.md#command-reference).

## Observe and control

| What | How |
|---|---|
| Watch every request, approve/reject | `factoryd serve`, open the printed URL (`console: http://<addr>/#t=<token>`) — embedded in the binary, same origin as the API. Request board is the landing screen; "Runs" reaches the per-run view |
| New run, release/stats, Operations without a build-time token | The `#t=<token>` fragment in `serve`'s printed URL is captured into the browser on first load and reused thereafter. Changes on every `serve` restart — re-open the newly printed link. `quickstart`/`install-service` instead generate a *stable* token file, reused across restarts; `factoryd console [-open]` reprints it |
| Quick offline run summary | `factoryd status` — first line `profile: <name> (<config path>) · data dir <dir>` (`-json`: `profile`, `config_path`), then one line per run/request: id, project, ticket, state, elapsed, cost, PR URL or halt/quarantine reason. `-project`, `-state`, `-n` (default 20), `-json`. On `chatgpt-codex`/`github-copilot`, cost reads `(API-price est.; billed to your subscription)` (aggregates: `(includes subscription-billed runs; API-price est.)`). While `worker` is alive, also prints `worker route: <credential-mode> · <worker-model>` — which subscription is billed; the route itself is daemon-wide, changed by the operator via `worker`/`quickstart -route`, not per request |
| Remove buildgate from this machine | `factoryd uninstall [-dry-run] [-yes] [-force] [-purge]`: stops everything `stop -all` does, then removes the launchd services, the Temporal containers (`docker compose -p buildgate down`), the OpenShell gateway and meter containers (`docker compose -p buildgate-openshell down`), the `factoryd-local-registry` container, the images `make install` built (`localhost:5050/{buildgate-worker,factoryd-meter,factoryd-registry-proxy,buildgate-pifork,project-worker}:local`), the `buildgate` skill in `~/.agents/skills` and `~/.claude/skills` (only a real directory holding a `SKILL.md`; a symlink is left), and the `factoryd` binary. Only steps whose target exists are listed. If a daemon cannot be stopped (a request is building) nothing else is removed; `-force` cancels the build. `~/.config/factoryd` and `~/buildgate` stay unless `-purge` (also drops the Temporal volumes and the gateway's state; you type `purge` to confirm). With `-purge` the gateway's own state on the Docker VM (`/var/lib/openshell`: its database, keys and stored credentials) is deleted too, through a throwaway container of the worker image run before that image is removed; if the image is already gone it prints the manual command (`colima ssh -- sudo rm -rf /var/lib/openshell`) instead. Reinstall with `make install` |
| Stop what factoryd started | `factoryd stop [-config <profile>] [-all] [-force]`: SIGTERMs this data dir's `worker` and `serve` (found via `quickstart-*.pid`, the worker heartbeat and `console-address`, each checked to still be a factoryd process) and prints one line per process. A launchd-supervised one is left running with a pointer to `factoryd uninstall-service`. Refuses while a request is building unless `-force`; a forced stop cancels a `worker` build (its Temporal workflow is terminated and the run halted; the ticket is rebuilt from scratch at the next `worker` start), while a `worker`'s building requests wait in `resume_review` from the next worker start and `factoryd resume <id>` continues or rebuilds them. `-all` does this for every profile's data dir, then `docker compose stop` on the embedded Temporal stack, then, when colima is the Docker provider and no other container runs, `colima stop` (the next command starts it; left running under `FACTORYD_AUTOSTART=0`); it refuses while any `worker` is still live unless `-force` |
| Everything waiting on you, across profiles | `factoryd inbox [-json]`: every request in `spec_review`, `oracle_review`, `plan_review`, `pr_review`, `resume_review`, `halted` or `quarantined` in each distinct profile data dir, oldest first. Each entry: `<age>  <profile>  <state>  <id>  <title>`, then the `factoryd approve -config <profile> <id>` / `factoryd reject -config <profile> -reason "..." <id>` commands (the PR URL for `pr_review`; `reason:` and `next:` for halted/quarantined), then `console: <link>` when a console link resolves. Prints `Nothing is waiting on you.` when empty |
| Follow one run live | `factoryd watch <run-or-request-id>`: on a terminal, one animated working line with the real state (e.g. `⠹ Forging… spec drafting · planning role luna (pi) · 1m12s`, or a run's stage, round, what it waits on and `STALLED <Nm>` past 5 min of silence); piped or logged, one plain line per change. `quickstart`, `doctor` and `make install`'s image builds show the same kind of line while they wait; console Timeline/Pipeline stepper; the Temporal Web UI; desktop/Slack/Discord notifications on accept/quarantine/halt |
| Read the log that matters | `factoryd logs <request-id \| run-id \| queue-run \| serve>` prints the last 40 lines of the log being written now (newest of the id's logs; for a request, its drafting logs and its current ticket's run). `-f` follows and switches to each newer log until the request or run finishes; `-list` shows every log, oldest first; `-n N` sets the line count. Log text is stripped of terminal escapes; the relay writes no log (usage: `relay-ledger/usage.jsonl`) |
| Cost per accepted ticket, by role x model | `factoryd cost` (`-request <id>` for one request, `-since YYYY-MM-DD`, `-json`) — spec/plan/oracle drafting plus every ticket build, including failed and corrective rounds, rolled up the same way `GET /requests`'s `cost_summary` is; also reports quarantined-ticket and rejected-spec/plan counts as a human-cost proxy |
| Cap a request's (or a month's) total spend | Session-config `request_token_budget`/`request_cost_budget_micro_usd` (one request's drafting + every ticket run + corrective/PR-review round) and `monthly_token_budget`/`monthly_cost_budget_micro_usd` (all requests in the data dir, current UTC calendar month) — 0/absent means unlimited. Unlike `meter_token_ceiling`/`meter_cost_ceiling_micro_usd` (a per-job ceiling the relay itself enforces mid-job), these are checked host-side before a job is launched at all; reaching one quarantines the request (`budget_exhausted:request`/`budget_exhausted:monthly`) naming the key, the spend, and the limit. `factoryd cost` prints the configured budgets and month-to-date spend once any are set |
| Fail closed all future releases for a project | `factoryd kill-switch -project <p> -state engaged -by <you> -reason "..."` — CLI-only, works without `serve` |
| Notification when a request waits with no `worker` alive | `factoryd status` and `factoryd serve` both check the heartbeat file each poll; the console board shows the same banner. Nothing notifies if neither is running |
| Reach `serve` through an ssh tunnel / reverse proxy | Bound to loopback by default, refuses any `Host` header that isn't its own loopback address (DNS-rebinding defense) — `-allowed-host <host[:port]>` adds exact extra values (reads only; writes still need `-override-token`) |
| Console link printed by `submit` / `factoryd console` | Built from `<data-dir>/console-address`, which a live `factoryd serve` writes for its data dir. `submit` and `factoryd console` start that `serve` when it is missing; with `FACTORYD_AUTOSTART=0` (or a `serve` that cannot start) `submit` prints how to get a link instead (or set `-console-base-url` / `FACTORYD_CONSOLE_URL`) |
| Drive Buildgate from Copilot / Claude Code / Codex | `factoryd install-skill`, then ask in plain words: see "Drive Buildgate from a coding agent" above. `factoryd upgrade` refreshes the skill |
| Local model host serialization | Automatic (`internal/modelhost`, keyed by the selected route's own `upstream` host) — no manual "one job at a time" discipline needed. Waiting shows as a `model_host_lock` event / `waiting` chip |
| Compose sidecar serialization | Automatic: at most `compose_services_concurrency` (default 1) runs on this machine have compose sidecars, across every data dir and factoryd process. A run takes its slot before its first phase and holds it to the end, so the wait never counts against its timeout; the wait itself is bounded by one run's timeout, after which the run halts naming the holder. The daemon's recovery of a crashed submission takes the slot too, deferring to its next scan while it is busy. Waiting shows as a `compose_services_lock` event / `waiting` chip |

Every command and flag:
[USAGE_REFERENCE.md § Command reference](USAGE_REFERENCE.md#command-reference).

### Desktop notifications

On macOS, `factoryd` notifies when a run halts, is quarantined, or is
accepted (`FACTORYD_DESKTOP_NOTIFICATIONS=0` opts out).

| Setup | Banner |
|---|---|
| Default | A plain `osascript` banner; clicking it does nothing useful |
| [`terminal-notifier`](https://github.com/julienXX/terminal-notifier) on `PATH` (`brew install terminal-notifier`) | Adds a "Next: ..." line. Clicking opens the run's console page when `FACTORYD_CONSOLE_URL` is set, else the run directory in Finder |

`terminal-notifier` shows nothing until macOS approves it:

1. Fire it once so macOS lists it: `terminal-notifier -title factoryd -message test -execute "open /tmp"`.
2. System Settings → Notifications → `terminal-notifier` (`fr.julienxx.oss.terminal-notifier`): Allow Notifications on, alert style Banners or Alerts (not None), Show in Notification Center on.
3. Re-run step 1; clicking the banner should open `/tmp`.

### Profiles

A profile is a session config file `~/.config/factoryd/<name>.yml` (`config.yml`
is `default`); keep one per model subscription. Only `*.yml` files count, so a
`x.yml.bak-…` backup never shows up.

| What | How |
|---|---|
| List profiles, marking the active one with `*` | `factoryd use`: name, data dir, route · model · harness, and whether a `worker` and a `serve` are alive for that data dir |
| Switch | `factoryd use <name>` (`use default` for `config.yml`): writes `~/.config/factoryd/active-profile`; every command, and the `buildgate` skill, follows it with no `-config`/`-data-dir` |
| One command on another profile | `-config <name>`: a value with no path separator and no `.yml` suffix is a profile name |
| One shell or script | `FACTORYD_PROFILE=<name>`, which beats `active-profile` |

Give every profile its own absolute `data_dir`. Without it a command falls
back to `./data` in whatever directory it runs from, so the profile names no
fixed queue: `use` marks it `no data_dir set`, `use <name>` warns, and
`inbox`/`stop -all` skip it, naming the file to fix.

Resolution order: `-config`, then `FACTORYD_PROFILE`, then `active-profile`,
then `~/.config/factoryd/config.yml`, then `~/.factory/config.yml`. An active
profile whose file is missing is an error naming the fix (`factoryd use
<name>`), never a fallback. `make install` re-points every profile at the
fresh images unless `FACTORYD_CONFIG` names some.

## Temporal: what runs every build

Temporal is buildgate's only execution path: every command that runs a
build (`quickstart`, `submit` + `worker`, a single-ticket run, the console,
`live-smoke`) uses it, starting it when it is down. It keeps a request's
progress and a build's completed steps durable, and it is what lets a lost
step wait for your `factoryd resume` decision with its work kept.

```text
factoryd submit / approve / reject / retry / resume / cancel, console
        |  saves the decision in request.json, then wakes the workflow
        v
factoryd worker ---- RequestWorkflow (one per request) ----+
        |               drafting, planning: model jobs      |  jobs queue:
        |               building: one ticket at a time      |  max_parallel_jobs at once
        v                                                   |
factoryd <run> (one ticket) ---> RunWorkflow <--------------+
                                 (build, verify, gates as Activities)
                                        |
                                        v
                          Temporal server (Postgres volume)
```

### Start and stop the server

```sh
make temporal-up                                        # or:
docker compose -f docker-compose.temporal.yml up -d
docker compose -f docker-compose.temporal.yml down      # stop, keep data
docker compose -f docker-compose.temporal.yml down -v   # stop, wipe data
```

- `make install` runs `temporal-up` for you (skipped without Docker); `worker` and `quickstart` start it too when it is down (below), sharing this same stack and volume.
- Postgres-backed; data persists in a named volume.
- Web UI: http://localhost:8233. gRPC: `localhost:7233`.
- Set `TEMPORAL_POSTGRES_PASSWORD` (env or `.env`) before the first `up`
  anywhere shared. The default, `temporal`, is for local use only.

### The default address

- Temporal is the default. `quickstart`, `worker` and a single-ticket
  run dial `localhost:7233`; if nothing answers and Docker works, they start the
  compose stack embedded in the binary (first start pulls images, behind a
  spinner, up to 3 minutes), then print "Temporal started at ...". If it
  cannot start, they print one line saying why and the run halts.
  `FACTORYD_AUTOSTART=0` neither starts nor auto-selects it: an address must
  then be given with `-temporal-address`, or the command is refused.
- `worker` forwards the address it settled on to every ticket it builds (each
  ticket runs as a `RunWorkflow`). An explicit address is used as given. An
  unreachable explicit server halts the run ([`USAGE_REFERENCE.md`](USAGE_REFERENCE.md#temporal-repository-owners-daemons-observing)).
- `factoryd doctor` fails its Temporal check when Temporal is unreachable
  (builds need it); `doctor -fix` starts it.

### Requests under the worker

`factoryd worker` drives every request of one data dir. Each request is one
`RequestWorkflow` (workflow id `factoryd-request-<request id>`);
`request.json` stays the source of truth and the workflow keeps no request
state of its own.

| What | How it runs |
|---|---|
| One step of a request (a drafting job, the plan, one ticket's build, one PR poll) | One `AdvanceRequest` Activity, which saves `request.json` itself; a step that changes the state is followed by the next at once |
| Model jobs and builds | The jobs task queue, at most `max_parallel_jobs` at once across all requests (default 2); everything else runs on an uncapped light queue. Both are named after a random id kept in `<data-dir>/worker-queue-id` |
| A ticket's build | Its own `RunWorkflow` (workflow id = run id), started by that step |
| Waiting on you (`spec_review`, `oracle_review`, `plan_review`, `resume_review`) | The workflow sleeps; a reminder goes out every `-hitl-reminder-interval` |
| Your decision (`approve`, `reject`, `retry`, `resume`, `cancel`, amend scope, send back; CLI or console) | Saved to `request.json`, then a wake signal ends the wait. With no worker running the decision is still saved and picked up at the next start |
| `pr_review` | The PR is read at most once per `-pr-poll-interval` |
| A build on a repository another build holds alone | Gives its job slot back and retries every 30 s |
| A step whose worker stops (no heartbeat for 2 minutes), or a worker restarted mid-build | The request goes to `resume_review`; nothing reruns on its own (below) |

### A lost step waits for a human

Temporal never reruns a lost step on its own. A step lost because its
`factoryd` process stopped or went quiet (laptop sleep, a crash, a lost
worker) halts its run, and a build keeps its worktree. A request run by
`factoryd worker` then waits in `resume_review` (a reminder at every
`-hitl-reminder-interval`) for one of three decisions:

```text
step lost (heartbeat stops)
        |
        v
run halted, lost build's worktree kept      request -> resume_review
        |
        +-- factoryd resume <id>               continue the build from the last
        |                                      completed round (fresh agent session +
        |                                      short handoff), or rerun a lost
        |                                      drafting / planning step
        +-- factoryd resume -from scratch <id> rebuild the ticket, reaping the kept worktree
        +-- factoryd cancel <id>               withdraw it, reaping the kept worktree
```

`resume` for a build runs only when no sandbox container of the lost run
is alive and the worktree is on the run's branch at (or descended from) the
HEAD its round state recorded; otherwise it is refused and only
`-from scratch` or `cancel` remain. A resumed run adopts the kept worktree,
lowers its token and cost ceilings by what the lost run spent, and reruns every gate
on the final commit as usual.

| Situation | What happens |
|---|---|
| Lid closed or Mac asleep mid-build | The step is lost when its heartbeat stops: the worker puts the request in `resume_review`. An idle Mac does not sleep mid-build (`caffeinate`) |
| `factoryd stop -force`, `kill -9`, crash, reboot (`worker`) | The request building it enters `resume_review` at the next `worker` start; `factoryd resume <id>` continues it |
| A build lost with its process (halted as above, or `factoryd` stopped with SIGTERM) | A request's run keeps its worktree and branch, flagged `kept_for_resume`, until a human decides (`resume`, `resume -from scratch`, `cancel`, or `retry` of a halted request reaps it); its containers are still removed. `factoryd -resume-worktree-of <run-id>` starts a run that adopts the kept worktree and continues from its round state |

Repository-owner runs (`-repository`), daemons, what to look at in the
Temporal Web UI and the Activities an oracle adds are in
[`USAGE_REFERENCE.md`](USAGE_REFERENCE.md#temporal-repository-owners-daemons-observing).

## The sandbox runtime: OpenShell gateway and meter

Every worker container is created by the
[OpenShell](https://github.com/NVIDIA/OpenShell) gateway (0.1.2, images
pinned by digest), not by `docker run`.

```text
  factoryd --mTLS--> gateway --Docker--> worker (no network)
                        |                    |
                        | credential         | only route out
                        v                    v
                   credential store     supervisor --> model upstream
                                             |
                                             v
                                           meter (token and cost ceilings, ledger)
```

| Part | What it is | Where |
|---|---|---|
| Gateway | Creates and deletes sandboxes, holds each route's credential | Container `buildgate-openshell-gateway-1`, `127.0.0.1:17670` on the Docker VM (health on `:17671`) |
| Supervisor | One per sandbox; the worker's only route out; admits the route's upstream, path and model executables | Started by the gateway with each worker |
| Meter | buildgate's own service; counts each run's tokens and cost and refuses a request past a ceiling | Container `buildgate-openshell-meter-1`, `127.0.0.1:17672`; ledgers in `~/buildgate/meter-ledgers/` |
| Stack files | Compose file, gateway config, client certificate | `~/.config/factoryd/openshell/` |

| Task | Command |
|---|---|
| Start both | `factoryd doctor -fix` (needs `meter_image`, which `make install` writes); `worker` and a single-ticket run start them when they do not answer, unless `FACTORYD_AUTOSTART=0` |
| Check | `factoryd doctor`: images present, gateway healthy, gateway config has TLS and mTLS on, meter healthy |
| Stop | `factoryd stop -all`; refused while a request or sandbox is active, because a gateway restart starts every sandbox's command again |
| Pull the pinned OpenShell images | `make openshell-images` (part of `make install`) |
| Apply changed stack files after an upgrade | `factoryd stop -all`, then `factoryd doctor -fix`: a gateway that already answers is not recreated |

The gateway container sees your home directory read-only, because it
refuses a bind whose source it cannot see and a build binds its
repository's `.git`. A repository outside your home directory cannot be
built.

What changes for a build:

- The worker has no network interface on any step. A model-backed step
  reaches exactly its route's upstream; the registry proxy and compose
  services are reached by address.
- The worker resolves no service name. A service whose clients reconnect
  to an address the server advertises (a Kafka broker, a Redis cluster, a
  MongoDB replica set) must be reached through a port the compose file
  publishes, on a listener that advertises `localhost:<published port>`:
  the worker's `localhost` forwards every published port. Reached at its
  `BG_SERVICE_<NAME>` address instead, the first connection succeeds and
  the one to the advertised name fails (`connect: permission denied`).
- The route's credential is pushed to the gateway before each launch; the
  worker holds a placeholder. A token that expires before the step's time
  budget ends refuses the launch.
- A `credential_mode: github-copilot` route sends the stored GitHub login
  token as its bearer token (it does not expire during a step); the meter
  writes the client headers the Copilot API requires.
- If the gateway restarts during a build, the worker's command is not run a
  second time: the step is recorded lost and the request waits in
  `resume_review` ([A lost step waits for a human](#a-lost-step-waits-for-a-human)).

## Other ways to run

| Want | Do |
|---|---|
| Durable, retryable execution on Temporal | Every build runs on it (`worker`, a single-ticket run, `live-smoke`): Temporal at `localhost:7233`, started with Docker when it is down (`make temporal-up` does the same by hand). If it cannot start the run halts; with `FACTORYD_AUTOSTART=0` an address must be given with `-temporal-address`. See [Temporal](#temporal-what-runs-every-build) |
| A different harness for one request | `factoryd submit -harness execution=<name> ...` (within `roles.execution.allowed_harnesses`) |
| Oracle stage for one request | `factoryd submit -draft-oracles ...`; add `-no-commit-oracles` to keep accepted oracles out of the repo |
| Per-repo defaults | Commit `.factory.yml` |
| Prove the pipeline end to end before merging pipeline changes | `make live-smoke` (real Docker, real model route, a few minutes) |
| Unit and lint checks | `make verify` (Go), `make console-test` (console). CI is manual-only — run these yourself |

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Halt naming a path that "does not appear to share ... into its containers" | The macOS Docker VM doesn't share `/private/tmp` (or anything outside `$HOME`) | Use a repo path, `-data-dir` and workspace under `$HOME` |
| New flags rejected, fewer `doctor` checks, odd old behaviour | A stale `factoryd` earlier on `PATH` | `which -a factoryd`; run the fresh build by full path or fix `PATH` |
| Halt says the factoryd process running this build stopped mid-run | A `worker`'s `factoryd` process was stopped or lost | A request waits in `resume_review`: `factoryd resume <id>`; a single-ticket run is started again |
| Request is in `resume_review`: "the factoryd worker stopped while the ... step ran" | The `worker` running that step stopped (stop, crash, reboot, sleep); no step is ever retried automatically | `factoryd resume <id>` continues it (a build from its last completed round), `factoryd resume -from scratch <id>` rebuilds the ticket, `factoryd cancel <id>` drops it |
| Quarantine names a gate but not why it failed | Nothing wrong — the triage sentence quotes the first compile/test failure line from that gate's log | Read the run's full log for the rest |
| Request halts at preflight for a repo | Repo has no `.factory.yml` (strict preflight profile) | `factoryd onboard -project <name> -root <repo> -write-factory-yml`, commit, resubmit |
| `factoryd doctor` warns the release policy denies every PR unconditionally | `release_max_files_changed`/`release_max_insertions` is `0` or `release_rollback_plan` is empty | Add all three to `config.yml` (`init-config`'s scaffold has usable defaults; `quickstart` writes them into a fresh config automatically) |
| Status shows "accepted, awaiting pull request" | Every ticket's build was accepted but no PR exists (`-open-pull-request=false`, a release-policy denial, or the PR failed to open) | Merge the branch by hand, or `factoryd retry <id>` — re-opens just the PR if the release decision now allows it, otherwise rebuilds |
| Status shows `building*` / `queued behind <id>` | This request's own ticket hasn't started yet — another request is the one currently building; only one build runs at a time | Nothing to do — it starts automatically once `<id>` finishes; `waiting_on` on the API/console shows the same id |
| Quarantined for `spec_conformity` and `factoryd retry` just fails again | The build didn't conform to the spec's own acceptance criteria — retrying rebuilds against the exact same spec | `factoryd reject -to spec -reason "..." <id>` to redraft the spec instead (the request's `next_action`/`factoryd watch` name this directly) |
| Spec/oracle/plan draft timed out or failed | Model slow/unreachable, or a malformed draft | `factoryd retry <id>` (spec halt) or `factoryd reject -reason "..." <id>` (oracle/plan review) |
| Runs stall with no error | A local/private model host is single-instance; `internal/modelhost` serializes callers automatically. Likewise only `compose_services_concurrency` runs at a time have compose sidecars | `factoryd watch`/console show a `model_host_lock` or `compose_services_lock` `waiting` line naming the holder |
| Containers left behind after killing a run | The process died before cleanup | `docker ps`, then `docker rm -f` only the `factoryd-*` containers |
| Run refuses at startup: "git credential preflight: ..." | The target repo's git config declares a credential helper, a fixed HTTP header, or a URL with embedded userinfo — a sandboxed worker's read-only git mount would expose it | Remove the offending key from the repo's own git config; keep credential helpers outside the mounted tree |
| A build fails: "sandbox runtime: ... run `factoryd doctor -fix`" | The OpenShell gateway or the meter is not running (a reboot, a colima restart, `stop -all`) | `factoryd doctor -fix` starts both; `worker` and a single-ticket run start them too unless `FACTORYD_AUTOSTART=0` |
| `doctor`, `doctor -fix`, `worker` or a run reports `port N is published by container ...` | Another container in the Docker VM publishes one of the stack's ports (`17670`, `17671`, `17672`); the gateway shares the VM's network, so it cannot bind | Stop that container or publish it on another host port, then `factoryd doctor -fix` |
| A launch is refused: "credential expires at ..." | The route's token (`~/.codex/auth.json`) expires before the step's time budget ends | Run any `codex` command to refresh it, then `factoryd retry` |
| `make` in the worker fails every recipe with "Operation not permitted" | Your own worker image carries a stock GNU make; OpenShell's sandbox denies the set-id calls it makes | Build the image on buildgate's worker image (`make project-sandbox-image`), whose make is built without `posix_spawn` |
| A build is refused: `compose_services_worker_env ... names the service ... as a host` | The worker joins no network and resolves no service name | Read `BG_SERVICE_<NAME>` (an address) in the target repo instead |

Per-command flags, session-config keys, model-route setup, `.factory.yml`
reference, and oracle/request-driver internals:
[`USAGE_REFERENCE.md`](USAGE_REFERENCE.md).
