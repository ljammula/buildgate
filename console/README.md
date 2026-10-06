# Factory Console

Flutter Web operator console for `factoryd`: approve specs, oracles and
plans; watch runs. To just use it, open the `View:` link `factoryd submit`
prints, or run `factoryd console`: both start `factoryd serve` for the data
dir when none is running. See [`DEMO.md`](../DEMO.md) for a walkthrough,
[`README.md`](../README.md) for the system, and
[`USAGE.md`](../USAGE.md#observe-and-control) for the path from
`factoryd submit`/`worker` to this console.

## Running it

`factoryd serve` embeds the built console and serves it on the API's own
origin (no Flutter toolchain, no second port, no CORS setup):

```sh
factoryd serve    # startup log prints: console: http://<addr>/#t=<token>
```

While it runs, `serve` records its address in `<data-dir>/console-address`
(with its pid; removed on exit, ignored when the pid is dead). `factoryd
submit` and `factoryd console` build their console links from that record
for the same data dir; without a live `serve` (or `-console-base-url` /
`FACTORYD_CONSOLE_URL`) they print no link rather than guess a port.
With a stable start token (see below) the startup log prints the link
without the `#t=` fragment; `factoryd console` prints the signed-in one.

Building `factoryd` yourself: `make install` also builds the console when
`flutter` is on `PATH` (`console-build-optional`); without Flutter the
binary serves a placeholder page instead. `make console-build` builds it
explicitly. Either way `internal/consoleweb/dist/` tracks only `.gitkeep`,
so rebuilding leaves `git status` clean.

| Command | What it does |
|---|---|
| `make console-build` | `flutter build web`, copied into `internal/consoleweb/dist/` |
| `make console-test` | `cd console && flutter test` |
| `flutter analyze` | Static analysis (run from `console/`) |

### Development workflow

For hot reload and breakpoints, use Flutter's dev server against a
separately running API:

```sh
flutter run -d chrome --web-port=8091 --dart-define=API_BASE_URL=http://localhost:8090
factoryd serve -cors-allow-origin=http://localhost:8091
```

`API_BASE_URL` defaults to `http://localhost:8090`. `serve` sets no CORS
headers unless `-cors-allow-origin` names the exact console origin; a
missing or mismatched origin fails closed with a browser CORS error.

## Screens

```text
Request board (/) --+-> Request detail (/requests/<id>)
  |                 |     Pipeline stepper, spec/plan text,
  |                 |     Edit / Approve / Request changes
  |                 +-> New request (/requests/new)
  |                 +-> Triage (keyboard review queue)
  v
Runs --+-> Run detail: Timeline, evidence, override
       +-> New run, project release, project stats, Operations
```

**Request board.** Every request (`GET /requests`): waiting-on-you first
(oldest first), then in-progress (most recently updated), then done or
failed. Refreshes every 5s. Banners:

- **`worker` not alive** (`GET /queue-run`, reads its heartbeat
  file): shown for a stale heartbeat, or for no heartbeat once a request
  is waiting on a state only `worker` can advance.
- **Release policy denies every PR** (`release_policy_warning` in
  `GET /console-config.json`): the configured release policy can never
  allow a release — the same check `factoryd doctor` warns about.

**Request detail** (`GET /requests/{id}`). A **Pipeline** stepper
(submitted through done, each step timestamped and attributed), then the
drafted spec or ticket plans. In `spec_review`/`oracle_review`/
`plan_review`:

| Action | Route |
|---|---|
| Edit (spec) | `PUT /requests/{id}/spec` |
| Edit (ticket plan) | `PUT /requests/{id}/tickets/{n}` |
| Approve | `POST /requests/{id}/approve` |
| Request changes | `POST /requests/{id}/reject` |

**Triage.** Keyboard list of pending spec and plan reviews: `j`/`k` move,
`a`/`r` approve/reject through the same confirm flow. No approve-all.
`oracle_review` is excluded (a keypress can't show oracle hashes); open
those from request detail.

**New request** (`/requests/new`, `POST /requests`). Workspace, request
text, optional "Draft oracles", and an Advanced section (verify command,
full-suite command, preflight profile) hinting each workspace's
`.factory.yml` default from `GET /workspaces`. The workspace is
allowlisted: it must already be some request's workspace or be listed in
the session config's `workspaces:` key. See
[`USAGE.md` § Five repos, zero terminals](../USAGE.md#five-repos-zero-terminals-submit--worker).

**Runs.** Run list (`GET /runs`),
refreshed every 5s, with project and elapsed time per row. Run detail
leads with a live **Timeline** (`GET /runs/{id}/progress`): workspace
prep, preflight, build, verify, gates, evidence, conformity review,
evaluate, finished — each pending/running/passed/failed, plus a status
strip ("stage · round n/m · elapsed · state"). The live build log renders
each worker `FACTORY_PROGRESS` line as one readable step, like `factoryd
watch` ("round 1/3 started", "agent  read: main.go", a failed round's
reason); any other line stays verbatim. Build-step round/action lines are
the sandbox's own stdout: untrusted and informational only. Each attempt
card names its kind (build, verify, review, ...); the combined review
decodes its exit code ("Exit code: 40 (spec conformity passed, code review
passed)"). A red **stalled** chip appears once a non-terminal
run's progress is silent for 5 minutes (`internal/progress.StallAfter`,
decided server-side); an amber **waiting: ...** chip when the factory has
already explained the silence (e.g. queued behind another run on the same
repo).

**Release screens.** The run-level screen shows a run's release decision
and the project kill switch's state and history; the project-level screen
(shield icon on the run list) shows just the kill switch. Both are
read-only: engaging the kill switch stays on the `factoryd kill-switch`
CLI so it never depends on a healthy `serve`.

**Costs on a subscription route.** The request board, Ops and project
stats screens show a per-run or aggregate dollar figure even on
`chatgpt-codex`/`github-copilot` routes, labeled so it's never mistaken
for a real charge: a single run reads "(API-price est.; billed to your
subscription)"; an aggregate that mixes routes reads "(includes
subscription-billed runs; API-price est.)" (`console/lib/request_cost.dart`).

## Oracle review

For a request submitted with `-draft-oracles`, the stepper shows
`oracle_drafting` and `oracle_review` between spec and plan (skipped
otherwise). See [`USAGE_REFERENCE.md`](../USAGE_REFERENCE.md), "Staged
oracles".

- **`oracle_drafting`**: shows the drafting job's status, including a
  previous pass's outcome after a rejection.
- **`oracle_review`**: lists every file of the request's `oracle/`
  directory (`GET /requests/{id}/oracle[/{name}]`). Only
  `RUN_COMMAND.txt` is editable (`PUT .../oracle/RUN_COMMAND.txt`).
  **Approve stays disabled until every file has been expanded and its
  displayed content matches the listed hash**; those hashes are sent with
  the approval. Invisible or bidirectional Unicode is shown escaped. With
  no files the button reads "Approve (skip oracle)".
- **Skipped oracle**: approving past a failed drafting pass puts a
  "Warning: ..." callout on every later screen for that request.
- **`plan_review`**: each ticket's `<NNN>.oracle/` files are shown
  read-only (`GET /requests/{id}/tickets/{n}/oracle[/{name}]`); plan
  approval likewise waits until all are shown and hashed.

The API refuses an `oracle_review` or `plan_review` approval that omits
the hash of any oracle file it would pin. The CLI's `factoryd approve` is
the deliberate exception.

## Tokens and write access

| Dart define | Used for |
|---|---|
| `API_START_TOKEN` (or `API_AUTH_TOKEN` fallback) | `POST /runs`, "Check project setup", Operations, release and stats screens |
| `API_OVERRIDE_TOKEN` | Run override action |

**A start token needs no dart-define.** With `FACTORYD_API_START_TOKEN`
unset, `serve` uses a token and prints it in the console link
(`http://<addr>/#t=<token>`). On first load the console
(`start_token_web.dart`) stores the `#t=` fragment in `localStorage` and
strips it from the address bar, keeping the path, so a deep link like
`/requests/<id>#t=<token>` works too. Start-token routes still fail
closed (403) without a valid token. A dart-defined token always wins
over a stored one.

Where the token comes from:

| Launched by | Token | Survives restart? |
|---|---|---|
| Plain `factoryd serve` | Fresh per process | No: re-open the newly printed link |
| `factoryd quickstart` (not `-no-serve`), `factoryd install-service` | `<session config dir>/serve-start-token` (mode `0600`) | Yes |

`factoryd console [-open]` reprints (and optionally opens) the tokenized
link. A 401/403 after a restart usually means a stale stored token; the
error callout says so.

**Write controls** (Approve, Request changes, Edit, Retry, Cancel, New
request's Submit, run override) are enabled when `RunApi.canWrite` is
true: an `API_OVERRIDE_TOKEN` is baked in, **or** `GET
/console-config.json` reports `"writes_enabled": true`. A stock console
on a loopback-bound `serve` gets writes with no token: the server itself
allows them after its `Host`/`Origin` check (see `safety-contract.md`,
"Console loopback writes"). Otherwise write buttons stay visible but
disabled, with a note naming the fix (bind `serve` to loopback, or
configure an override token). A write the server still refuses shows its
403 reason in the error callout.
