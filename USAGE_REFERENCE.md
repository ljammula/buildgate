# buildgate: detailed reference

The detailed back half of [`USAGE.md`](USAGE.md): per-command flags,
gotchas, model-route and harness configs, and the full session-config key
table. New here? Start with [`README.md`](README.md), [`DEMO.md`](DEMO.md)
and [`USAGE.md`](USAGE.md). When this page and
`factoryd <command> -h` disagree, `-h` wins.

## Command reference

| Command | What it does | Key flags |
|---|---|---|
| `factoryd submit <workspace> [text]` | Records a request under `<data-dir>/requests/<id>/`, then wakes the request's workflow when a `factoryd worker` is live for the data dir, starts a `worker` and a `serve` when no drainer or console is live (pid and log path printed) and prints `View: <console-link>`; with `FACTORYD_AUTOSTART=0` it starts nothing and warns when no `worker` is draining. Needs a resolvable `Verify-Command` up front. | `-verify-command`, `-preflight-profile` (default: `.factory.yml`); `-full-suite-command` (the `full_suite_verify` gate; default: `.factory.yml`'s `full_suite_command`); `-harness role=name` (repeatable, e.g. `-harness execution=pifork`) picks the coding-agent harness for the `planning` and/or `execution` role, only within that role's own `roles.<role>.allowed_harnesses` -- `review` is never requester-selectable; refused with no `roles:` session config; `-model role=model` (repeatable, e.g. `-model execution=sonnet -model planning=opus`) picks the model for the `planning` and/or `execution` role, but only within that role's own `roles.<role>.allowed` -- `review` is never requester-selectable, and every other per-role setting (routes, thinking) stays factory-owned; refused outright with no `routes:`/`models:`/`roles:` session config at all (see Model routes below); `-draft-oracles` (add the staged oracle step, below); `-no-commit-oracles` (oracles still gate, nothing committed); `-request-file <path>` or `-issue <url>` (via `gh issue view`; exclusive with inline text); `-spec-file <path>` hands over a finished spec in place of the drafted one, and `-plan-dir <dir>` with it the finished tickets (below); `-console-base-url` (or `FACTORYD_CONSOLE_URL`) sets the printed console link, which otherwise points at the console a live `factoryd serve` for the same data dir recorded (never a guessed default port); `-watch` attaches like `factoryd watch`; `-config`, `-data-dir` |
| `factoryd watch <run-or-request-id>` | Follows a run's progress feed; for a request, follows each ticket's run in turn and prints the `approve`/`retry` hint when it waits on you. Recap on exit. | `-data-dir`, `-no-follow` (print what exists and exit), `-config` |
| `factoryd worker` | The only request driver. Drives every request of the data dir through Temporal: one `RequestWorkflow` per request (workflow id `factoryd-request-<id>`), started or woken at worker start for every request not `done` or `cancelled`. Model jobs, builds and `pr_review` passes (which can run a corrective build) run on task queue `factoryd-jobs-<id>`, at most `max_parallel_jobs` at once across requests; workflows and light steps (loading state, reminders, halts) on uncapped `factoryd-light-<id>`. `<id>` is the random id in `<data-dir>/worker-queue-id`. Holds the drain lock: a second worker for one data dir refuses to start. A step lost mid-run (worker killed or stopped) puts its request in `resume_review` and waits for `factoryd resume`; a lost `pr_review` pass that only read the PR polls again. The next worker start halts the runs a lost step left (their workflows terminated, containers removed, a lost build's worktree kept) and puts the requests running them in `resume_review`; nothing is rerun on its own and Temporal never retries a lost Activity. While it is live, `submit`, the API's `POST /requests` and every decision (`approve`, `reject` (including send-back), `retry`, `resume`, `amend-scope`, `cancel`, on the CLI and the API) wake the request's workflow (signal-with-start, so a request submitted before the worker started is adopted); a wake that cannot reach Temporal prints one warning and the decision stays saved. Without a live worker nothing is woken: the next worker start reads `request.json` itself. A review wait also rechecks at `-hitl-reminder-interval`. `submit`, `quickstart` and `upgrade` start it. Requests on one repository build concurrently in their own worktrees; only a build that touches the shared checkout holds the repository alone, and stranded worktrees are cleaned when the repository is idle. Known limit: a build that needs the repository alone (a non-isolated build, a repository-owner run, a corrective round on an existing branch), the daemon reclaim and `factoryd reconcile` can wait indefinitely while isolated builds keep overlapping, since the lock has no writer preference; stranded worktrees are cleaned up only when no build holds the repository, and `factoryd reconcile` reports busy then. Writes a liveness heartbeat with its Temporal address, `max_parallel_jobs` as job slots and every request it is running a job for, so `status`, `stop`, `use`, `upgrade` and the console see it; with every slot busy, the other job-state requests show as waiting on the first running one. A build that needs the repository alone, while others hold it, waits for that lock (retrying every 30 s, without holding a job slot) instead of halting the request. Needs Temporal. | `-config`, `-data-dir`, `-skip-doctor`. Image: `-sandbox-image` (also a session-config key). The model route itself (upstream, credential, worker model id/API) is configured entirely through session config's `routes:`/`models:`/`roles:` block, not flags -- see "Model routes" above. `-registry-proxy`, `-registry-proxy-image`, `-egress-ca-bundle`, `-compose-services` (default on). Build: `-build-app-script`, `-build-app-max-attempts`, `-verify-max-attempts`, `-open-pull-request` (default **true** here), `-conformity-policy` (`required` default / `advisory`), `-temporal-address` (default: Temporal at `localhost:7233`, started with Docker if down; `none` is refused; an address is used as given). Request driver: `-draft-spec-script`, `-spec-draft-timeout-minutes` (10), `-plan-tickets-script`, `-plan-tickets-timeout-minutes` (15), `-draft-oracles-script`, `-draft-oracles-timeout-minutes` (15), `-hitl-reminder-interval` (15m, min 1m), `-advance-on` (`accepted` default / `pr_approved`). PR review loop: `-pr-poll-interval` (5m, min 1m), `-pr-trusted-authors` (empty = no comment triggers a round), `-pr-ignore-authors` (wins over trusted), `-max-review-rounds` (3, min 1) | |
| `factoryd status` | Requests (state, project, age, `ticket i/n`, latest PR URL), then runs. | `-project`, `-state` (runs only), `-n` (default 20), `-json` (`{"requests": [...], "runs": [...]}`), `-data-dir` |
| `factoryd logs <request-id \| run-id \| queue-run \| serve>` | Prints the newest log for the id (the one being written now): a request's drafting logs plus its current ticket's run logs, a run's own logs, or the newest launchd/quickstart `.out`/`.err` pair for `queue-run` (the worker)/`serve`. Header line `==> <path> (<size>, modified <age> ago)`, then the last lines, with terminal escapes stripped. `-f` follows, switching to newer files, until the request or run is terminal. `-list` shows every log oldest first (a request lists all its tickets' runs), saved prompts included, marked `[prompt]`; `-prompt <name>` prints the saved prompts of that name in full (`<launch>-<n>/<name>` picks one launch), terminal escapes stripped. | `-n` (default 40), `-f`, `-list`, `-prompt <name>`, `-data-dir`, `-config` |
| `factoryd cost` | Cost per accepted ticket, grouped by role x model (`execution`/`planning`/`review`/`unknown`), across drafting jobs (spec/plan/oracle) and every ticket build, including failed and corrective rounds. Reuses the same rollup `GET /requests`'s `cost_summary` field is computed from (`api.Server.ComputeCostSummary`), never a separate calculation. Also reports quarantined-ticket and rejected-spec/rejected-plan counts, the human-cost proxy alongside the dollar figures. | `-request <id>` (one request only), `-since YYYY-MM-DD` (requests submitted on/after; default: all), `-json`, `-data-dir`, `-config` |
| `factoryd approve <request-id>` | Releases a request from `spec_review` (to `planning`, or `oracle_drafting` if submitted with draft oracles), `oracle_review` (to `planning`) or `plan_review` (to `building`). Refuses any other state. The CLI does not show oracle files; the console does and pins their hash into the approval. Refused from `spec_review` while the spec has a `[NEEDS DECISION]` item under Open questions: the items are listed, and the answers go in `reject -reason`. | `-config`, `-data-dir` |
| `factoryd reject -reason "<text>" <request-id>` | Sends `spec_review`/`oracle_review`/`plan_review` back to `spec_drafting`/`oracle_drafting`/`planning`, appending the reason to `request.md` (an oracle rejection also feeds it to the next drafting pass). With `-to plan\|spec` on a `quarantined`/`halted` request instead: sends it back to `planning` or `spec_drafting` -- refused once any ticket is accepted, or for `plan` with no approved `spec.md`; every ticket (and, for `spec`, `spec.md` itself) must be re-approved before any build. | `-reason` (required, before the id), `-to` (`plan`\|`spec`; quarantined/halted only), `-config`, `-data-dir` |
| `factoryd retry <id>` | A `quarantined`/`halted` request mid-`building`: back to `building` at the same ticket with a fresh run, which continues from the quarantined attempt's commit when a build may be told what it failed on (see "A retry's rebuild"). Same rules as `POST /requests/{id}/retry`. | `-reason` (requests only), `-from attempt\|scratch` (default `attempt`; `scratch` rebuilds from the base commit; the API body's `"from"`), `-config`, `-data-dir` |
| `factoryd resume [-from round\|scratch] <request-id>` | The decision for a request in `resume_review` (its step was lost because the `worker` stopped): `-from round` (default) continues a lost build in its kept worktree from the last completed round, or reruns a lost drafting or planning step; `-from scratch` rebuilds the ticket from a fresh worktree (a drafting or planning step simply reruns). For a lost build, `round` first checks that no sandbox container of the lost run is alive and the worktree HEAD equals or descends from the HEAD its round state recorded, and refuses otherwise, naming `-from scratch` and `factoryd cancel`; the check is made again when the build launches. A lost drafting or planning step is refused (either verb) while a container its job launched is still alive; `worker` start removes those of a dead process first. Cancel with `factoryd cancel`. `factoryd retry` refuses a request in `resume_review`. Same rules as `POST /requests/{id}/resume` (body `{"from":"round"\|"scratch","by":"<name>"}`; 409 for another state or a refused check). | `-from` (`round`\|`scratch`, before the id), `-config`, `-data-dir` |
| `factoryd amend-scope -reason "<text>" <request-id> <file>...` | A human operator widens ONE `quarantined` ticket's approved `Allowed-Files:` scope by hand, re-pinning that ticket spec's approval hash, so `factoryd retry` can rebuild it -- the recovery for a build `diff_scope` quarantined over a file that is a legitimate part of the ticket but that the plan never listed. Refused unless the ticket has no PR yet, every existing approval hash still verifies, and each file is a new, valid, not-already-allowed workspace-relative path. Only that one `Allowed-Files:` line is rewritten; state stays `quarantined`. | `-reason` (required, before the id), `-config`, `-data-dir` |
| `factoryd cancel <request-id>` | Moves a non-terminal request, or one already `quarantined`/`halted` (dismisses it instead of retrying), to `cancelled`. Refuses `done`. Same rules as `POST /requests/{id}/cancel`. | `-reason`, `-config`, `-data-dir` |
| `factoryd install-service` | macOS: installs and bootstraps launchd agents `dev.factoryd.worker` (`factoryd worker -temporal-address <addr>`, the address resolved as `submit` does; refuses with one line when no Temporal is available) and `dev.factoryd.serve` (`KeepAlive`, logs under `<data-dir>/logs/`). Creates or reuses a stable start token at `<config dir>/serve-start-token` (0600); the plist never embeds it. | `-config`, `-data-dir` (default: config `data_dir`; error if neither), `-force` (overwrite existing plists), `-print` (print the worker plist only), `-no-serve` |
| `factoryd uninstall-service` | macOS: boots out and removes both agents. Leaves the start token file for reuse. | none |
| `factoryd console` | Prints the tokenized console link (`<addr>/#t=<token>`) from the stable token file, for the running `serve` of this data dir (the address it recorded); a plain link if no token or no such serve is found. | `-config`, `-data-dir` (when serve's differs from the config's), `-open`. Starts a `serve` for the data dir first when none is running (unless `FACTORYD_AUTOSTART=0`) |
| `factoryd mcp` | Turns on `serve`'s MCP endpoint (`POST /mcp`): creates the token file `<config name>.mcp-token` beside the session config (`config.mcp-token` for `config.yml`, mode 0600, one per profile) if it is missing, then prints the endpoint, the token and the `claude mcp add` line. With no `serve` recorded for the data dir it prints neither, and says to start one. `serve` reads the file on every call, so nothing restarts. Tools and limits: [USAGE.md § Drive Buildgate from an MCP client](USAGE.md#drive-buildgate-from-an-mcp-client) | `-rotate` (new token; the old one stops at once), `-disable` (remove the file; endpoint off), `-config`, `-data-dir` (when serve's differs from the config's) |
| `factoryd inbox` | Lists every request waiting on the operator (`spec_review`, `oracle_review`, `plan_review`, `pr_review`, `resume_review`, `halted`, `quarantined`) across the distinct data dirs of all profiles, oldest first (waiting since `WaitingSince`, else `EnteredAt`): age, profile, state, id, title, then the commands or PR URL or `reason:`/`next:`, then the console link. Empty: `Nothing is waiting on you.` | `-json` (the same entries as an array: `profile`, `data_dir`, `id`, `title`, `state`, `since`, `age_seconds`, `reason`, `next`, `approve`, `reject`, `pr_urls`, `console_url`) |
| `factoryd memory <subcommand>` | Repository memory for one repository: `list` (the switch, the budget, the lines in force, the candidates; with memory on it first collects candidates from finished runs' notes), `show <id>`, `add "<text>"`, `drop <id>`, `propose [<id>...]` (opens one request that rewrites the fenced section of `AGENTS.md` and stops at `spec_review`), `on`, `off`. See "Repository memory" below. Every subcommand but `list` and `show` is refused while memory is off. | `-workspace <repository>` (required), `-config`, `-data-dir`, `-json` (`list`, `show`), `-reason` (`drop`, `off`), `-remove "<exact line>"` (`propose`, repeatable). Flags come before the arguments |
| `factoryd stop` | Stops the `worker` and the `serve` of the data dir, one output line per process (`stopped (pid N)`, `not running`, or why it was skipped). Finds pids from `<data-dir>/quickstart-queue-run.pid` or the worker heartbeat, and from `<data-dir>/console-address` or `quickstart-serve.pid`, and signals a pid only while it still looks like factoryd. A running launchd service (`dev.factoryd.worker`/`dev.factoryd.serve`) for that data dir is left alone (`factoryd uninstall-service` removes it). Refuses when the heartbeat names any request being built or run (all are listed). With `-force`, the running builds halt at the next worker start and their requests wait in `resume_review` (`factoryd resume <id>` continues or rebuilds one). Exits non-zero on a refusal or a process that would not exit. | `-config` (path or profile name), `-data-dir`, `-all` (every profile's data dir, then, when `~/.config/factoryd/openshell` exists, `docker compose -p buildgate-openshell stop gateway meter` (refused while a request or sandbox is active), then `docker compose -f ~/.config/factoryd/temporal/docker-compose.yml stop`, then `colima stop` when colima is the Docker provider and no other container runs (`factoryd uninstall` leaves the VM running; `FACTORYD_AUTOSTART=0` leaves it too); refuses while a worker is still live; exclusive with `-config`/`-data-dir`), `-force` (stop despite a building request, cancelling its build, or, with `-all`, a live worker) |
| `factoryd restart` | Stops every profile's running `worker` and `serve` and starts them again with this binary (a launchd service is kickstarted), one line saying what came back. A worker runs each build in its own process, so one started before an install keeps building with the code it started with; `make install` runs this itself. Refuses while a request is building, naming it, and stops nothing. If a process does not exit within 15 s of SIGTERM, restart starts the others again, leaves that one running, names it and exits non-zero: stop it by pid, then `factoryd restart`. `serve` ends open event and log streams when it is told to stop, so a console tab left open does not hold it. `factoryd doctor` warns (`worker runs this factoryd`) when the running worker is another version |
| `factoryd setup` | Chooses the model and coding agent every run uses and writes them into the session config, keeping its recorded images. `make install` runs it. Asks which model route (detected ChatGPT/Codex and Copilot logins first) and, on a route that can run more than one (`chatgpt-codex`: `pi` or `codex`), which coding agent; the rest is defaulted. Asks nothing with no terminal (a single detected login is used, else it fails naming `-route`) or when the config already names a model. | `-route`, `-harness`, `-model-id`, `-model-host`, `-context-window`, `-credential`, `-sandbox-image`, `-egress-ca-bundle`, `-config`, `-data-dir`, `-non-interactive`, `-reconfigure` |
| `factoryd upgrade` | Upgrades this machine to a release: resolves the source checkout (`-source`, else the active profile's `image_source_root`; must be a clean `module buildgate` checkout), fetches tags from origin, picks the target (`-to`, else the newest `m<N>` tag), and exits 0 with "already on <target>" when the installed `factoryd version` equals its 12-char sha. Otherwise prints the plan and asks (`-yes` required without a terminal), refuses while any profile's worker heartbeat names an active request (`-wait` polls every 10 s), stops each data dir's detached `worker` and `serve` (as `factoryd stop` does; launchd services stay up and get `launchctl kickstart -k` afterwards; Temporal is left running), checks the target out detached and runs `make install` in the source, checks the installed binary (`go env GOBIN`, else `GOPATH/bin`) prints the target's version, restarts what ran with that binary (a running process that is not a worker, such as an old `queue-run`, is replaced by a worker), and runs `install-skill -dir <parent>` for each non-symlinked `~/.agents/skills/buildgate` and `~/.claude/skills/buildgate`. A failure after the stop prints the log tail, which processes are stopped, where the source is, and the command that checks out the previous sha and reruns `make install`. | `-to`, `-source`, `-yes`, `-wait` |
| `factoryd use [<name>]` | With no argument, lists the profiles (`*` marks the active one): name, data dir, execution route · model · harness, worker and serve liveness. With a name, validates that profile loads and makes it active by writing `active-profile` (`use default` is `config.yml`). | none |
| `factoryd uninstall` | Removes what `make install` put on this machine: stops `worker`/`serve`/Temporal, removes the launchd services, the Temporal containers, the local registry container, the locally built images, the `buildgate` skill, the `factoryd` binary and the `factoryd` links on `PATH` that point at it (`~/.local/bin/factoryd`). Lists the plan and asks first. Keeps `~/.config/factoryd` and `~/buildgate` (requests, runs, evidence) unless `-purge`, which also deletes the Temporal volumes and the OpenShell gateway's state on the Docker VM, and asks you to type `purge`. Never touches Docker/colima, Go, Node, `gh`, Homebrew packages or git config. | `-dry-run` (only prints), `-yes` (skips the question; required off a terminal), `-force`, `-purge` |
| `factoryd kill-switch -project <p>` | Durably engages or disengages a project's release kill switch; without `-state`, prints its state and history. Engaged = every release decision fails closed. CLI-only so it works without `serve`; file-locked; no merge, deploy or run side effect. | `-state engaged\|disengaged`, `-by <who>`, `-reason <why>` |
| `factoryd reconcile` | Reaps a stranded isolation marker, worktree or branch from a killed run. Refuses if a live run holds the repo lock; preserves anything still resumable. | `-workspace <path>`, `-data-dir <dir>`, `-temporal-address` |

`factoryd doctor` reports the launchd service state as info. It fails only
when an installed plist points at a `factoryd` binary that no longer exists
(fix: `factoryd install-service -force`).

**`-data-dir` resolution.** The same on every command that takes the flag,
so a command means the same records from any directory:

| Order | Source |
|---|---|
| 1 | An explicit `-data-dir` |
| 2 | The session config's `data_dir`. `~/` is the home directory; a relative value is relative to the config file, not to the current directory |
| 3 | A session config that sets none: `~/buildgate/data` for a profile, `<config's directory>/data` for a config outside `~/.config/factoryd` |
| 4 | No session config at all: `data` in the current directory, with a warning |

A command that reads or writes records prints `data dir: ... (source: ...)`
first, and a `note:` when `data` in the current directory holds records it is
not using. `supervise` resolves it once and passes it to each daemon.

**`-config` resolution.** Same rule on every command that takes it
(`worker`, `submit`, `watch`, `status`, `cost`, `approve`, `reject`,
`retry`, `cancel`, `amend-scope`, `doctor`, `daemon`, `supervise`, `serve`,
`install-service`, `console`, `quickstart`): an explicit `-config` wins,
else `FACTORYD_PROFILE`, else the profile named in
`~/.config/factoryd/active-profile`, else the first of
`~/.config/factoryd/config.yml`, `~/.factory/config.yml` that exists. A
`-config` value with no path separator and no `.yml` suffix is a profile name
(`~/.config/factoryd/<name>.yml`); an active profile with no file is an error,
not a fallback. `doctor`, `daemon` and `serve` share
one resolver (`loadSettingsForConfig`); `supervise` forwards `-config` to
each `daemon` child it runs.

## Gotchas

| Gotcha | Detail |
|---|---|
| CI does not run on push | `ci.yml` is effectively manual (its push/schedule triggers are gated off unless a self-hosted runner is enabled). Run `make verify` (and `make live-smoke` for pipeline changes) yourself. |
| Autostart of dependencies | `worker` and a single-ticket run (Temporal, then the OpenShell gateway and meter when `meter_image` is set), `submit` (`worker` and `serve`), `console` (`serve`) and `doctor -fix` (Temporal, the gateway and meter) start what is missing, show a spinner while waiting, and say in one line why something could not start; Builds run only on Temporal, so a run whose Temporal cannot start halts with one line saying why. Temporal is started from the compose file embedded in the binary, written to `~/.config/factoryd/temporal/docker-compose.yml` (same `buildgate` compose project and volume as `make temporal-up`). When Docker is down and colima provides it, the Temporal start runs `colima start` first, for a VM that `stop -all` stopped or whose docker context is still `colima` after a reboot (log `~/.config/factoryd/temporal/colima-start.log`). `FACTORYD_AUTOSTART=0` turns all of it off, for scripts and CI, and an empty `-temporal-address` is then an error rather than auto-selecting a running Temporal. |
| Flags must precede positional args | Go's `flag` stops at the first non-flag. `factoryd submit <ws> -issue <url>` ignores `-issue`; write `factoryd submit -issue <url> <ws>`. Same for `reject -reason`, `retry`, `cancel`. |
| Project id is derived | Basename of the git repo containing the workspace (symlinks resolved): `~/code/payments` is `payments`. `status -project` and `kill-switch -project` filter on it. |
| Local models need `contextWindow` | Set the `models:` entry's `context_window` field. Without it the worker never compacts and a long run ends as "model route unreachable". `doctor` checks it. |
| One `worker` per data dir | A second fails: "a factoryd worker is already driving" (naming the pid from the heartbeat). Kernel `flock`; a crash leaves no stale lock. |
| Ctrl-C / SIGTERM on `worker` | Request steps are single saves, so nothing is lost mid-drafting; a mid-`building` ticket's run is picked up again. |
| Explicit `registry_proxy` needs explicit `-sandbox-image` | An explicit `-registry-proxy`/`-registry-proxy=false` flag or `registry_proxy` key without `-sandbox-image` fails: `-registry-proxy requires -sandbox-image`. Pass the digest `doctor` prints, or drop the key. |
| Registry-proxy caches are on disk | Go module/build caches live under `<data-dir>/scratch/<run-id>/<launch>` (one directory per worker container, removed when that container exits), not the sandbox tmpfs. |
| Every container starts with a cold Go build cache | Canonical verify, full suite and each gate get their own scratch dir, so none replays a cache the build phase wrote (no `(cached)` test results); modules come back through the registry proxy. Expect the first `go test` of each phase to compile from scratch. |
| Stale `factoryd` on `$PATH` | `doctor` runs or fixes a shadowing binary only in `$HOME/.local/bin`, `$GOPATH/bin` or a Homebrew prefix (elsewhere: `stat` only). `-fix` prints the `rm`/`mv`; add `FACTORYD_DOCTOR_APPLY_PATH_FIX=1` to rename it to `<path>.stale-<date>`. |
| `worker` start after a kill -9 | With the drain lock held, it reclaims every nonterminal run whose owner is gone: the pid in the run's `sandbox-owner.pid` no longer exists (a live pid, no marker, or a terminal run is never touched). Each is halted with the triage "the factoryd process running this build stopped mid-run", its Temporal workflow is terminated if still `Running` (unreachable Temporal: the run is left for the next start), and its worker, supervisor, registry-proxy and compose containers are removed; its ticket rebuilds as a new run. One log line per run. |
| Paths colima does not share | colima should share only `~/buildgate` read-write and your repositories read-only (USAGE.md, "Data directory and colima"), so a data dir or workspace anywhere else — `/tmp`, `~/.cache` — fails inside the sandbox ("does not appear to share ... into its containers"). `doctor -workspace <dir>` probes the workspace and data dir; `worker` and `factoryd <run>` preflights probe the data dir when it already exists. `doctor -fix` can repoint `data_dir` to `~/buildgate/data`. `mkdir -p` the data dir before a first bare `factoryd` run. On macOS `doctor` also warns (`Docker VM does not share all of $HOME`) while the VM still sees all of `$HOME`. |
| Correct work quarantined as all-"unavailable" | A slow/local model's conformity review can time out or return unparseable output; under `-conformity-policy required` (default) every criterion reads `unavailable` and blocks. If `agent_evidence.stopped_reason` lists every criterion as unmet, retry with `-conformity-policy advisory` (flag or `conformity_policy` key). The verdict is still recorded. |
| Configured image is out of date with its own source | `factoryd doctor`/`quickstart` compare each configured image's stamped build-inputs label against the checkout named by `image_source_root` (session config, written by `factoryd configure-images`) and warn (never fail) when it's stale; `quickstart` offers to rebuild, `worker`/`serve` print one warning line at startup. Run `make install` in that checkout to rebuild. No `image_source_root` recorded (or the checkout is unreadable): nothing is checked. |

## Request lifecycle

`submitted` -> `spec_drafting` -> `spec_review` (you) -> [`oracle_drafting`
-> `oracle_review` (you), only with `-draft-oracles`] -> `planning` ->
`plan_review` (you) -> `building` (one ticket at a time) -> `pr_review` ->
`done`. Off-ramps: `quarantined`, `halted`, `cancelled`. A step whose
`worker` stopped (drafting, planning, a build, a PR-review corrective round)
waits in `resume_review` (you): `factoryd resume`, `factoryd resume -from
scratch` or `factoryd cancel`; it returns to the state of the lost step.
Merge stays human, outside this graph.

**Staged oracles (opt-in, `submit -draft-oracles`).** `oracle_drafting`
runs `agent/pi/scripts/draft_acceptance_oracles.py` (see
[`agent/pi/README.md`](agent/pi/README.md)) against the approved spec's
acceptance criteria. At `oracle_review` you approve, edit files or
`RUN_COMMAND.txt`, reject with feedback, or approve an empty set to skip.
Approving after a failed draft carries a skipped-oracle warning forward.
Approved oracles are mounted read-only into every build and gate it
(`reference_oracle` gate plus a runtime canary). Unless `-no-commit-oracles`,
the host commits them with `.buildgate/oracles.json` and re-runs canonical
verify (`verify_after_oracle_commit`). Console review:
[`console/README.md`](console/README.md).

**Reminders.** A request waiting in `spec_review`/`plan_review`/`resume_review` is
reminded on entry, then every `-hitl-reminder-interval` (default `15m`,
min `1m`; key `hitl_reminder_interval`) until approved/rejected, via the
same channels a halted run's alert uses. Approving/rejecting stops
reminders immediately; a `worker` restart never re-sends one already
current (`request.json`'s `last_notified_at` is the source of truth, not
memory).

**Ticket sequencing (`building`).** Tickets build one at a time, chained
with `-prior-run <previous run id>`. A ticket's actual `-spec` is not
`tickets/NNN.spec.md` itself but a derived `tickets/NNN.build.md`: that
spec verbatim plus a `## Acceptance criteria this ticket must satisfy`
section carrying the full text of every approved-spec criterion the
ticket's own `### Acceptance criteria covered` numbers name (every
criterion, if it names none) — the builder used to see only those
covered-criteria NUMBERS and had to guess field/format/error-code names
from the ticket's own paraphrase; it now gets the approved spec's exact
wording. `plan_review`'s approval hash still pins `tickets/NNN.spec.md`
itself, never the derived file. `-open-pull-request` only opens a PR
when the run's `release.Decision` is `allowed: true`; an accepted run
with a denied decision still accepts, but no PR opens (the notification
says why). The bare `-release-*`/session-config zero defaults deny
everything, so a usable `-release-rollback-plan` and non-zero
`-release-max-files-changed`/`-release-max-insertions` are required — see
"Release policy" below. Per `-advance-on` (`accepted` default, or
`pr_approved`), the next ticket starts once the current one is `accepted`
(default) or once its PR is reviewer-approved. A ticket that quarantines
or halts moves the whole request to that state, naming the ticket
(`i/n`); `factoryd retry <id>` resumes at that ticket with a fresh run,
leaving earlier tickets' runs/PRs untouched.

A ticket that quarantines with its failed gates a subset of
`spec_conformity`/`code_review` (every other gate passed), where at least
one of those two carries something actionable (a flagged, non-"clean"
spec-conformity criterion, or a "high"-severity code-review finding),
first gets up to `-review-corrective-rounds` (default `1`, `0` disables;
key `review_corrective_rounds`; the prior key,
`conformity_corrective_rounds`, is refused outright, not silently ignored)
automatic corrective builds on its own branch before the request
quarantines: each round's addendum spec is the ticket spec plus a "Spec
conformity review to address" section (the flagged criteria and the
reviewer's own detail) when spec_conformity was flagged, and/or a "Code
review findings to address" section (each blocking finding's file:line,
severity, summary and failure scenario) when code_review was flagged,
rebuilt via `-on-branch`/`-diff-base` against the quarantined run's own
branch/base SHA, through every gate again including a fresh, independent
review. A code_review failure with no "high" finding (an unavailable
reviewer, or one that found nothing blocking) is NOT actionable and gets
no round -- the request quarantines as usual, with the quarantine naming
`code_review`. Accepted opens the PR and advances exactly like an ordinary
first-build acceptance; quarantined or halted again quarantines the
request as usual. A budget greater than `1` runs up to that many
*consecutive* rounds, not just one: a round that itself quarantines again
with the identical review-eligible shape starts another round from its own
flagged content, until one accepts, a round's own outcome stops being
review-eligible, or the budget runs out. Never triggered when any other
gate also failed, or the run halted rather than quarantined. Counted
separately from `-max-review-rounds`' own PR-review-round cap, so neither
budget can consume the other's rounds. The budget is per ticket and is not
reset by `factoryd retry`: a ticket that already used its rounds gets none
on a rebuild, because a human chose that retry.

**Corrective builds for other checks.** A ticket run quarantined by checks
other than the two reviews alone is followed by a corrective build when its
handoff sorts every judged failed check as `corrective` (see "What a stopped
run left behind" in USAGE.md for the bins). The build runs on the
quarantined run's branch (`-on-branch`/`-diff-base`) with the ticket's own
build spec, unchanged, and the handoff rendered as text as
`-earlier-attempt <file>`: a read-only input beside the spec that only the
build's first prompt receives, never a review. The file is kept in the
request's directory (`rounds/<ticket>-corrective<n>/earlier-attempt.md`), the
round's run is `<request>-<ticket>-corrective<n>`, and the round is recorded
on the ticket with kind `corrective`. `-review-corrective-rounds` is one
budget per ticket build for this round and the review round together; `0`
disables both. Not eligible: `tests_added` on a committed diff, a review that
gave no verdict, an unknown check, a repository gate the worker never ran, a
halt, and a run with no handoff or one that no longer matches its record.
When verification never passed and the attempt committed nothing, the checks
on its diff are not judged and the run is sorted on the others.

The build agent's own notes for the next attempt (the last section of
`earlier-attempt.md`, labelled as the agent's unverified view):

| | |
|---|---|
| Runs when | A build ends without passing, after at least one round in the process, its last turn finished cleanly (no timeout, exit 0, no error, the model route reachable), the harness can continue the session (Copilot: its session id was stored), no `--sonnet-fallback` or `--spec-acceptance-criteria`, and at least 300 seconds of the build's time budget are left (`FACTORY_BUILD_TIME_BUDGET_SECONDS`, the launch's timeout, set for the build launch only; the script measures its own elapsed time against it, not the container's clock) |
| Skipped when | Any of the above fails, or the turn itself fails (`notes_turn failed: <exception class>`, the partial file removed, the build unaffected); `BUILD_EVIDENCE.json`'s `notes_turn` says `ran`, `skipped_reason`, `duration_s` and the turn's usage |
| The turn | One reply in the build's own session, 180 s at most, asked for five fixed headings and no file change or command; `changed_files_during_notes` is set in `notes_turn` when the tree changed anyway |
| Caps | Reply cut to 12,000 bytes of UTF-8 in the session; at most 16 KiB retained; at most 8 items per heading, each one line of at most 300 characters, cleaned like every value from a build; the notes section of the record at most 3000 bytes, last, and the first thing the size cut drops |
| Stored | `.pi-build-session/handoff-notes.md` in the worktree, copied by the host to `agent-notes.md` in the run's directory before the session folder is removed, and parsed into `agent_notes` in `handoff.json` |
| Readable by | The operator (`GET /runs/{id}/handoff`) and a later build of the same ticket (its first prompt). Not a review, a planner, a pull request, a notification, the progress feed, a build log, `run.json` or an MCP tool |

**Saved prompts.** The text of each prompt a script hands to a coding agent,
for the operator to read:

| | |
|---|---|
| Saved by | `agent/pi/scripts/saved_prompts.py`'s `save_prompt(session_dir, name, text)`, at the one place each script hands a prompt to the harness. It never raises and never changes the turn; a failure is one progress note naming the exception class |
| Names | `build-round-<n>`, `build-notes`, `build-sonnet-fallback` (the build); `review-conformity`, `review-code`, `review-combined`; `draft-spec`, `draft-spec-example-check`, `draft-plan`, `draft-oracle-c<nnn>`. A name saved twice in one session (a relaunch) gets `-2`, `-3`. Not covered: `goal_pilot.py`, which `factoryd intake` runs outside a run's or request's directory |
| Caps | 2 MiB per prompt (cut, with a last line saying how many bytes), 50 per launch; the host enforces both again, takes only regular files named `[a-z0-9-]{1,64}.md`, and follows no link |
| Stored | `<session folder>/prompts/<name>.md` in the worktree, copied by the host (through `sanitize.Text`: escapes, control characters and recognisable credentials removed) to `prompts/<launch>-<n>/<name>.md` in the run's directory (`<launch>` is `build`, `spec_conformity`, `code_review` or `review`; `<n>` the Temporal attempt), or in the request's directory (`spec-<n>`, `plan-<n>`, `oracle-<n>`) for a drafting job, then removed from the worktree before any later launch. No field of `run.json` names them |
| Readable by | The operator: `factoryd logs -list` / `-prompt`, `GET /runs/{id}/prompts` (name, attempt, bytes, time) and `GET /runs/{id}/prompts/{attempt}/{name}` (`text/plain`), gated like `GET /runs/{id}`, and the run page's **Prompts sent**. Not a model, a review, an MCP tool, a pull request, a notification, the progress feed or `run.json`. A prompt may quote repository files, failing output and the record of an earlier attempt |

**A retry's rebuild.** `factoryd retry <id>` rebuilds the quarantined ticket
as a fresh run with every gate again. Where it starts and what it is told:

| The ticket's last run | `retry` (`-from attempt`, the default) | `retry -from scratch` |
|---|---|---|
| Quarantined, with a handoff a build may be given (the corrective round's rule), built from the ticket's own spec as it is now, and it committed something | On that run's branch, from its commit (`-on-branch`, with the run's diff base); given the record (`rounds/<ticket>-retry/earlier-attempt.md`), which opens by saying the attempt's commit is in the workspace | From the base commit; given the record, which opens by saying the workspace starts from the base |
| The same, but it committed nothing (verification never passed) or recorded no branch | From the base commit; given the record | The same |
| Anything else: a failure a build is never told about (`tests_added`, a review with no verdict), a spec changed since (`amend-scope`, an edit), a review corrective round (it was built from the spec plus the reviewers' findings, not the ticket's own spec), an accepted run whose pull request the release policy refused, a halted run | From the base commit, no record | The same |

The gates and the release decision of a rebuild that continues judge the
ticket's whole change, measured from the diff base, so nothing in the commit
it continues from escapes them. A build started by `factoryd resume -from
scratch` always starts from the base. A lost rebuild that was continuing on a
branch cannot be resumed in its worktree (only `resume -from scratch`).

A retry is not a corrective round: it uses none of that budget and records no
round. If the rebuild cannot start on the quarantined run's branch (the branch
is gone, or the repository stays busy), the request halts saying so;
`retry -from scratch` rebuilds from the base.

A build that was given a record and did not finish passes it on. Its run
names the quarantined run the record is of (`earlier_attempt_of` in
`run.json`), and the build that follows it is given that run's record again,
under the same checks:

| The unfinished build | What follows it | Its record |
|---|---|---|
| Lost, worktree kept (a retry's rebuild from the base; the worktree of a corrective round, which runs on an existing branch, is never kept) | `factoryd resume` in that worktree | `rounds/<ticket>-resume/earlier-attempt.md`; opens by saying the record is of the attempt before the interrupted build, and whether the interrupted build ran on that attempt's branch |
| Lost or halted | `factoryd resume -from scratch` or `factoryd retry` | `rounds/<ticket>-retry/earlier-attempt.md`, as for any retry |

**`-on-branch`/`-diff-base`.** Both travel in the workflow input
(`RunWorkflowInput.OnBranch`/`DiffBaseSHA`): a corrective
round checks out the quarantined run's own existing branch
(`wsisolation.PrepareOnBranch`) and gates the cumulative diff from
`-diff-base`, for every ticket in a
multi-ticket request. The review corrective round and the PR-review
corrective round (`runCorrectiveRound`) both rely on this.
`-instruction-base <sha>` (`RunWorkflowInput.InstructionBaseSHA`, recorded on
every run as its `instruction_base_sha`) names the commit whose instruction
files the run's reviews read. The request driver passes it to every build that
follows an earlier run of the request (first build of ticket 2 onward, retry,
corrective round, PR-review round, resume): ticket 1's recorded value, or for
ticket 1 the value its own earlier run recorded, so an earlier build's
unmerged instruction text is not trusted. When that run cannot be loaded or
records no base, the request halts instead of building. It is a full
40-character object id and an ancestor of the run's base. Default: a resumed
run's lost run's value, else the run's diff base, else its base.

**PR review (`pr_review`).** `worker`'s poll loop
(`internal/requestdriver/pr_review_driver.go`) checks each ticket's PR at most once
per `-pr-poll-interval` (`5m` default, min `1m`):

- An unresolved thread from a `-pr-trusted-authors` login (comma-separated
  allow-list; default empty = nothing triggers a round; the factory's own
  account and `[bot]` logins are always excluded; `-pr-ignore-authors`
  wins over trusted) becomes a corrective round against the ticket's
  existing branch (never a fresh worktree). Accepted: pushes the branch,
  replies "Addressed in <sha>." on each thread. Quarantined/halted: pushes
  nothing, stays in `pr_review`, one notification, and replies on each
  thread: the round's number, that nothing was pushed, the gates that
  failed, what the review flagged, and whether another round follows.
  Capped at `-max-review-rounds` (default `3`; key `max_review_rounds`),
  then halts naming the ticket.
- A thread is judged by its latest comment the factory's account did not
  write. The factory's replies never resolve a thread and never clear it:
  an answered thread blocks the ready flip until a person resolves it, and
  starts no further round unless the reviewer comments again.
- A round passes every gate the ticket's first build passes, judged on the
  whole pull request diff: verify, the full suite, the diff-shape gates,
  spec conformity against the ticket's acceptance criteria, and code review.
- A round the review gate alone quarantines (only `spec_conformity` and/or
  `code_review` failed, with a flagged criterion or a `high` finding) gets up
  to `review_corrective_rounds` fix attempts (default `1`, `0` disables) on
  the same branch before it ends, each told what the gate flagged in the
  attempt before it. The last attempt decides the round, and the round uses
  one slot of `max_review_rounds` however many attempts it took. The request
  page links each earlier attempt's run under the round.
- A quarantined attempt's commits stay on the local branch, unpushed, and
  the next attempt or round builds on them. Each one's ticket is the
  ticket's own spec plus:

  | Section | Content |
  |---|---|
  | `## Reviewer comments to address` | Each open trusted thread: path, line, author, body |
  | `## Why the previous attempt was not pushed` | Only after an attempt the review gate quarantined: that the earlier attempt is on the branch and was refused |
  | `## Spec conformity review to address` | That attempt's flagged acceptance criteria, with the reviewer's detail |
  | `## Code review findings to address` | That attempt's blocking (`high`) findings: location, summary, failure scenario |
- No new threads, still a draft, checks passing, no unresolved thread
  from any non-bot/non-self author (trusted or not — an untrusted human's
  open comment still blocks readiness, it just can't trigger a round),
  decision still allowed: `gh pr ready` is called once.
- Reviewer `APPROVED`: ticket marked `approved`, sequencing moves on.
  PR merged: ticket marked `merged`; the request reaches `done` once
  every ticket has merged. Closed without merging: request halts, naming
  the PR. The factory never merges.

Every poll that finds no new thread also checks the pull request against
the ready-to-merge bar ([STATUS.md § Ready to merge](STATUS.md#ready-to-merge))
and records the result as the ticket's `merge_readiness` (`ready`,
`checked_at`, `head_sha`, `blockers`). Nothing the factory does depends on
it. It is cleared while a corrective round runs and once the pull request
has merged.

`factoryd status` appends each `pr_review` request's per-ticket PR state
(`tickets: 1:approved 2:open`; `1:ready,ready-to-merge` once the bar is
met). The request's next step, in the console's
"Next" line and in `GET /requests`' `next_action`, says what each open pull
request waits on:

| PR state | Next step named |
|---|---|
| `ready` | Review it: approve and merge, or leave review comments for a corrective round |
| `approved` | Merge it; the factory never merges |
| `stacked` | Merge the earlier ticket's pull request first |
| `draft` | Nothing yet: the factory marks it ready once checks pass and no thread is open |
| `ready` or `approved`, bar met | Merge it: ready to merge; the factory never merges |
| `ready` or `approved`, bar not met | That it is not ready to merge, and each thing it lacks |
| `ready`, last corrective round not accepted | That the round pushed nothing and why; the open thread starts another round on the next poll, up to `max_review_rounds` |

The request page lists each ticket's corrective rounds (the one under way, then each that ended, with its outcome, cause and run); `GET /requests/{id}` names a round under way as the ticket's `active_round_run_id`.

The same two verbs are on HTTP: `POST
/requests/{id}/approve`, `POST /requests/{id}/reject`, gated by
`Authorization: Bearer <token>` — same class of write as `/runs/{id}/override`.

`POST /requests/{id}/reject` body:

| Field | Meaning |
|---|---|
| `reason` | Free text for the redraft. Required unless `anchors` has an entry |
| `by` | Who rejected; recorded on the rejection |
| `to` | `plan` or `spec`: send a quarantined or halted request back instead |
| `anchors` | Notes tied to places in the reviewed files: `[{"path": "spec.md", "section": "## Acceptance criteria", "item": 2, "note": "..."}]`. `section` and `item` are optional; each field is put on one line; at most 50. The rejection's reason becomes one line per anchor (`- spec.md, ## Acceptance criteria, number 2: ...`) followed by `reason`, and that text is what the redraft reads. Not accepted with `to` |

## A retried draft

`factoryd retry` on a request halted while drafting its spec or plan runs
that draft again. What the next draft is told:

| The halt | The next draft is told |
|---|---|
| The factory's own checks refused the drafted spec or plan (the spec skeleton, the plan checks) | The factory's reason: one line of at most 2000 bytes, no log path, in a section headed `Previous draft refused by the factory`, after your own feedback in the stage's feedback file. Kept until a draft of that stage reaches review or you send the request back |
| The draft job failed or timed out (model route, credential, timeout) | Nothing: there is no draft to correct |
| An infrastructure halt (stale approval hash, missing verify command, budget, oracle materialization) | Nothing |
| A spec or plan you handed over | Nothing: a retry never sends your document to the model (see below) |

## Handing over a finished spec and plan (`submit -spec-file`, `-plan-dir`)

```text
factoryd submit -spec-file my-spec.md <repo>
  └─ refused unless the file passes the spec skeleton check, naming the missing heading
submitted -> spec_drafting   (no model call: your file becomes spec.md)
          -> spec_review     approve, or reject -reason "..."
                               └─ reject: the planning model revises YOUR document with the feedback,
                                  changing only what the feedback asks; back to spec_review

factoryd submit -spec-file my-spec.md -plan-dir my-tickets/ <repo>
  └─ refused unless the tickets are 001.spec.md, 002.spec.md, ... with the ticket headings,
     and together cover every acceptance criterion of the spec
... -> spec_review -> planning   (no model call: your tickets get a drafted plan's checks)
                   -> plan_review  approve, or reject -reason "..."
                                     └─ reject: the planning model revises YOUR tickets with the feedback
```

| Point | Detail |
|---|---|
| Format | The skeleton a drafted spec has: `# Spec`, `## Problem`, `## Scope`, `## Non-goals`, `## Affected services and packages`, `## Acceptance criteria` (a numbered list), `## Risks`, `## Open questions`, in that order, each heading on its own line |
| Request text | Optional with `-spec-file`: the spec's `## Problem` text is used. A request text, `-request-file` or `-issue` may still be given |
| Review | The same `spec_review` gate and approval hash as a drafted spec. Planning, oracles and builds read it exactly like a drafted one |
| After a reject | The request's spec is from then on a drafted document: later rejects revise the latest version, and the drafting cost shows in `factoryd cost` |
| Edited in the console | At `spec_review` and `plan_review` the console's Edit saves the file in place. Each save is recorded on the request: who, when, the changed lines, and the replaced text as a revision (the request page's Audit lists them). If you then reject that stage, the changed lines go to the planning model with your feedback, marked to be kept; an edit you approve reaches no model, since nothing redrafts the file |
| Not in the skeleton | Hand the document over as the request (`-request-file`) and review the spec Buildgate drafts from it |
| Ticket format | Header lines `Verify-Command:` (the request's verify command, exactly), `Allowed-Files:`, `Required-Changed-Files:` (with at least one test file, or a `Tests-Required: no -- <reason>` line), then `## Goal`, `## Plan` with `### Files to touch`, `### Steps`, `### Tests to add`, `### Acceptance criteria covered` (criterion numbers, one per list item), `## Out of scope` A file the ticket moves or renames is a change to both paths: list the old path and the new one in `Allowed-Files:` (the changed-file list names both, for `diff_scope`, required files and protected paths alike) |
| Plan checks | Names, headings and criteria coverage at submit; at planning, every check a drafted plan gets (verify command, allowed files, the tests rule, criterion feasibility). A plan that fails one halts the request with the reason, or, when the factory judges it infeasible, is revised by the planning model with that reason as feedback |
| `-plan-dir` alone | Refused: tickets name the criteria of the spec they plan |
| A halted hand-over | `factoryd retry` takes the same document again and halts the same way. Send it back with a reason instead (`factoryd reject -to plan -reason "..." <id>`, or `-to spec`): the model then revises your document to fix it. Or `cancel`, correct the file and submit again |
| Spec changed after hand-over | If you reject the spec and the revision changes its criteria, your tickets are still taken on the first planning pass and must still cover them. Sending a request back to spec after a plan exists discards the plan's revisions and its feedback: the tickets as you handed them over are taken again |
| Team design guide | Not applied to a handed-over spec or plan on its first pass; a revision after a reject reads it. To have your document checked against the guide, reject it with that as the reason (`reject -reason "check this against the team design guide"`) |

## Oracle drafting internals

Applies to `submit -draft-oracles`; see [USAGE.md](USAGE.md#oracles-submit--draft-oracles)
for when to use it.

**Step by step.** (1) Approve the spec. (2) A sandboxed drafter reads the
spec's acceptance criteria and the repo at the base commit (never the new
implementation) and writes Go/Python tests plus a `MANIFEST.json` mapping
each criterion to a file and `target_path`; judgement criteria are
skipped. Drafting is per criterion — one bounded pi pass each, strictly
sequential, so one hard criterion's timeout doesn't cost the others.
`oracle_draft_job.go` splits the overall `-draft-oracles-timeout-minutes`
budget across the criteria found and passes it to
`draft_acceptance_oracles.py --criterion-timeout-minutes` explicitly, so
the container deadline and the per-criterion budget always agree. A
criterion whose pass wrote no manifest entry is salvaged from the model's
final response text rather than discarded (`salvaged_count` in the
draft's evidence). (3) At `oracle_review`, read every file, edit
`RUN_COMMAND.txt` if needed, approve (hash-pinned) or reject with
feedback. (4) Planning copies the oracle files per ticket. (5) The build
mounts them read-only at `.oracle`; a runtime canary checks the tests ran,
a failing oracle blocks acceptance and feeds the in-loop retries. (6)
After acceptance, the tests and `.buildgate/oracles.json` are committed
unless `-no-commit-oracles`.

**Drafter self-checks**, run after a draft is installed, before you see
it (none runs the model's code, none can approve or refuse an oracle):

- *Compile self-check* (Go). Type-checked in process (stdlib from GOROOT
  source; no `go` command). Every name the change may add ("undefined:
  X") and every non-stdlib import is ignored, so only errors independent
  of the target are reported. Catches "this file can never compile", not
  a signature mismatch. On any problem the drafter re-runs once with the
  compiler output appended to feedback (`oracle_draft.redraft.log`); if
  that also fails, the first draft is kept, flagged, no third pass. The
  parse/package-clause check is separate and still blocks approval.
- *Spec-example check.* Heuristic: a backtick/quoted literal in a
  criterion clause that says accepted/rejected is looked up among the
  oracle files' string literals; a warning (`spec_warnings`) fires when
  the nearest test context says the opposite. Warn-only, can be wrong
  both ways, can't see prose examples or helper-asserted outcomes.
- *Generated `RUN_COMMAND.txt`.* For 2+ Go oracle files in one module,
  the host writes one (one `Replace` entry per file, `-run TestOracle`),
  built from validated names, never the model — only when none exists or
  the existing one is byte-identical to a prior generated one. A file you
  wrote or edited is never replaced.

**Limits.** Go and Python only (other languages: `none_eligible`,
hand-placed oracles still work); at most 15 oracle files and 64 KiB per
ticket. A timed-out draft keeps files already written if `MANIFEST.json`
existed first; otherwise the draft fails and you redraft. `factoryd
retry` returns a materialization halt to `oracle_review`.

**Triage hint on quarantine.** When `spec_conformity` fails and this
run's `reference_oracle` gate shows the build *passed* the approved
oracle, the triage sentence names the oracle file(s) covering the
flagged criteria as a possible cause — a hedged hint from durable gate
evidence, never a guess.

## Session config

`factoryd init-config` writes a commented scaffold. Keys mirror flag names
with underscores. Precedence: explicit flag > config key > built-in
default. An unknown key fails at startup naming it. `worker` with no
config file and no sandbox flags refuses to start.

A running worker reads its config file once, at start. Restart it
(`factoryd restart`) to apply a change.

**Profiles.** `~/.config/factoryd/<name>.yml` is profile `<name>` (`config.yml`
is `default`; other files, e.g. `x.yml.bak-…`, are not profiles).

| Item | Meaning |
|---|---|
| `active-profile` | One line in `~/.config/factoryd/` naming the active profile; written by `factoryd use <name>` |
| `FACTORYD_PROFILE` | Profile name overriding `active-profile` for one process tree; `-config` still wins |
| `factoryd configure-images -all-profiles` | Writes the image refs into every profile (what `make install` does when `FACTORYD_CONFIG` is empty); exclusive with `-config` |
| `factoryd configure-images -sandbox-image <ref> -meter-image <ref> -registry-proxy-image <ref>` | Writes the digest-pinned refs into one session config (`-config`, default the active profile); a ref left out keeps its existing value. `make install` calls it |

Local OpenAI-compatible model example:

```yaml
routes:
  local:
    upstream: http://<model host>:8080   # bare API root, no /v1
    allowed_path_prefix: /v1
    worker_base_path: /v1
    allow_plaintext_upstream: true
    allow_no_credential: true
models:
  local-model:
    id: <id from GET /v1/models; never hardcode>
    routes: [local]
    context_window: 131072
roles:
  planning: { model: local-model }
  execution: { model: local-model }
  review: { model: local-model, allow_shared_model: true }
# registry_proxy: true   # needs -sandbox-image too (see gotchas)
```

`registry_proxy_image` has no built-in default: `make
install` records it in the config (`factoryd configure-images`), or pass
the flag; `factoryd doctor -fix` builds it when absent.
`meter_image` (buildgate's own meter, `make meter-image`) is set the same way
by `make install` (`-meter-image`). The OpenShell gateway stack reads it:
every build launches through that gateway. `factoryd doctor` warns when it
is unset (fix: `make install`) and, once set, warns on any of its four
OpenShell rows (images, gateway, gateway config, meter) that fails;
`doctor -fix`, `worker` and a single-ticket run start the stack
([USAGE.md](USAGE.md#the-sandbox-runtime-openshell-gateway-and-meter)).

### Corporate TLS interception

```yaml
egress_ca_bundle: /path/to/corp-ca.pem
```

Trusted by the registry proxy for its outbound HTTPS (package
registries) and by factoryd's own host-side model-listing and Copilot
token-exchange calls. Also a flag (`-egress-ca-bundle`) on
`factoryd <run>`, `worker`, `doctor`, `daemon`, `serve` and
`quickstart`. `doctor`/`worker` fail at startup if the file is not a
readable PEM. `quickstart -egress-ca-bundle` resolves it to an absolute
path and loads it before writing it to config (it also uses it for its own
Copilot model listing).

Unset, on a network that re-signs TLS, it is `~/.config/factoryd/build-ca.pem`:
the bundle `make install` and `doctor -fix` write from the keychain. Set the
key only to trust a different file.

A model call from a sandbox does not use this key. Its TLS connection is
opened by the sandbox's supervisor, which always takes the machine's bundle
(one gateway serves every profile):

| On a network that | The supervisor runs from | It trusts |
|---|---|---|
| does not re-sign TLS | OpenShell's pinned image | Its built-in public roots |
| re-signs TLS | `buildgate-openshell-supervisor:ca-<hash>`, built by the stack start from the pinned image | Also `~/.config/factoryd/build-ca.pem`, which `make install` and `doctor -fix` write from the keychain |

A proxy whose CA is not in the keychain: write that CA (PEM, with any
public roots the proxy does not replace) to `~/.config/factoryd/build-ca.pem`,
then `factoryd doctor -fix`.

## `.factory.yml` reference

Committed at the repo's git top level; `internal/projectconfig.Load`
reads it and fills in any flag the caller left unset (an explicit flag
always wins). `factoryd init -write-factory-yml` / `factoryd onboard
-write-factory-yml` generate a starting one (detects a real verify
command; always writes `preflight_profile: brownfield`); both run the
`doctor` preflight first and refuse to write on failure (`-skip-doctor`
bypasses). Read from committed `HEAD` only:
an uncommitted edit is ignored (logged), so a run can never widen its own
defaults mid-run; `.factory.yml` is always a protected path, and so is
everything under `.factory/` (any letter case): a run that changes anything
there is refused at release, and an oracle manifest `target_path` under it is
refused.

```yaml
verify_command: "make verify"          # canonical check, same as -verify-command
fast_check_command: "make fast-check"  # cheap check run before verify_command each round
# lint_command: "golangci-lint run ./..."           # optional named gate "lint"
# security_command: "govulncheck ./..."             # optional named gate "security_audit"
# unit_test_command: "go test ./..."                # optional named gate "unit_tests"
# integration_test_command: "go test -tags=integration ./..."  # optional named gate "integration_tests"
# reference_oracle_command: ""                       # optional named gate "reference_oracle" -- not agent-authored, see below
# gates:                                             # gates this repository defines for itself; see "Repo-defined gates"
#   - id: no_todo                                    # recorded as "repo-no_todo"
#     command: "! grep -rn TODO src"
# setup:                                            # commands for the repository; see "Setup and autofix commands"
#   - "npm ci"
# autofix:                                         # formatters run inside each build round; see "Setup and autofix commands"
#   - "gofmt -w ."
# test_patterns:                                     # glob patterns for the always-on "tests_added" gate
#   - "*_test.go"
#   - "*.spec.ts"
preflight_profile: brownfield          # "" (strict, default) or "brownfield"
protected_paths:                       # same as session config's release_protected_paths
  - "go.mod"
  - "README.md"
token_ceiling: 5000000                 # same as meter_token_ceiling; may only lower the session's effective ceiling, never raise it (a higher value refuses the run/submission)
cost_ceiling_micro_usd: 25000000       # same as meter_cost_ceiling_micro_usd; may only lower the session's effective ceiling, never raise it (a higher value refuses the run/submission)
# design_guide: go-service             # team design guide for spec drafting and planning; see "Design guide"
```

**Repo-defined gates.** `gates:` lists further command gates, for a check
none of the five named gates fits (a license check, a migration check, a
size budget). No buildgate change or release is needed to add one.

| | |
|---|---|
| Entry | `id` (`[a-z0-9_]`, at most 32 characters, unique) and `command` (a shell command run from the workspace root). At most 16 entries; any other key is refused |
| Name | `repo-<id>`, in the run record, the evidence and the draft PR body |
| When | After canonical verify (and the full suite, when it runs) passes and after the five named gates, in name order, whether or not an earlier gate failed; and again after an oracle commit, against the committed tree |
| Result | Passing adds a `pass` line. Failing quarantines the run naming the gate. A gate can only add a denial: it cannot pass another gate or be made a required gate of the release policy |
| Where it runs | The sandbox only, with no network, like `verify_command` |
| Source | The committed `.factory.yml` only. There is no flag, and a run cannot change the file it is judged by |
| Cost | One sandbox launch per gate per run, two with an oracle commit; with Compose services, each launch brings the services up and down. `doctor` does not check a repo gate's executable against the image, as it does for the five named gates |
| A gate that did not run | An accepted run with no result for one of the repository's gates is quarantined naming it (a long-lived Worker older than this `factoryd`): `factoryd restart` |

**Setup and autofix commands.** `setup:` and `autofix:` each list shell
commands for the repository. `setup:` runs in every build, verify and gate
sandbox; `autofix:` runs inside each build round only.

| | |
|---|---|
| Keys | `setup`, `autofix` |
| Shape | List of strings, one command per entry |
| Limits | At most 8 entries per key, 2000 bytes per entry, one line each (no newline, carriage return or NUL byte), none blank |
| Source | The committed `.factory.yml` only. There is no flag, and a run cannot change the file it is read from |
| Status | Both run, as below. Both are recorded on the run (`project_config_sha256`, and `project_config_commit_sha`: the commit the file was read from) |

Where `setup:` runs, in the order listed, each command by `sh -c` in the
workspace; the command of the step follows only when all passed:

| | |
|---|---|
| Runs before | The baseline verify; the build (by the build script: once before the first agent turn, and again before each round's fast check and verify, output in `setup.log`, each command with a 10-minute limit); canonical verify; the full suite; each named and repo gate; the oracle canary; the reruns after an oracle commit |
| Never runs in | Review sandboxes (they hold a model route); drafting and planning jobs |
| A failure | The step's own check fails (exit 95, `buildgate: setup failed: <command>` in its log). On the base commit the run halts before any model call: `setup fails on the base commit: <command>`. In a build round it fails the round (`setup command failed: <command>`), skips that round's verify and goes to the next round as feedback. A setup command that fails before the first agent turn ends the build without a model call (`setup command failed: <command>`). In the build a setup command gets 10 minutes |
| Cost | It runs once per sandbox: a run with N gates runs it at least N+1 more times. Nothing is cached between sandboxes |
| Network | Whatever the step already has: none for verify and gates beyond the registry proxy and Compose sidecars; in the build, the model route. `setup:` is given to no planner or reviewer; a setup command that fails inside a build round is named, with its output, to that build's own agent |
| Background processes | A setup command that times out is stopped with everything in its process group. One that returns leaves what it started in the background running for that step. Do not rely on that for services: declare them as Compose services, which every step that needs them gets |
| Files it writes | Must be gitignored: untracked files are committed with the build and judged by `diff_scope` |
| A step that did not run it | An accepted run whose canonical verify attempt does not record the digest of the commands is quarantined as the operator's (a long-lived Worker older than this `factoryd`): `factoryd restart` |
| Entries | One may not begin with `-` (the shell would read it as an option) |

`autofix:` (a formatter or a `--fix` linter) runs in the build only:

| | |
|---|---|
| Where it runs | Inside each build round, after the agent's turn and before that round's checks (by the build script, in the order listed, each command by `sh -c` in the workspace), so the checks judge the fixed tree. Never after the build, and never in a verify, gate or review sandbox |
| A failure | Advisory: a non-zero exit or a timeout is recorded and never fails the round (many `--fix` tools exit non-zero when they fixed something). The next round's feedback carries one line, `autofix command failed (advisory): <command> exit <n>`, only when a command failed or timed out. Two cases do fail the round, because the tree may hold edits outside the ticket's files that nobody can put back: its changes could not be checked (`autofix ran but its changes could not be checked against the ticket's files`), and a revert failed (`autofix changed files outside the ticket's and could not revert: <paths>`). When the files changed before autofix cannot be listed, autofix does not run that round (`skipped`) |
| Scope | It may change only files the ticket's build has changed so far (paths that differ from the commit the build started from, or are untracked and not ignored, before autofix ran). Any other path it changed is restored to that commit's content (a file it lacks is deleted) and listed; a submodule or directory entry is never rewritten. Its edits never count as the agent's work: a round where only autofix changed files is still "no changes". Each command runs in its own process group, which is stopped when the command returns or times out, so background work cannot write after the check; a process that detaches into its own session is not stopped and ends with the build's container |
| Evidence | `autofix` on each round of the build evidence: per command `command` (first 200 characters), `exit_code`, `timed_out`, `duration_s`, and `reverted_count` with the first 20 `reverted` paths (only paths actually restored or deleted), `revert_failed_count` with the first 20 `revert_failed` paths, `scope_check_failed`, and `skipped` when it did not run. The output and any revert are in `autofix.log` in the round's feedback folder |
| Limits | 8 entries, 2000 bytes and one line each, 5 minutes per command: up to 40 minutes a round, inside the build's own time limit |

**The `.factory/` directory.** Scripts the commands above call
(`lint_command: sh .factory/lint.sh`) go in `.factory/` at the repository
root. Every sandbox that runs a repository command sees that directory as a
trusted commit holds it, read-only, so a build cannot change the script a gate
judges it with.

| | |
|---|---|
| What it is | The directory `.factory/` at the repository root, committed. Regular files and subdirectories only: a symlink or a submodule in it, two names that differ only by letter case, more than 2,000 files, a file over 4 MiB or 16 MiB in all is refused |
| Which commit | The commit `.factory.yml` was read from: `HEAD` of your checkout when the run was dispatched (`project_config_commit_sha` on the run), also for a repository with no `.factory.yml` |
| Mounted read-only in | The baseline verify, the build, canonical verify, the full suite, each named and repo gate, the oracle canary and the reruns after an oracle commit, at `/workspace/.factory`. A new snapshot is taken from git objects before each launch |
| Not mounted in | Review sandboxes; drafting and planning jobs (they run no repository command) |
| The commit has no `.factory/` and the worktree does | An empty read-only directory is mounted over it |
| What now fails | A command that writes into `.factory/`: a cache or report written there, `chmod +x .factory/*`. Commit the executable bit (`git update-index --chmod=+x`) and write output elsewhere (a gitignored directory, `/tmp`) |
| A build that changes it | Its edit is not what the gates run, and the result is refused at release: `.factory/` is a protected path |
| Evidence | `factory_dir_sha256` and `factory_dir_commit` on every attempt that had the mount. The hash is the same for every attempt that mounted the commit's directory. An attempt records none when nothing was mounted (a review attempt, or neither the commit nor the worktree had the directory at that launch) and the empty snapshot's hash when an empty directory was mounted over a `.factory/` only the worktree has, so a run whose build creates the directory has both |
| Not covered | Files outside `.factory/` that a script there calls (`sh .factory/lint.sh` running `scripts/check.py`, or `verify_command: make test`): those run as the build left them |

| Halts the run before the sandbox starts (`halt_reason_code` `factory_dir_failed`) | What to change |
|---|---|
| The commit has `.factory/` and the run's worktree has no such directory (the run builds on a branch or commit older than the one that added it, or a build deleted it) | Rebase the branch onto the commit that has `.factory/`, or start the request again from it |
| `.factory` in the worktree is a file or a symlink, or the worktree root holds another spelling (`.Factory`) | Make it one real directory named `.factory` in the repository |
| `.factory/` at the commit holds a symlink, a submodule, names that differ only by case, or exceeds a limit above | Replace the link with the file, move the submodule, keep one spelling, or move large files out |

The halt is not retried and is not an infrastructure failure; a snapshot that
could not be taken for another reason (git could not run, the worker was
stopping, the run's directory could not be written) is one, and is retried
like any other. `factoryd
status` quotes the reason, which names the path; it is on the refused attempt
as `factory_dir_error`. On the baseline verify, the first sandbox of a run,
this stops the run before any model call.

**Named gates.** `lint_command`/`security_command`/`unit_test_command`/
`integration_test_command`/`reference_oracle_command` (flags:
`-lint-command`/`-security-command`/`-unit-test-command`/
`-integration-test-command`/`-reference-oracle-command`) each declare one
optional gate (`lint`/`security_audit`/`unit_tests`/`integration_tests`/
`reference_oracle`). Each configured command runs in the sandbox after
canonical verify passes and becomes its own
gate with its own exit code/log/log-SHA-256 — mirroring
`full_suite_verify`. Unset = "not configured", never blocks; a failing
one quarantines the run naming that gate, same as `canonical_verify`. The
draft PR body lists `pass`/`FAIL`/`not configured` per gate. `doctor`
checks each configured command's leading executable exists in the
sandbox image. These five gates, their flags, and their `.factory.yml`
keys are one compiled-in table (`internal/policy.CommandGates`) rather
than five independently-maintained lists — adding a 6th command gate
touches that table, its YAML key (`projectconfig.Config.GateCommands`),
and this doc, not the run loop or the Temporal wiring.

`reference_oracle_command` should run a check the agent didn't author — a
script outside `Allowed-Files` that diffs the candidate's output against
an independent oracle (published vectors, a reference implementation).
That's enforced only on the final diff (`diff_scope`); for structural
protection, pass `-reference-oracle-dir <host dir outside -workspace>`
with `-reference-oracle-mount-path` on `factoryd <run>` to bind-mount a
pristine read-only host copy over that path for the gate's own container
run.

`test_patterns` bounds the always-on `tests_added` gate: passes when a
changed file matches one of these globs (full path or basename), or the
ticket declares `Tests-Required: no -- <reason>` in spec.md (recorded on
the release decision). Unset falls back to
`policy.DefaultTestPatterns` (Go/Python/JS/TS/Ruby/Java/C#/Rust); this
gate always runs. Plan drafting rejects a ticket up front, before it ever
reaches `plan_review`, if its own `Allowed-Files` could never satisfy
`tests_added` and it declares no `Tests-Required` opt-out (found live,
example-app run 3, 2026-09-28: a plan reached approval and a build round ran
before quarantining on a `tests_added` failure its own `Allowed-Files`
made structurally unavoidable). A second plan-time check runs alongside
it: for every approved-spec acceptance criterion, it collects the
repo-relative file paths the criterion names in backticks and checks
them against the union of `Allowed-Files` of every ticket that lists the
criterion as covered, rejecting the plan if a named path is owned by no
covering ticket (found live, example-app habit-insights request, 2026-09-28: a criterion
naming three tickets' own files was listed as covered by only one of
them, whose `Allowed-Files` could never satisfy the part of the
criterion naming the other two). A criterion covered by no ticket at all
is left to the existing "unclaimed criterion" check, not this one. On
either rejection the request driver re-plans automatically, once: it
records a factory-authored rejection, feeds every infeasibility found
(both classes, in one message) back to the planner as plan_review-style
feedback, and re-launches planning before the request ever reaches a
human. If the redrafted plan is still infeasible by either check, the
request halts exactly as before -- one automatic retry, never an
unbounded loop.

Real correctness coverage is `-conformity-policy`'s
per-criterion review (default `required`, no `.factory.yml` key) plus the
deterministic gates and the human gates. Formatting drift never reaches
the conformity reviewer:
`build_app.py` runs `gofmt -w` on changed `.go` files once a round's
verify passes, and the reviewer prompt excludes pure formatting/lint
drift from spec-conformity findings.

`-code-review-policy` (`off`/`advisory`/`required`, default `off`; session
config: `code_review_policy`) is the standalone AI code-review pass
(`agent/pi/scripts/code_review.py`) -- a free-form review of the diff for
concrete correctness/security/data-loss/concurrency defects, distinct
from `-conformity-policy`'s own check against declared acceptance
criteria: it runs regardless of whether the ticket declared any, right
after build, canonical verification, full suite (if run), and every
named gate have all passed. `off` never runs it. `advisory` runs it and
records every finding in the run record and the PR body, but never
blocks. `required` quarantines the run (`code_review` gate) on any
`high`-severity finding, or if the reviewer produced no parseable
response at all. Findings reach the PR body under a `## Code review`
section, sanitized and capped at 20.

When both `-conformity-policy` (via a declared `-spec-acceptance-criteria`)
and `-code-review-policy` are enabled for the same run, both reviews run
as ONE combined call (`agent/pi/scripts/combined_review.py`) instead of
two separate sandboxed sessions over the same diff -- measured live,
example-app request 2026-09-28, review alone was 50% of that run's whole
spend.

| Restriction | Why |
|---|---|
| No `sandbox_image` key | The API's `-api-allowed-sandbox-images` allowlist check happens in `serve` before this file is read — a repo-committed image would bypass it. |
| `token_ceiling`/`cost_ceiling_micro_usd` only tighten | Applied when lower than the session's effective ceiling; equal is a no-op; a higher value refuses the run/submission outright (M3-E1) rather than being silently ignored. |

## Model routes

Four routes, three `credential_mode` values, each written into a
routes: entry:

```text
quickstart -route  routes: credential_mode  credential
-----------------  -----------------------  ----------------------------
openai             static                   none (allow_no_credential)
                                            or ANTHROPIC_API_KEY as
                                            Authorization header
anthropic          static                   ANTHROPIC_API_KEY,
                                            Authorization (placeholder,
                                            not live-tested -- see below)
copilot             github-copilot           GitHub OAuth login token, sent
                                            as the bearer token
chatgpt-codex       chatgpt-codex            codex-cli auth.json (OAuth)
```

Credentials passed on the command line or via env are kept in the daemon
process environment only; config stores file paths, never token values.

- Under `quickstart -non-interactive`, only a single unambiguous file-based
  login is picked automatically; an inherited `ANTHROPIC_API_KEY` never is.
- A Copilot model that can't run under the configured worker API (a
  `/responses`-only model with no `api: openai-responses`, or
  `policy: disabled`) is refused with the reason (`meter.CopilotModelUsable`).
- A `chatgpt-codex` route with a leftover `upstream`, `allowed_path_prefix`
  or `worker_base_path` fails startup naming the key.

### Route selection in `quickstart`

```text
 quickstart, -route unset
          |
          v
 detect: codex auth.json | pi auth.json | $ANTHROPIC_API_KEY
          |
          +-- interactive --> menu, detected routes first
          |                   (openai always offered)
          v
 -non-interactive: count detected FILE logins (codex, copilot)
          |
          +-- 0 --> fail: -route required
          +-- 1 --> use it, print which (-route overrides)
          +-- 2 --> fail: pass -route to pick one
 ($ANTHROPIC_API_KEY alone never auto-selects)
```

Detection checks a file's existence and shape only, never expiry or the
token value.

### OpenAI-compatible (`static`)

Point the route's own `upstream` at the bare API root (no `/v1`; the
worker's base URL is the upstream plus `worker_base_path`). `quickstart -route openai` takes `-model-host`,
`-model-id`, `-context-window`, and asks whether the endpoint needs a
credential (`-credential` or `ANTHROPIC_API_KEY`, sent as
`Authorization`).

### Anthropic (`static`, placeholder)

```yaml
routes:
  anthropic:
    credential_mode: static
    upstream: https://api.anthropic.com
    allowed_path_prefix: /v1
    worker_base_path: /v1
    credential_env: ANTHROPIC_API_KEY
models:
  claude-sonnet-5:
    id: claude-sonnet-5
    api: openai-completions
    routes: [anthropic]
    context_window: 200000
```

`quickstart -route anthropic` targets Anthropic's OpenAI-compatible
endpoint (`https://api.anthropic.com/v1`, Chat Completions-shaped), not
its native Messages API -- **placeholder, not live-tested**: nothing in
this repo's own live-validation runs has exercised this endpoint yet.
Takes `-model-id`/`-context-window` like the other routes (defaults
`claude-sonnet-5`/200000); the credential is `-credential` or
`ANTHROPIC_API_KEY`. No `credential_header` is set (defaults to
`X-Api-Key` today -- likely
wrong for this endpoint's own `Authorization: Bearer` convention;
revisit once this route is actually live-validated).

### GitHub Copilot (`github-copilot`)

```yaml
routes:
  copilot:
    credential_mode: github-copilot
    github_token_file: ~/.pi/agent/auth.json
models:
  copilot-model:
    id: <usable id from doctor -list-models>
    routes: [copilot]
    context_window: <from the listing>
roles:
  planning: { model: copilot-model }
  execution: { model: copilot-model }
  review: { model: copilot-model, allow_shared_model: true }
```

- Token source at run time: `routes.<r>.github_token_file` >
  `GITHUB_COPILOT_TOKEN` > `~/.pi/agent/auth.json`. `routes.<r>.github_token_key` overrides the
  `github-copilot` entry name in the file.
- `quickstart -route copilot` source order: `-credential` >
  `GITHUB_COPILOT_TOKEN` > discovered pi login. A discovered
  login is saved as `routes.copilot.github_token_file` (a path); an
  explicit token stays in the daemon env.
- A build sends the GitHub login token itself as the bearer token: it is
  pushed to the OpenShell gateway before each launch, the worker holds a
  placeholder, and it does not expire during a step. The meter overwrites
  the client headers the Copilot API requires (`User-Agent`,
  `Editor-Version`, `Editor-Plugin-Version`, `Copilot-Integration-Id`,
  `Openai-Intent`, `X-Initiator`) on every request. `quickstart`, `doctor`
  and `doctor -list-models` exchange the login token host-side for a
  short-lived Copilot token, as before; no build uses that token.
- `routes.<r>.upstream` defaults to `https://api.individual.githubcopilot.com`,
  `worker_base_path` to empty, `allowed_path_prefix` to
  `/chat/completions` (or `/responses` with the model's own
  `api: openai-responses`). `routes.<r>.credential_header` is rejected.
- `models.<m>.api: openai-responses` is supported (route defaults,
  model-usability checks, `quickstart`) for GPT-5.x/6 models Copilot
  serves only on `/responses` (e.g. `gpt-5.6-luna`).
  Unproven live: the one live build used a chat-completions model.
- `routes.<r>.upstream` is pinned (`meter.ValidateGitHubCopilotRoute`,
  called identically from `RoutePolicy.Validate` and `factoryd doctor`, so
  both refuse the same input): it must be an `https://` URL, carrying no embedded userinfo,
  whose host is `githubcopilot.com` or ends with `.githubcopilot.com`
  (covers the individual/business/enterprise plan hosts) — the Copilot
  bearer token is never sent anywhere else.
- GHE.com data-residency tenants (`copilot-api.<tenant>.ghe.com`, GitHub's
  own documented Copilot inference host for that plan) are **refused**,
  not accepted, even though the host itself is genuine: this repo's own
  OAuth **token exchange** (`internal/meter`'s `copilotTokenExchangeURL`/
  `ExchangeGitHubCopilotToken`) is hardcoded to `api.github.com`
  regardless of `routes.<r>.upstream`, which is the wrong identity domain
  for a GHE.com tenant's own login — accepting the upstream pin alone
  would let an operator configure a route whose token exchange
  authenticates against the wrong domain. Not supported end to end yet;
  revisit once the token exchange itself is made GHE.com-aware.

### ChatGPT Codex (`chatgpt-codex`)

```yaml
routes:
  codex:
    credential_mode: chatgpt-codex
models:
  luna:
    id: gpt-5.6-luna
    routes: [codex]
    context_window: 272000
roles:
  planning: { model: luna }
  execution: { model: luna }
  review: { model: luna, allow_shared_model: true }
```

- Pi harness (or `codex`, which needs this Responses route); standard worker image, no special image.
- Reads the access token and account id from codex-cli's `auth.json`
  (`routes.<r>.codex_auth_file`, else `$CODEX_HOME/auth.json`, else
  `~/.codex/auth.json`) fresh on every launch and pushes them to the OpenShell
  gateway's credential store; the worker holds placeholders. Never refreshes or
  writes it (the refresh token rotates; writing would log out the host
  `codex`).
- Fails closed if `auth_mode` is not `"chatgpt"`, a token field is missing,
  or the access token expires within 6h or before the step's own time
  budget ends. Fix: `codex login` on the host; other `codex` commands leave a
  token inside that margin as it is.
- Forces `Authorization` and `chatgpt-account-id` on every request and
  strips both from upstream error responses.
- Pinned to `https://chatgpt.com/backend-api/codex/responses`: a leftover
  `routes.<r>.upstream`, `allowed_path_prefix` or `worker_base_path`
  fails startup naming the key. `routes.<r>.credential_header` is rejected.
- Strips `max_output_tokens` from the request body (the backend rejects
  it); everything else is forwarded unchanged.
- `doctor` validates the auth file locally (no network call).

### Model discovery

| Route | Listing | Context window |
|---|---|---|
| copilot | `GET <copilot host>/models` after the token exchange, with Copilot's own client headers | `max_prompt_tokens`, else `max_context_window_tokens` |
| openai-compatible | `GET <route upstream><route worker_base_path>/models` | not reported; pass `-context-window` |
| anthropic | none (quickstart never probes this endpoint's own listing) | default `claude-sonnet-5` / `200000`; a different `-model-id` needs `-context-window` |
| chatgpt-codex | none (backend speaks only `POST /responses`) | default `gpt-5.6-luna` / `272000`; a different `-model-id` needs `-context-window` |

- `factoryd doctor -list-models` prints the list for the configured route
  and exits, tagging each entry with an `endpoints: <supported_endpoints>`
  tag, plus `picker default`, `preview`, `premium xN` and (when not usable)
  `not usable: <why>` -- the not-usable reason never repeats the endpoint
  list already shown in its own tag.
- A Copilot model is **not usable** when its `supported_endpoints` exclude
  the path the model's `models.<name>.api` forwards to (`/chat/completions`
  by default, `/responses` with `api: openai-responses` --
  many GPT-5.x/6 models, e.g. `gpt-5.6-luna`, serve only `/responses`), its
  `supported_endpoints` exclude both paths (e.g. a Claude model listed for
  Copilot's own `/v1/messages` only -- no `models.<name>.api` setting fixes
  this), or its `policy.state` is not `enabled` (enable it in GitHub
  Copilot settings). The refusal reason names the setting that would make
  the model usable, when one exists. `doctor` additionally fails closed
  when an explicit `routes.<name>.allowed_path_prefix` disagrees with the
  model's `api` (e.g. a leftover `/chat/completions` prefix from before
  switching the model to `api: openai-responses`).
- `quickstart -route copilot` offers models usable under either worker
  API, fills `-context-window` from the listing, and writes the model's
  `api: openai-responses` when the picked model needs it and
  the model listing is available. When the listing didn't run or doesn't
  carry the picked model (no token yet at listing time, or the listing
  failed), quickstart cannot verify this and instead warns that the
  worker API could not be verified, naming the model's own
  `api: openai-responses` as the fix for a `/responses`-only model,
  rather than silently writing an unverified completions config.
- `doctor` fails when the configured Copilot model is missing from the
  listing or not usable; with a non-default route `upstream`
  (Business/Enterprise host) it downgrades to a warning. For
  OpenAI-compatible routes, `doctor` checks from inside the sandbox that
  the upstream lists the configured model.
- Credential hygiene on every listing call: `ValidateUpstreamScheme` and
  `CredentialSafeForUpstream` run before any credential is sent (so no
  token goes to a plaintext upstream a real run would refuse); nothing is
  sent when the selected route's `allow_no_credential` is set; redirects are refused;
  userinfo is redacted from printed URLs.

**Weak-model advisory.** `quickstart` and `doctor -list-models` print a
warning (never a block) for a model on a short, dated list with cited
evidence. Current entry: `gpt-4.1` (2026-09-25 onboarding walk: could not
finish a small ticket that `gpt-5.6-luna` did first try).

**First real request is still authoritative.** `unsupported_api_for_model`
= wrong API for the model (check the model's own `api` field); `model not found` /
`not accessible` = wrong id or no entitlement.

## Routes and models (`routes:`/`models:`)

`routes:`/`models:`/`roles:` is the only session-config schema: named
routes (an upstream/credential pairing) separate from named models
(which route(s) a model may reach, in order, never another model), and
`roles:` picks a model and a Pi thinking level per kind of work.
`internal/modelrole.SelectRoute` resolves `roles.<role>.model` against
this config at every real launch (bare run, worker, and the Temporal
Worker's own route-binding check), trying that model's own declared
routes in order and skipping any whose policy validation, upstream
scheme, or credential resolution fails.

```yaml
routes:
  codex:   { credential_mode: chatgpt-codex, codex_auth_file: ~/.codex/auth.json }
  litellm: { credential_mode: static, upstream: https://litellm.example.internal,
             allowed_path_prefix: /v1, credential_header: Authorization,
             credential_env: LITELLM_KEY }
models:
  luna:
    id: gpt-5.6-luna
    api: openai-responses
    routes: [codex, litellm]        # ordered fallback; never another model
    context_window: 272000
    reasoning: true
    thinking_level_map: { xhigh: xhigh, max: max }
    extra_json: { samplingParams: {} }   # may not set id/api/baseUrl/reasoning/thinkingLevelMap/contextWindow
roles:
  execution: { model: luna, thinking: medium, allowed: [luna] }
  review:    { model: luna, thinking: max, allow_shared_model: true }
```

- Each `routes:` entry is validated on its own: `credential_mode` must be
  `static` (the default), `github-copilot`, or `chatgpt-codex`; a static
  route needs `upstream` unless `allow_no_credential: true`;
  `credential_env`/`credential_header` only apply to a static route,
  `github_token_file`/`github_token_key` only to a github-copilot route,
  and `codex_auth_file` only to a chatgpt-codex route -- each refused
  outside its own matching mode rather than silently ignored. A
  chatgpt-codex or github-copilot route's own defaulted upstream/path is
  checked against the same host pin a real launch enforces
  (`meter.ValidateChatGPTCodexRoute`/`ValidateGitHubCopilotRoute`), so an
  explicit `upstream`/`allowed_path_prefix`/`worker_base_path` override
  the pin would reject at launch time is refused here too, at validation
  time. `billing` is `subscription`, `metered`, or left empty to derive
  from `credential_mode` (chatgpt-codex/github-copilot default to
  subscription, static to metered) -- display only, never enforcement.
- Every `models:` entry is validated, whether any role references it yet
  or not: a non-empty `id`, a non-empty `routes` list naming only routes
  that actually exist, and `route_ids` keys that are themselves already
  in that model's own `routes`.
- Every top-level `relay_*`/`model_aliases` key from the retired
  single-route schema is refused outright at load, naming which key
  moved under `routes:`/`models:`/`roles:`.
  `relay_image` and `relay_upstream_timeout` (deleted with the inference
  relay) and `sandbox_pids` (a worker's process limit is the OpenShell
  gateway's own, 1024) are refused the same way, telling you to remove the
  line: delete them from an older `config.yml` before `make install`, which
  fails at `configure-images` otherwise.
  The eight budget keys are `meter_*` (`meter_token_ceiling`,
  `meter_cost_ceiling_micro_usd`, `meter_token_budget`, ...): a config that
  spells one `relay_*` is refused with its new name.
- `roles.execution` is required for a model-backed build (there is no other
  model source to fall back to; a config with no `routes:`/`models:`/
  `roles:` at all is still valid for an offline build). `roles.*.allowed` is the
  closed set of `models:` entries that role may resolve to (`model` stays
  the default pick, defaulting `allowed` to `[model]` when unset); a
  role's `thinking` is checked against every model in `allowed`, not just
  `model`. A request may choose a model within `allowed` for the
  `planning` and/or `execution` role only (`submit -model role=model`,
  POST /requests' `models` body field, `run -execution-model` for a
  bare `factoryd` run) -- `review` is never requester-selectable, and the choice
  never picks a route, thinking level, or anything else the role doesn't
  already own; a choice outside `allowed` is refused, naming the
  rejected model and the allowed list, and is refused outright with no
  `routes:`/`models:`/`roles:` session config at all.
- The coding-agent CLI (the harness: `pi` by default, or `pifork`, `codex`,
  `copilot`) is chosen
  per role: `roles.<role>.harness`, with `allowed_harnesses` the closed set a
  request may pick from (empty means just `[harness]`; `harness` must be in it).
  Every name must be a compiled-in registry entry (`internal/harness`), and a
  harness must speak the api of every model/route its role may resolve to.
  A request may pick a harness for `planning` and/or `execution` only
  (`submit -harness role=name`, POST /requests' `harnesses` body field,
  `run -execution-harness` for a bare `factoryd` run); each job launches with its own
  role's harness (build and corrective rounds: execution; conformity, code and
  combined review, oracle drafting: review; spec and plan drafting: planning --
  a role with no `roles:` entry uses `pi`, except drafting/review jobs whose
  role is unset, which run as the execution role). A role whose harness needs
  its own worker image (`pifork`) requires an explicit digest-pinned
  `sandbox_image`; `codex` (Codex CLI) speaks only the Responses API, so its
  role's model must resolve to an `openai-responses` route, while `copilot`
  (Copilot CLI, bring-your-own-key) speaks all three apis; both run from the
  standard worker image with per-session state under the workspace session dir; the retired top-level `engine:` key is refused at load.
  `factoryd doctor` prints each role's harness on its `roles resolve` row and
  runs `<binary> --version` in the sandbox image for every harness in use.
- Any model in `roles.review`'s own `allowed` list resolving to the same
  **backend** as any model in `roles.execution.allowed` -- compared by
  `credential_mode` + `upstream` (both after defaults) + effective id
  (`route_ids` override applied) + effective API, not by route name, so
  two differently-named routes to the identical real upstream still
  count as one backend -- is refused outright unless
  `roles.review.allow_shared_model: true` waives it.
  `allow_shared_model` set with no `routes:`/`models:` configured at all
  is refused too, naming the key, rather than silently doing nothing.
- A model's `extra_json` may not set `id`, `api`, or `baseUrl` at all, nor
  `contextWindow`/`reasoning`/`thinkingLevelMap` when the model's own
  first-class field for that key is also set -- those compose from the
  model's own first-class fields instead, so a model owns its whole
  worker-model JSON object rather than layering a session-level default
  underneath it.
- A `models:` block written without a `routes:` key is refused too
  (there is nothing for a `models:` entry's own `routes:` list to resolve
  against), naming `models:` rather than silently doing nothing.
- `factoryd doctor`'s own `roles resolve` check validates this schema,
  reporting an invalid block as a FAIL row alongside every other check,
  and a schema-clean config as a plain OK.
- Every model any role's `model` or `allowed` could resolve to must also
  be launchable, not just schema-valid: the same route-policy checks a
  real launch runs (a worker model `id` must not contain a slash, among
  others) are checked at config-load time too
  (`modelrole.ValidateAllowedPolicies`, wired into `validateRoles` and
  `doctor`'s own `roles resolve` check) -- found live (M3 walk,
  2026-09-28), an `allowed` model whose `id` was a local filesystem path
  passed config load, `doctor`, and `submit`, and only failed once a
  human had already drafted and approved a spec/plan against it and the
  real build tried to launch.

### Model prices

A dollar cost/budget/ceiling is computed from each model's real provider
price, not a session-config default: `internal/prices/prices.yml`, a
table compiled into the `factoryd` binary with
`go:embed` and keyed by the provider's own model id (`models.<m>.id` --
so every session config and every alias sharing that id shares one
price). Each entry is USD per 1,000,000 tokens:

```yaml
gpt-5.6-luna:
  input: 0.20
  cached_input: 0.02   # optional; absent = input (no cache discount)
  cache_write: 0.20    # optional; absent = input (no cache-write premium)
  output: 1.20
  source: https://developers.openai.com/api/docs/pricing
  as_of: 2026-09-28
```

- To change a price (a provider updated theirs, or you're adding a new
  model), edit `internal/prices/prices.yml` and run `make install` --
  there is no runtime override; the table is part of the built binary.
- A model whose `id` has no table entry, or whose prices are `0` (a local
  model), costs $0: its dollar budget and ceiling never trip, and its token
  budget and ceiling bound it. `factoryd doctor` warns about a missing entry
  so a paid model doesn't silently run at $0.
- `factoryd doctor` prints one `models.<name>: price` row per declared
  model naming its own price, source, and `as_of`, and warns when the
  entry is missing or `as_of` is missing or more than 90 days old (the price itself may
  still be correct; nothing here can prove that from the date alone).
- A dollar cost budget/ceiling (`meter_cost_budget_micro_usd`,
  `meter_cost_ceiling_micro_usd`, and the request/monthly cost budgets)
  is computed from these prices, so correcting a stale price changes how
  many tokens a dollar budget allows for that model -- a TOKEN
  budget/ceiling is unaffected either way. The retired per-token session
  keys (`relay_cost_per_input_token_micro_usd`/
  `relay_cost_per_output_token_micro_usd`) and the retired per-model keys
  (`models.<m>.cost_per_input_token_micro_usd`/
  `cost_per_output_token_micro_usd`) are refused at load, naming
  `internal/prices/prices.yml` as where pricing now lives.

## pifork harness (opt-in)

Pi is the default harness. `pifork` runs a fork of Pi from a worker image
you build yourself (`make pifork-image PIFORK_DOCKERFILE=<your Dockerfile>`).
Image contract, an example Dockerfile and containment:
[`doc/designs/pifork-harness.md`](doc/designs/pifork-harness.md). Then:

```yaml
sandbox_image: localhost:5050/buildgate-pifork@sha256:<digest>
routes:
  copilot:
    credential_mode: github-copilot
models:
  luna:
    id: <usable Copilot model id>
    # api: openai-responses   # only for a Responses-only model
    routes: [copilot]
    context_window: 131072
roles:                  # harness is per role; repeat it on planning/review to run them on pifork too
  execution:
    model: luna
    harness: pifork
registry_proxy: true   # optional: npm/PyPI/Go via the run-scoped proxy
```

```sh
factoryd doctor \
  -sandbox-image localhost:5050/buildgate-pifork@sha256:<digest>
factoryd worker
```

## Codex and Copilot harnesses (opt-in)

Both run from the standard worker image (`codex` and `copilot` are baked
in; no `sandbox_image` needed) and reach the model only through the
sandbox's supervisor. Pick them per role; a request may switch `planning`/`execution`
within `allowed_harnesses`:

```yaml
routes:
  codex: { credential_mode: chatgpt-codex }
models:
  luna: { id: gpt-5.6-luna, routes: [codex], context_window: 272000 }
roles:
  planning:  { model: luna, harness: pi }
  execution: { model: luna, harness: codex, allowed_harnesses: [codex, pi] }
  review:    { model: luna, allow_shared_model: true }
```

| Harness | Model api it can use | Notes |
|---|---|---|
| `codex` | `openai-responses` only (a `chatgpt-codex` route, or a model with `api: openai-responses`) | Config load and `doctor` refuse it on a completions or anthropic-messages model |
| `copilot` | any | Bring-your-own-key, no GitHub login; its provider key is the route's placeholder; on a `chatgpt-codex` route it also sends the account header's placeholder and runs behind an in-worker loopback proxy that fills the final response event's empty `output`, where the CLI reads a turn from; reports no token counts, so read the meter's figure |

Both take the role's `models.<m>.id` as their model. `doctor` prints each
role's harness on its `roles resolve` row and runs `<binary> --version`
in the sandbox image for every harness in use. Requirements and
per-harness state directories: [`agent/pi/README.md`](agent/pi/README.md).

## Worker skills (`skill_dirs:`, `roles.<role>.skills`)

Agent skills (folders holding a `SKILL.md`) reach each role's coding agent
as a read-only snapshot, opt-in per role. No host harness folder
(`~/.codex`, `~/.copilot`, `~/.claude`, `~/.pi`) is ever mounted, and no
sessions or memories are shared.

```yaml
roles:
  execution:
    model: luna
    skills: [buildgate-tdd, buildgate-diagnosing, go-service]   # built in; nothing is on by default
skill_dirs:                      # optional: your own skills, searched first, in order
  - ~/team-skills
```

Built-in skills ship inside `factoryd` (`internal/workerskills`), written
for sandboxed, non-interactive build rounds and buildgate's own prompts.
They are conditional knowledge, loaded by the agent only when relevant; the
every-round definition of done is part of the build prompt itself.

| Built-in skill | Use |
|---|---|
| `buildgate-tdd` | One seam, one failing test, minimal code; expected values from an independent source |
| `buildgate-diagnosing` | Red/green loop before changing code when a check fails |
| `go-service`, `typescript-service`, `flutter-app` | Stack practice when the repo has `go.mod` / `package.json` / `pubspec.yaml` |
| `kafka-processing`, `postgres-change`, `temporal-go` | Delivery semantics, migration safety, workflow determinism when the repo uses them |

Planning and review jobs have fully specified, JSON- or file-only prompts;
a measured live comparison found skills there added tokens and conflicting
instructions, so none is recommended for those roles.

| Key | Meaning |
|---|---|
| `skill_dirs` | Optional ordered host folders, absolute or `~/`, searched before the built-ins (a same-name skill there overrides the built-in). Read by `factoryd` on the host; they need not be shared with the Docker VM |
| `roles.<role>.skills` | Skill names only (`^[a-z0-9][a-z0-9-]{0,63}$`), never paths. No key means no skills. A request cannot add, remove or name one |

```text
config load ── every name resolves (skill_dirs, then built in)? ──no──> refused (run, submit, worker, serve, daemon)
launch of a model-backed job (build, review, spec/plan/oracle drafting)
  └─ host: resolve the job's role skills, snapshot them under the job's log dir,
           SHA-256 the snapshot ── a refusal below fails the launch
  └─ worker: snapshot mounted read-only at /inputs/skills
       pi, pifork    --skill /inputs/skills/<name>
       codex, copilot fresh copy into <session home>/skills/<name> every round
                      (codex's own bundled skills are off)
```

| Refused at launch | Why |
|---|---|
| The target repo has a project skill (`.github/`, `.agents/`, `.claude/`, `.pi/skills`) with the same folder or front-matter name | Copilot lets it replace the operator's skill; Codex lists both |
| A symlink below a skill's root, a non-regular file, or a file with an exec bit | The snapshot is a plain, non-executable copy; scripts would not run |
| `SKILL.md` missing, or its front-matter `name` differs from the configured name | Catches a `skill_dirs` entry pointing at the wrong folder |
| All of a role's skills together over 2 MiB | Bounds the snapshot |
| A skill folder inside the workspace | The worker writes there |

Target-repo skills are a second, separate layer: the repo's own committed
skills, which the harness finds in `/workspace` itself.

| Harness | Repo skills it loads | In which jobs |
|---|---|---|
| pi, pifork | `.agents/skills` (passed as `--skill`, at most 64; pi skips project skills on its own in non-interactive mode) | The build only. Review, spec drafting, planning and oracle drafting take the operator's skills alone |
| codex | `.agents/skills` | Every job: the CLI reads the folder itself |
| copilot | `.agents/skills`, `.github/skills`, `.claude/skills` | Every job: the CLI reads the folders itself |

A repo skill is instruction text, and a build can write one. A review does
not read what the build wrote: see "What a review reads of the repository's
instructions" below. The pi row holds at pi's default project trust: an image
whose pi settings trust every project lets pi find the repo's skills itself,
in every job.

Put a repo skill meant for every harness in `.agents/skills`.

Review jobs take `roles.review`'s skills, or `roles.execution`'s when
`roles.review` is unset, the same fallback as their harness. Each attempt
records `skills`, `skills_sha256` and `repo_skills` (the repo's own project
skills, scanned after the attempt from the worktree, so for a review attempt the
masked paths are in `review_masked_paths`; at most 64); drafting jobs record
`skills`/`skills_sha256` on the request. A Temporal Worker mounts a run's
skills only when its own session config resolves the same ones; `factoryd status` prints them, and
`factoryd doctor`'s `skills` row dry-runs every role's snapshot (against
`-workspace` when given). Skill content is operator-trusted: vet a
third-party skill like any dependency.

### What a review reads of the repository's instructions

Every model review of a run (spec conformity, code review, the combined
review; a pull-request round's fix runs through the same ones) launches with
the repository's instruction files as the base commit holds them. Each path
below counts in any directory of the repository, at any depth
(`packages/app/.github/instructions` as `.github/instructions`). Build, verify
and gate launches are not masked.

| Kind | Instruction paths |
|---|---|
| Files, by name | `AGENTS.md`, `AGENTS.override.md`, `CLAUDE.md`, `CLAUDE.local.md`, `GEMINI.md` |
| Folders, with everything in them | `.pi/`, `.codex/`, `.claude/`, `.agents/skills`, `.github/skills`, `.github/instructions`, `.github/agents`, `.github/hooks` |
| Files, by path | `.github/copilot-instructions.md`, `.mcp.json`, `.vscode/mcp.json` |

| | What the review gets |
|---|---|
| Which commit | The commit the request started from (the run's `instruction_base_sha`, set by `-instruction-base` for every build that follows an earlier run; for a ticket's first build, the run's own base). It must be an ancestor of the result, else the run halts as below |
| Sees, as the base commit has them | Each instruction path the build changed, mounted read-only over the worktree; a path the build deleted is mounted back |
| Shown as data | What the build did to those paths, as a fenced block after the diff (`--instructions-diff`, at most 60,000 characters), labelled as data about the change, never instructions. A complete, uncut list of the paths it touched comes first, and names those the cap cut out; the review is told to report such a path as a finding |
| Removed first | Instruction-named paths the worktree holds that the result commit does not (untracked or ignored); listed in `review_removed_paths` |

| Halts the run before the review launches | What to change |
|---|---|
| A tracked instruction file no longer matches the result commit (an uncommitted edit, or a mode or content the checkout rewrote) | Commit or discard the difference in the worktree, then `factoryd retry` |
| An instruction path, or the `.github`, `.vscode` or `.agents` folder above one (in any directory), is a link to a directory that is not an instruction path, or a link whose target the build changed | Replace the link with the real file or folder in the repository |
| A checkout converted an instruction file (line endings, filters) | Stop converting that file (`.gitattributes`) |
| A path is spelled two ways (`AGENTS.md` and `agents.md`, or `pkg/.claude` and `Pkg/.claude`), or a submodule sits under an instruction path, or its checkout holds one or a link at the `.github`, `.vscode` or `.agents` folder above one | Keep one spelling; move the submodule |
| More than 64 instruction paths changed, over 2,000 instruction files, a file over 16 MiB, or an instruction path that changed between a file and a directory | Split the change so the build leaves fewer instruction files altered; the review attempt's `review_instructions_error` names which limit |

The halt's `halt_reason_code` is `review_instructions_failed`; its message is
a fixed sentence. The cause is in the review attempt's
`review_instructions_error`, `factoryd status` quotes it for the operator,
and no handoff or later build is told.

| Evidence field on a review attempt | Holds |
|---|---|
| `review_instructions_sha256` | SHA-256 of the snapshot of base-commit instruction files the review read |
| `review_masked_paths` | The instruction paths mounted from the base commit (at most 64, then `... and N more`) |
| `review_removed_paths` | The instruction-named paths removed from the worktree before the review (same cap) |

The list of instruction paths is a table in the code: a harness that loads a
path not on it is not covered. `make probe-instruction-paths` measures what
the pinned `pi`, `codex` and `copilot` harnesses load from a workspace (no
model call, no network) against that table.

## Repository memory (`factoryd memory`)

`factoryd memory <subcommand> -workspace <repository> [flags] [arguments]`.
Flags come before the arguments. The walkthrough is in USAGE.md.

| Subcommand | Does | Writes |
|---|---|---|
| `list` | Prints whether memory is on (and which switch has it off), the budget and its use, the lines in force (the fenced section of root `AGENTS.md` at the checkout's HEAD, read from git, never the worktree) and the candidates: `ID`, `SEEN`, `SOURCE`, `STATE`, `LINE`, most seen first, then newest, dropped last. With memory on it first collects candidates from the project's newest 200 finished runs and reconciles the store with the section | The store, only with memory on |
| `show <id>` | One candidate: its line, the runs that said it and when each ended, its history. An id may be shortened to a unique prefix | Nothing |
| `add "<text>"` | Adds your own candidate under the text rule; the refusal names the rule broken. A dropped line added again is a candidate again | The store |
| `drop [-reason <why>] <id>` | Moves a candidate or proposed line to `dropped`. A dropped line still counts the runs that say it and is never proposed | The store |
| `propose [-remove "<line>"]... [<id>...]` | Opens one memory request: adds the named candidates (none named: the most seen that fit) and removes each `-remove` line. At most five changes. Refused when the section would be over its budget (pass `-remove`, or raise `budget_lines`/`budget_chars`), when the repository has no `verify_command`, or while another memory request of the repository is neither `done` nor `cancelled` | The proposal, the request, the store |
| `off [-reason <why>]` / `on` | Writes / removes the project's stop marker. `on` does not list a repository under `memory.repositories`: that is your edit | The stop marker |

| Flag | Meaning |
|---|---|
| `-workspace <path>` | The repository: its root or any directory inside it. Required |
| `-config <file or profile>` | The session config whose `memory.repositories` is the switch and the budget |
| `-data-dir <dir>` | Default: the session config's |
| `-json` | `list` (the shape of `GET /projects/{project}/memory`) and `show` |
| `-reason <text>` | `drop`, `off` |
| `-remove "<exact line>"` | `propose`: a line as `memory list` prints it, `- ` included. Repeatable |

The text rule a line passes (never repaired, for a build agent's note and for
`add` alike):

| A line | Rule |
|---|---|
| Length | One line, at most 120 characters |
| Characters outside backticks | Letters, digits, space and `. , : ; ( ) ' " / = + -` |
| A command | Inside one pair of backticks: letters, digits, space and `. _ / : = -`, not starting with `-` |
| Refused anywhere | `//` and `www.` (a URL in any form), an e-mail address, `/users/` and `/home/` in any case, an IPv4 or hex-colon address, `::`, any unbroken run of 20 or more of `A-Z a-z 0-9 + / = _ -`, anything secret redaction would change |
| Refused outside backticks | A path that starts with `/`; a start of `-`, `+` or digits followed by `.` or `)` |

Store layout, under `<data-dir>/memory/<key>/`, where `<key>` is the
lower-cased project name, `-`, and the first 12 hex characters of the SHA-256
of the repository's root path:

| File | Holds |
|---|---|
| `state.json` | The candidates (`candidate`, `proposed`, `dropped`), each with its line, source, the runs that said it (at most 20) and its history; and the runs already collected. At most 200 lines and 1 MiB. A line in force is not stored: the file is the memory |
| `off` | The stop marker `memory off` wrote |
| `proposals/<request-id>.json` | The exact `AGENTS.md` a memory request must produce, and its SHA-256: what the release check compares |
| `proposals/<request-id>.changes.json` | The lines that request adds and removes and the run ids an added line came from, for the pull request body |

`GET /projects/{project}/memory` (read token, like the project's
observations) returns the same view as `memory list -json` for the repository
the project's requests are submitted against: `on`, `off_reason`,
`budget_lines`, `budget_chars`, `used_lines`, `used_chars`, `in_force`,
`section_error`, `candidates` (`id`, `line`, `source`, `state`, `seen`,
`first_seen_at`, `last_seen_at`, `request_id`). It collects nothing and writes
nothing; the console's Memory tab of a project reads it. There is no MCP tool
for memory.


## Design guide (`design_guide`, `design_guide_dirs:`)

A design guide is the team's own rules for what to decide before code is
written: one Markdown file, shared by every repository that names it. Spec
drafting reads its questions and planning reads its structural rules.

```yaml
# target repo: .factory.yml
design_guide: go-service         # a name, never a path

# operator: session config
design_guide_dirs:               # searched in order; the first <dir>/<name>.md wins
  - ~/code/team-standards
```

```markdown
# Go service guide

## Spec decisions

Answer each for every request. "Not applicable: <reason>" is a valid answer.

1. Delivery. Does the caller need the result in the response?
   - Yes, and the work finishes within 500 ms: synchronous.
   - No, and the work must survive a restart: Kafka worker; the endpoint
     does not wait. State the delivery contract.
   - Unclear from the request: raise it as a decision for the operator.
2. Caching. Is the same data read far more often than it changes? ...
3. New infrastructure. Name each endpoint, worker, topic, table or cache
   the change adds. A bug fix adds none.

## Plan rules

- Layers: handler -> service -> repository/client.
- A dependency crosses a layer as an interface declared by its consumer;
  tests use a fake of it.
- Independent upstream calls run concurrently with errgroup.
```

```text
.factory.yml names a guide ──no──> prompts unchanged
        │ yes
resolve <name>.md in design_guide_dirs ──not found / malformed──> the drafting job fails, naming the guide
        │
spec drafting  <- text under "## Spec decisions"   (a section before the drafting instructions)
planning       <- text under "## Plan rules"
        │
name and SHA-256 recorded on the request's spec and plan drafting records
```

| Key or rule | Detail |
|---|---|
| `design_guide` (`.factory.yml`) | The guide's name. Absent: no guide, prompts unchanged |
| Checking it | `factoryd doctor`'s `design guide` row lists the guides the folders hold; with `-workspace <repo>` it resolves the guide that repository names and prints its path and digest, or fails naming what is missing |
| `design_guide_dirs` (session config) | Ordered host folders, absolute or `~/`, each an existing directory; a guide is `<dir>/<name>.md`. Read by `factoryd` on the host at each draft, so an edit to a guide applies to the next draft; a change to the key needs a worker restart |
| Headings | `## Spec decisions` then `## Plan rules`, each on its own line with text under it, and neither repeated anywhere else in the file (a code fence does not hide one). Anything above the first is ignored |
| What goes where | A decision someone outside the service can observe, or that a human signs off (sync or async, caching, new infrastructure), is a spec decision. Code structure (layers, interfaces, concurrency patterns, test style) is a plan rule |
| Wording | Write rules with their condition ("shared cache when more than one replica must agree within N seconds"), and say which choices go to the operator: those become `[NEEDS DECISION]` items in the spec |
| File | A regular file (a symlink is refused), UTF-8, at most 16 KiB |
| Name | `^[a-z0-9][a-z0-9-]{0,63}$`. The repository selects a guide; only the operator supplies one |
| Service-specific facts | Its upstreams, replica count and latency targets belong in that repository's `ARCHITECTURE.md`, which drafting already reads |
| Rules a tool can check | Put them in the repository's `verify_command`, where every agent and human hits them; the guide is not enforced on the built code. The sandbox has no network and no lint tool (`golangci-lint`, `gocyclo`), so write such a rule as a test that needs only the language toolchain: a Go test that parses the repository with `go/ast` and fails on a function over the team's complexity limit, or on a package importing a layer it must not, with a list of existing exceptions that may shrink but not grow. A `lint_command` works only when its tool is in the image (`make project-sandbox-image`) |

Measured on six requests against a Go, Kafka, Postgres and Redis service:
with a guide 5 of 6 requests stated or raised every applicable design
decision, against 3 of 6 without, for about 2% more planning tokens.

## Keys with no CLI flag of their own

Defaults come from `internal/sessionconfig.DefaultSettings`; omitting a key
(or the file) changes nothing. They apply identically to `factoryd <run>`,
`worker` and `daemon`; `doctor` also reads them. `build_app_max_attempts`
and `verify_max_attempts` also have flags on `factoryd <run>` and
`worker` (so `POST /runs` can set them per run).

`serve` exceptions: its `-release-*` flags win only when passed (else the
config value applies); its `-sandbox-memory`/`-sandbox-cpus`/
`-sandbox-tmpfs-size`/`-sandbox-worker-uid` flags always decide those
limits for API-started runs.

| Key | Default |
|---|---|
| `sandbox_docker` | `docker` |
| `sandbox_user` | unset (`sandbox_worker_uid`:this process's GID) |
| `sandbox_worker_uid` | `65532` |
| `sandbox_memory` | `4g` |
| `sandbox_cpus` | `2` |
| `sandbox_tmpfs_size` | `1g` |
| `model_host_concurrency` | `1`. Locks only a single-instance-looking route upstream (loopback, RFC1918, Tailscale, `.local`/`.lan`, non-HTTPS or unparseable). SaaS hosts (`chatgpt.com`, `api.githubcopilot.com`, `api.anthropic.com`) are never locked. `0` disables. |
| `max_parallel_jobs` | `2`, at least `1` (two builds fit a 4 GiB Docker VM next to Temporal). How many model jobs and builds `factoryd worker` runs at once across all requests. Each build's sandbox takes up to `sandbox_memory`. |
| `memory.repositories` | Unset: repository memory is off for every repository. A list of `{path, budget_lines, budget_chars}`: `path` is the repository's root, absolute or `~/` (listed once; matched by root path with symlinks resolved, never by directory name); `budget_lines` 5 to 80 (default 40) and `budget_chars` 500 to 6000 (default 3000) bound the section memory keeps in its `AGENTS.md`. A memory action also stops while the project's kill switch is engaged or its `off` marker exists (`<data-dir>/memory/<project>/off`) |
| `meter_max_request_bytes` | `1048576` |
| `meter_requests_per_minute` | `60` |
| `meter_token_budget` | `1000000` |
| `meter_token_budget_window` | `1h` |
| `meter_cost_budget_micro_usd` | `5000000` |
| `meter_cost_budget_window` | `1h` |
| `meter_token_ceiling` | `0` (= 5x `meter_token_budget`) |
| `meter_cost_ceiling_micro_usd` | `0` (= 5x `meter_cost_budget_micro_usd`) |
| `request_token_budget` | `0` (unlimited) -- one request's total spend (drafting + every ticket run + corrective/PR-review rounds); reaching it quarantines the request instead of launching another job |
| `request_cost_budget_micro_usd` | `0` (unlimited) -- same scope as `request_token_budget`, in micro-USD |
| `monthly_token_budget` | `0` (unlimited) -- total spend across every request in the data dir during the current calendar month (UTC) |
| `monthly_cost_budget_micro_usd` | `0` (unlimited) -- same scope as `monthly_token_budget`, in micro-USD |
| `registry_proxy_npm_upstream` | `https://registry.npmjs.org` |
| `registry_proxy_pypi_upstream` | `https://pypi.org/simple` |
| `registry_proxy_pypi_files_upstream` | `https://files.pythonhosted.org` |
| `registry_proxy_go_upstream` | `https://proxy.golang.org` |
| `registry_proxy_gosumdb_upstream` | `https://sum.golang.org` |
| `registry_proxy_cache_bytes` | `1073741824` (1g) |
| `registry_proxy_max_object_bytes` | `268435456` (256m) |
| `registry_proxy_max_concurrent_upstream` | `16` |
| `registry_proxy_upstream_timeout` | `60s` |
| `compose_services_memory` | `2g` -- per service; a service's own `mem_limit` may lower it, never raise it |
| `compose_services_cpus` | `1` |
| `compose_services_max_services` | `8` |
| `compose_services_ready_timeout` | `3m` |
| `compose_services_allowed_registries` | `[docker.io/library/]` -- the registry/namespace prefixes a target repo's compose images may come from. Never widened by a run or by a compose file: `doctor -target-repo <repo> -fix` and `quickstart` offer the prefixes a repo's images need and write them only on a yes |
| `compose_services_worker_env` | empty -- `KEY: VALUE` worker variables for application endpoints the published ports don't already cover (the worker's `localhost` reaches each published port), e.g. `PSQL_URL: postgres://database:5432/<db>`, `REDIS_URL: redis:6379`. Factory, sandbox, credential, cache, proxy and `SF_*` names are rejected. Values reach the worker by name only: never in the docker argv, `run.json` or Temporal history |
| `compose_services_require_digest` | `false` |
| `compose_services_concurrency` | `1` -- runs on this machine (any data dir or factoryd process) with compose sidecars at once; `0` disables the slot |
| `release_protected_paths` | empty (`.factory.yml`'s `protected_paths` overlays it) |
| `release_max_files_changed` | `0` (deny-all; see below) |
| `release_max_insertions` | `0` (deny-all; see below) |
| `release_rollback_plan` | empty (deny-all; see below) |
| `release_allow_overrides` | `false` |
| `release_allow_dependency_lockfile_changes` | `false` |
| `release_allow_unsandboxed` | `false` |
| `release_allow_skipped_project_check` | `false` |
| `build_app_max_attempts` | `2` |
| `verify_max_attempts` | `2` |

Other keys that are not in the table: `data_dir` (the data dir of every
command; see `-data-dir` resolution above for a config that sets none), `routes`/`models` (see Routes and models above --
`github_token_file`/`github_token_key`/`codex_auth_file` are per-route
fields there, not top-level keys), `roles` (per-kind-of-work model/
thinking picks over `models:` -- see above), `workspaces`,
and the flag-mirroring keys (`sandbox_image`, `meter_image`,
`registry_proxy*`, `pr_*`, `max_review_rounds`, `review_corrective_rounds`, `hitl_reminder_interval`,
`advance_on`, `conformity_policy`, `code_review_policy`, `open_pull_request`,
`compose_services`, `egress_ca_bundle`).

**Release policy.** The bare defaults above (`0` / `0` / empty) make
`MergePolicyCheck` deny every release. `quickstart` writes a usable policy
(`25` / `1000` / `"git revert the merge commit on main"`, from
`sessionconfig.DefaultReleaseMaxFilesChanged` and friends) into a new
config, `init-config`'s scaffold has the same values, and `quickstart`
reusing an existing config backfills only the missing keys. `doctor` warns
(and the console board shows a strip) while all three are deny-all;
`doctor -fix` can write them.

**Code review default.** A freshly written config (`quickstart` writing a
NEW config, not reusing an existing one) also sets `code_review_policy:
required` -- unlike `worker`'s own bare `-code-review-policy` flag,
which stays off by default for an operator running with no config at all.
A new factory therefore gets the review-corrective round's code_review
half (above) working out of the box; an operator who wants it off edits
`code_review_policy` in the written config.

## Temporal: repository owners, daemons, observing

Every build runs on Temporal; [`USAGE.md`](USAGE.md#temporal-what-runs-every-build) covers the server,
the default address, how the worker drives a request and what happens to a
lost step. This section is the rest: naming a server, queueing runs per
repository, long-lived daemons, and what to look at.

```text
  factoryd <run>  ---+
  factoryd daemon ---+   -repository X    +-------------------------+
  factoryd supervise +------------------->| RepositoryOwnerWorkflow |
  POST /runs --------+   (one task queue  | (serializes requests    |
         |               per repository)  |  for repository X)      |
         |                                +------------+------------+
         | no -repository                              | child
         | (no exclusivity)                            v
         +------------------------------------->+-------------+
                                                | RunWorkflow |
                                                +------+------+
                                                       v
                                        Temporal server (Postgres)

  factoryd serve -daemon-temporal-address --(GET/POST /daemons)--> daemons
```

### A single ticket against a named server

Same command as [`USAGE.md` § Run a ticket](USAGE.md#run-a-ticket). Name a server only when it isn't `localhost:7233`:

```sh
factoryd \
  -workspace ~/code/calc-app/workspace \
  -spec ~/code/calc-app/spec/tickets/001-*.md \
  -ticket 001 \
  -ticket-file ~/code/calc-app/spec/tickets/001-*.md \
  -verify-command "make verify" \
  -temporal-address localhost:7233
```

If an explicitly named server is unreachable:

| Flags in effect | Result |
|---|---|
| any run | Fails closed: run recorded `halted`; the error reads `Temporal at <addr> is unreachable: ...; builds run only on Temporal (factoryd doctor -fix starts it)` |

Preflight, verify gate and release decision run in the workflow.

### Serialize runs per repository (`-repository`)

Without `-repository` every invocation is an independent `RunWorkflow` in
its own worktree and branch, so runs on one repository overlap. To queue
them one at a time, add `-repository` (requires `-temporal-address`):

```sh
factoryd \
  -workspace ~/code/calc-app/workspace \
  -spec ~/code/calc-app/spec/tickets/002-*.md \
  -ticket 002 \
  -ticket-file ~/code/calc-app/spec/tickets/002-*.md \
  -verify-command "make verify" \
  -prior-run <ticket-1-run-id> \
  -temporal-address localhost:7233 \
  -repository calc-app
```

Every invocation naming the same `-repository` signals one
`RepositoryOwnerWorkflow` on a shared task queue. A second invocation
queues behind the first instead of overlapping.

**Stop line.** The owner halts a repository after recurring infrastructure
failures, a quarantined run, or a child that committed to the shared
workspace before failing. Once you have checked the workspace, resume it:

```sh
factoryd reset-stop-line -temporal-address localhost:7233 \
  -repository calc-app -by "<you>" -reason "<what you checked>"
```

### Daemons and lifecycle control

`-repository` needs a live worker polling its task queue.

```sh
factoryd daemon -temporal-address localhost:7233 -repository calc-app

factoryd supervise -temporal-address localhost:7233 \
  -repository calc-app -repository another-app
```

| Command | Temporal-relevant flags | Notes |
|---|---|---|
| `factoryd daemon` | `-temporal-address`, `-repository` (both required), `-config` | One repository per process. Blocks until SIGINT/SIGTERM |
| `factoryd supervise` | `-temporal-address`, `-repository` (repeatable), `-config`, `-restart-backoff`, `-restart-backoff-max`, `-startup-grace` | Runs one `daemon` child per repository; restarts it on exit or stale heartbeat. `-config` is forwarded to every child |
| `factoryd serve` | `-daemon-temporal-address`, `-config` | Enables `GET /daemons`, `POST /daemons/start`, `POST /daemons/stop`. Unset, those routes fail closed; everything else still works |
| `factoryd worker` | `-temporal-address` | See [`USAGE.md`](USAGE.md#temporal-what-runs-every-build) |
| `factoryd doctor` | `-temporal-address`, `-config` | Checks the server is reachable |
| `factoryd reconcile` | `-temporal-address` | Keeps an isolation marker whose workflow is still running; without it, Temporal-mode markers are always kept |

- `-config` on `daemon`, `supervise`, `serve`, and `doctor` wins when set.
  Empty searches the default session-config paths.
- `daemon` and `supervise` take the build-tuning flags of a single-ticket run
  (`-build-app-script`, `-max-rounds`, `-timeout-minutes`,
  `-verify-command`, `-conformity-policy`).
- Sandbox resource knobs (`sandbox_docker`, `sandbox_memory`, etc.) and
  build/verify attempt limits come only from the session config
  ([`USAGE_REFERENCE.md`](USAGE_REFERENCE.md) config-key table). They are
  fixed per daemon, whatever a request asks for.
- A daemon servicing `github-copilot` or `chatgpt-codex` requests needs
  that route's credential source, set in the session config's `routes:`
  entry: `github_token_file` (or `GITHUB_COPILOT_TOKEN`), or
  `codex_auth_file` (read fresh on every launch).

### Observe a run in Temporal

- **Temporal Web UI** (http://localhost:8233): executions, history,
  retries. A request is `factoryd-request-<request id>`. A run's workflow
  id is its run id. Under
  `-repository`, the run is a child of `RepositoryOwnerWorkflow`
  (`repo-owner-<hash>`); filter on `FactoryRunID` or open the owner and
  follow its child link.
- **Memo and search attributes.** Every `RunWorkflow` carries a Memo
  (`ticket`, `project`, `repository`, `factoryd_run_id`). A run without
  `-repository` also sets Keyword search attributes `FactoryTicket`, `FactoryProject`,
  `FactoryRunID`. `docker-compose.temporal.yml` registers them (its
  `temporal-search-attributes` service). On a server without them,
  `factoryd` retries the start once without them and logs a hint.
  `-repository` children set only the Memo.
- **Live progress.** Query `run-progress` (UI "Query" tab, or `temporal
  workflow query`) for the current stage, its start time, `base_sha`, and
  `state`. Long Activities' heartbeats carry the same stage. A plain
  `factoryd <run> -temporal-address` also prints stage changes to stdout
  (checked every 15s).
- **`factoryd status`.** `-json` includes `temporal_ui_url`; the plain
  table prints it under the run. Set only when the server uses gRPC port
  7233. The run record (`<data-dir>/runs/<id>/run.json`) holds
  `temporal_workflow_id`, `temporal_run_id`, `temporal_task_queue`, and
  `temporal_address`.
- **Branches.** An isolated `RunWorkflow` works on
  `factoryd/<slug>-<shorthash>`, where `<slug>` is the run id through a
  sanitizer and `<shorthash>` is a short hash of the workflow id.
- The console, `kill-switch`, release decisions, and `factoryd retry`
  (including its PR-only re-open for an accepted ticket with no PR) work
  the same with and without `-repository`.

### Baseline verify

`RunWorkflow` runs `RunBaselineVerifyActivity` between `PreflightActivity`
and `RunBuildActivity`, on every history that has the `baseline-verify`
version marker. A history recorded before the marker replays without it.

| | |
|---|---|
| Command | The run's verify command (`Verify-Command:`, else `-verify-command`), once, `sh -c` in a fresh sandbox with no model route; registry proxy and compose services as for canonical verify |
| Where | The run's own worktree, just created at the base commit. What the command left there (anything `git status` reports that it did not report before) is put back to `HEAD` or deleted before the build, by host git commands that run no hook and no file-system monitor from the repository's configuration |
| Attempt | Kind `baseline_verify`, log `<execution key>.attempt-1-baseline_verify.log`, in the run's attempt list and progress feed (stage `baseline_verify`) |
| Record | `baseline_verify.json` in the run's directory, written once the command has run to an exit code, before the Activity returns; `run.json` carries it as `baseline_verify`. A launch that failed as infrastructure (a timeout, a lost worker) leaves no record and halts the run as any failed launch does |
| Failing tests | Read from the last 8 MiB of the log: `--- FAIL: <Test>` (a subtest counts as its top-level test) and pytest `FAILED`/`ERROR <file>::<test>` (parameters dropped); a Go compile error (`<file>.go:<line>:<col>:`) and a pytest collection error (`ERROR <file>.py`) name a file. No other runner's output is read |
| Named by the ticket | The run's ticket (`-spec`, `-ticket-file`), the text its build is given, holds the test's full name or the last component of a pytest node id, with no identifier or path character on either side; a Go test may be followed by `/<subtest>`; a file is named by the path the log printed, or a longer path ending in it |
| A path the ticket creates | When no test failed by name: the log's first recognised error line names, as a path or part of one, a file from the ticket's `Allowed-Files:`/`Required-Changed-Files:` (patterns left out) that does not exist in the worktree before the command runs, or a directory above it that does not exist either. Recorded as `needs_created` |
| Left paths | Right after the command ran, before the restore above: the paths `git status` reports now that it did not report before (untracked files the repository does not ignore, tracked files the command changed or deleted). Those that the `diff_scope` gate would flag for the ticket's `Allowed-Files:` (the same harness-by-product exclusion and matcher as `policy.EvaluateRun`; a ticket with no `Allowed-Files:` has no such gate and records none) are `left_out_of_scope`. The factory commits what a build leaves, so each would quarantine every build of the ticket. Not added to `baseline_failure.md` |
| Told to the build | `baseline_failure.md` in the run's directory, staged read-only and named to the build script as `--baseline-failure`: the factory's sentence, the exit code, the verify command and each test in the words the ticket names it with, or the path the ticket creates (`named_as`). No text from the command's output |
| Halt | Application error type `BaselineVerifyFailure`, `halt_reason_code` `baseline_verify_failed`, not retried, not counted toward the repository's stop line. The same halt, with `left_out_of_scope_count` above 0, when the command passed or failed as the ticket expects but left paths that `diff_scope` would flag (see Left paths). A rejected compose file found by this launch halts as `compose_services_rejected` |
| A run that follows another | Nothing is launched; the earlier run's record is copied, with `inherited_from`, and the note is written again for the build. Two cases: a resumed run (the halted run it resumes), and a run with a `-diff-base` other than its own starting commit (the request run that produced that starting commit: a retry on the attempt's branch, a corrective round, a PR-review round). The baseline runs after all when the run it follows never got past its own baseline (no record, or one that halted it, and no build attempt: the kept worktree is first put back to the base commit), or when no request run produced the starting commit |

`baseline_verify` fields: `command`, `base_sha`, `exit_code`, `passed`,
`failing_tests` and `unnamed` (at most 20 each, with `failing_count` and
`unnamed_count`), `named_as`, `needs_created`, `first_error` (when no test was named), `expected`,
`left_out_of_scope` (at most 20, each a clean line of at most 200 bytes) and `left_out_of_scope_count`, `inherited_from`, `log_path`, `duration_ms`.

### Oracle-approved runs

A run with an approved oracle (`submit -draft-oracles`, or
`-reference-oracle-*` on a single-ticket run) runs extra Activities in
`RunWorkflow`:

| Activity | What it does |
|---|---|
| `RunNamedGateActivity` | Runs the `reference_oracle` gate (and other named gates). The oracle gate also runs the runtime canary, so its timeout is doubled |
| `CommitOraclesActivity` | After every gate and conformity review passes, commits the oracle files plus `.buildgate/oracles.json` on the factory host. With `-no-commit-oracles`: validate only, nothing committed |
| `RunPostOracleCommitVerifyActivity` | Re-runs canonical verify, full suite, and named gates on the committed tree. Failure quarantines the run |

`-no-commit-oracles` travels as the `no_commit_oracles` input field; older
workers ignore it, so the opt-out holds only once every worker is current.

Live check through Temporal (fails a fixture whose run has no workflow id):

```sh
LIVE_SMOKE_ONLY=oracle LIVE_SMOKE_TEMPORAL=localhost:7233 scripts/live-smoke.sh
```

### What every run shares

- `-temporal-address` and `-repository` work on `factoryd <run>`,
  `daemon`, `supervise`, and `POST /runs`.
- Full-suite gate, sandbox/registry-proxy
  flags, and credential checks apply to every run. Docker containment is
  unconditional; see
  [`USAGE.md` § Run a ticket](USAGE.md#run-a-ticket) and
  [`containment-matrix.md`](containment-matrix.md).
- The registry-proxy policy travels in the Workflow input. Whichever
  worker runs the Activity launches one proxy per sandboxed Activity. Its
  scratch cache (`<data-dir>/scratch/<run-id>/<launch>`, one directory per
  worker container) is removed when that container exits; `factoryd daemon` also reaps leftovers at startup and on each
  reclaim scan.

## Appendix: existing repo without `quickstart`

`quickstart` defaults to `-preflight-profile brownfield`, which tolerates
a repo without the onboarding docs. By hand:

| Repo has its own `ARCHITECTURE.md`? | What to do |
|---|---|
| Yes | Skip `onboard` (it refuses, writing nothing, if any of `spec/spec.md`, `spec/contract.md`, `ARCHITECTURE.md` exists). Run `factoryd <run>` with `-skip-project-check`; `-workspace` can be the repo root as long as the data dir is outside it. |
| No | `factoryd onboard -project <name> -root <repo>` scaffolds the three files and pre-fills a detected `Verify-Command`; add `-write-factory-yml` for a `.factory.yml` with `preflight_profile: brownfield`. (`-workspace <repo>/workspace` still works but is deprecated.) |

Under the strict profile, a run whose repo has none of the three files
refuses to start and prints the exact `factoryd onboard ... -root <repo>
-write-factory-yml` command. If only some exist, you get the normal
per-check failure instead. `init` and `onboard` run `doctor` first
(`-skip-doctor` to bypass; `-sandbox-docker`/`-sandbox-image` to check the
values a run will use).

| Gotcha | What to do |
|---|---|
| Writing a ticket by hand | `factoryd ticket-template [-o <path>]` for a skeleton; `factoryd check-ticket <path>` to validate the header lines. Point `-spec` at it. `-ticket-file` is a separate from-scratch format; leave it unset. Both run paths refuse a malformed or near-miss header (`Verify-command:`, `Allowed_Files:`) (`TestPreflightRefusesNearMissTicketHeader`). |
| Module root is a subdirectory | Point `PROJECT_DIR` (for `make project-sandbox-image`) and `-verify-command` at it. `doctor -workspace <repo>` warns when it finds the manifest one level down (`TestDoctorCheckMonorepoModuleRoot`). |
| Need a worker image | `make sandbox-image` builds one locally (every image is built from source, never pulled); pass its printed ref as `BASE_IMAGE` to `make project-sandbox-image`. |
| The project needs another Go or Python version | `make project-sandbox-image` installs it: see Project toolchains below. |
| Model host only on Tailscale | Use the Tailscale IP or full `*.ts.net` FQDN for the route's own `upstream`; the container may not share the host's resolver. `doctor` resolves the host from inside a sandbox container and fails with `container DNS cannot resolve ...` (`TestDoctorCheckRelayUpstreamHostResolvesInSandbox`). |

### Project toolchains

A build has no network and downloads no toolchain (`GOTOOLCHAIN=local`), so it
has the versions in its sandbox image and no others. When a repository
declares a Go or Python version the configured image lacks, the build derives
an image that has it and runs in that. Nothing is configured.

| Tool | Read from, at the top of the repository | The image satisfies it when | When it does not |
|---|---|---|---|
| Go | `go.mod`: the `go` line, and the `toolchain` line when it is newer | Its Go is at least the `go` line | That version is installed from the official `golang` image |
| Python | `.python-version`, else `requires-python` in `pyproject.toml` | Its Python matches the numbers `.python-version` states, or meets every `requires-python` clause | That version is installed from the official `python` image built on the configured image's distribution; `python3`, `python` and `pip` become it |
| Node | `.nvmrc`, else `.node-version` | Its Node matches the numbers stated | Nothing is installed: the coding agents run on the image's Node. The run and `submit` print a warning |

```text
a build starts (worker, or a single-ticket run)
        |
        v
read the repository's declarations --- none, or the configured image has them ---> configured image
        |
        | a Go or Python it lacks
        v
already derived for this image and these versions? --- yes ---> that image
        |
        | no: build it (official toolchain image over the configured one),
        |     push it to the local registry          [a minute or two, once]
        v
the build runs in localhost:5050/buildgate-toolchain@sha256:<digest>
```

| Fact | Detail |
|---|---|
| What you see | `sandbox image: installing Go 1.27: go.mod declares "go 1.27" and the sandbox image has go 1.26.8`, then the derived image's reference, in the run's log |
| What goes into the derived image | The configured image and the official toolchain image. No file of the repository, and none of its code runs to build it. Its dependencies come through the registry proxy during the build, or from `make project-sandbox-image` below |
| Python and buildgate's own scripts | The build scripts run on the worker's `python3`. Deriving an image with another Python runs those scripts' test suite on it first and refuses the run if they fail |
| Checking ahead | `factoryd doctor -target-repo <repo>`: one row per declaration, saying what a build will do |
| Turning it off | `FACTORYD_AUTOSTART=0`: the configured image is used as it is, `doctor -target-repo` fails the row and `submit` warns |
| A declaration with no version to install | An upper bound alone (`requires-python = "<3.12"`) refuses the run, naming it; add `.python-version` |
| An alias | `lts/*`, `system` and other names with no number are not compared |
| Not covered | Acceptance-test drafting (`-draft-oracles`) runs in the configured image |
| Removing them | `factoryd uninstall` removes every `localhost:5050/buildgate-toolchain` image |

`make project-sandbox-image PROJECT_DIR=<repo> BASE_IMAGE=<worker ref>` builds
the same toolchains into an image that also carries the project's
dependencies (Go modules, npm packages, pip packages), for a build that must
not fetch them. It prints a reference to set with `factoryd configure-images
-sandbox-image <ref>`; `sandbox_image` is per profile, and `.factory.yml`
cannot name an image.

### Private Go modules

A build has no credential and no route to a module's own host, so it cannot
fetch a module the public proxy does not have. Such modules are fetched for
it on the host, by your own `go` command. Nothing is configured in buildgate:
what counts as private is what your Go settings say.

```text
a run is dispatched (with the registry proxy, the default for a model-backed build)
        |
        v
read every go.sum of the commit the build starts from (not under vendor/ or testdata/)
        |   module paths, versions and hashes only
        v
keep the ones your Go settings mark as private:
        |   GOPRIVATE, GONOPROXY, GONOSUMDB; all of them when GOPROXY is your own
        v
your go command fetches those on the host
        |   your credentials; an empty directory, so it reads no file of the repository
        v
~/buildgate/gomodules/views/<key>   this list's modules, each with the hash go.sum gives it
        |   mounted read-only into the run's registry proxy
        v
the build's go asks the registry proxy, which answers these from that directory
and asks the public proxy only about other modules
```

| Fact | Detail |
|---|---|
| What you see | `go modules: fetching N private module version(s) go.sum lists, with this machine's Go settings`, then how many are served and which were left out, in the run's log. Nothing is printed for a repository with no private module |
| Which Go settings | `go env` on this machine: the environment of the process that runs the build, and what `go env -w` wrote. A worker under launchd does not have your shell's exports, so set `GOPRIVATE` with `go env -w GOPRIVATE=...` |
| Nothing marked private | Nothing is fetched. A `GOPRIVATE` set only in the repository's Makefile is not a setting of this machine |
| A module that cannot be fetched | Named in the log and left out of that run. Fix this machine's access (your own `go mod download` must work), then `factoryd retry <id>`: the next run fetches again |
| Checksums | A fetched module is served only with the hash `go.sum` gives it, and the build's `go` verifies it against `go.sum` again |
| `GOPRIVATE` in a Makefile | The build still uses the proxy for every module (`GONOPROXY=none` in its environment) |
| Public proxy | Asked nothing about a module served this way, so its name does not leave the machine through the build |
| A dependency the build adds | Not served: the list is the base commit's `go.sum`. A ticket that adds a private dependency needs it in `go.sum` first |
| A repository that vendors its modules | Needs none of this unless its verify command resolves modules anyway (`go mod tidy`, `go mod vendor`, `-mod=mod`) |
| Turning it off | `FACTORYD_AUTOSTART=0`, or a run without the registry proxy |
| Disk | One shared download cache and one small view per list, under `~/buildgate/gomodules`; safe to delete when no build is running |
| The registry proxy image | Must be the one built with this factoryd (`make install` builds both). An older one refuses the option, and the run stops saying so |

Single-ticket run example (the model route itself comes from session config's
`routes:`/`models:`/`roles:`, not a flag -- see Model routes above):

```sh
factoryd \
  -workspace ~/code/my-app \
  -spec ~/code/buildgate/data/tickets/my-feature.spec.md \
  -ticket my-feature \
  -skip-project-check \
  -verify-command "cd backend && go vet ./... && go test ./..." \
  -sandbox-image localhost:5050/project-worker@sha256:<digest>
```

with, e.g.:

```yaml
routes:
  local:
    upstream: http://<model host>:8080
models:
  m:
    id: "<id from GET /v1/models>"
    routes: [local]
roles:
  execution:
    model: m
```

Run `factoryd doctor` with the same config first; it catches the gotchas
above in seconds.
