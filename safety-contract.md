# Factory safety contract

Status: normative Phase 0 contract (2026-08-27; condensed 2026-09-25)

This document is the reviewable safety boundary for unattended factory work.
It describes what the factory may do, what it must refuse, and how a run
reaches a terminal state. It is intentionally independent of any particular
Go package, Temporal deployment, runner, or operator console. A control marked
`partial` or `not yet implemented` is a known prerequisite for unattended
operation up to an accepted, mergeable state, not permission to bypass that
control. Merge and deploy stay outside that scope permanently (decided
2026-09-08).

This file states rules and names where they are enforced. The review history
behind each rule (dates, findings, incident write-ups) lives in git history
and `CLAIMS.md`'s gap register.

## Scope and trust boundaries

The factory control plane owns workflow truth, policy decisions, credentials,
durable evidence, and release decisions. A worker (including the coding agent,
build scripts, test commands, and model client) is untrusted and may be
incorrect, compromised, or terminated at any point.

The trust boundaries are:

| Boundary | Trusted side | Untrusted side | Required rule |
|---|---|---|---|
| Acceptance | Factory-owned canonical oracle | Agent-authored tests and reports | Only the external canonical oracle can satisfy verification. |
| Filesystem | Control-plane data and host | Selected task workspace | The worker can write only the selected workspace and its own per-launch, disk-backed scratch cache (Go build/temp and module caches), which no other container -- canonical verify in particular -- ever mounts; Compose bind inputs are materialized read-only from the immutable base commit; evidence is written by the factory. |
| Network | The run's OpenShell network policy | Worker process | The worker has no network interface on any step; no direct host or cloud access. Its only egress is the OpenShell supervisor, which admits a connection only when the run's policy names the destination and the calling binary. |
| Credentials | Control plane / secret broker | Worker and generated subprocesses | Host SSH, cloud, GitHub, and override credentials never enter a worker. A route's model credential is stored in the OpenShell gateway's credential store on the sandbox VM; the worker holds a placeholder, and the supervisor substitutes the value only on a request to an endpoint the route's profile binds. The supervisor does not strip a credential header the worker writes itself. factoryd pushes the current host-file value immediately before each launch (`sandbox.RunThroughRuntime`) and refuses a launch whose credential expires within the step's budget (`RoutePolicy.RouteCredential`). |
| Container boundary | Host runtime | Worker container | The worker has no Docker socket and cannot control sibling containers. The OpenShell gateway holds the Docker socket; the worker cannot reach the gateway API. |
| OpenShell gateway | factoryd's `LaunchSpec` validation and host control | Worker container; Compose sidecars | The gateway is host-root equivalent inside the sandbox VM: UID 0, the Docker socket, and the operator's home directory mounted read-only. The gateway refuses a bind whose source it cannot see, and a worktree binds its repository's `.git`, so the gateway must see every repository as well as the data root (`~/buildgate`); in a colima VM set up as USAGE.md describes, the home directory holds only what the VM shares (the data root and the repositories). The mount adds nothing the Docker socket does not already give it. It listens on VM loopback only (`127.0.0.1:17670`, host-networked) and is attached to no network a sidecar or worker can join; a client needs the factory's client certificate (`~/.config/factoryd/openshell/mtls/`, directory 0700, `tls.key` 0600). It launches a worker only from a `LaunchSpec` the factory validated: OpenShell's resource admission is off (bind mounts need it off), so `LaunchSpec.Validate` and `withResolvedMounts` are the only checks on mounts (workspace and inputs resolved and contained, Git metadata read-only, no Docker socket). factoryd's host control refuses to stop or restart it while a request or sandbox is active (`hostcontrol.ActiveBuilds`); a restart by any other path is detected (SC-017). |
| Supervisor | OpenShell's pinned supervisor image | Worker process | One per sandbox, host-networked, uid 65534, read-only root. It terminates the worker's TLS, holds resolved credentials in memory, and dials upstreams from the VM's network namespace. It opens no TCP listener on the VM: only Unix sockets under `/run/openshell`. |
| Spend meter | `factoryd-meter` (`internal/meter`), called by the supervisor for every model request | Request and response bodies of the worker's model calls | Counts each run's tokens and cost and refuses requests past the ceiling. It parses untrusted bodies, holds no credential, writes each sandbox's ledger under `~/buildgate/meter-ledgers/` where no worker and no other run's sandbox writes, fails closed (`on_error: fail_closed`), denies WebSocket upgrades, and refuses a response it cannot read as a stream. Exposure as it is: a plaintext gRPC listener published on VM loopback only (`127.0.0.1:17672`), with no TLS and no caller check; no worker or sidecar can reach it, and any process on the VM or the Mac's loopback can. |
| Compose sidecars | The operator's `compose_services_*` settings and `internal/composeservices`' field allow-list | The target repo's compose file and `.env` at the run's base commit | Repo content can only select allow-listed images, Linux platforms (`linux/amd64` or `linux/arm64`), and fields; fixed `container_name` values are inert and never emitted; relative bind inputs are read from the immutable base commit, reject links/special files/path escapes, and are mounted read-only (a source the base commit lacks becomes an empty per-run named volume); it never widens a sidecar's containment (a service's own `mem_limit` is accepted only at or below the operator's `compose_services_memory`). Operator-supplied worker endpoint variables are host/session configuration, sorted and collision-checked, cannot override factory/sandbox/credential/cache/network variables, and never enter Temporal history; a value that names a service as a host is refused, since the worker resolves no service name. They are stored in the gateway's sandbox record and readable through the gateway API by any holder of the factory certificate. Sidecars stay on the factory-owned internal network; the worker does not join it and reaches each sidecar by address and port through the supervisor. A compose file the allow-list rejects halts the whole run before its build (`compose_services_rejected`, naming each service and the fix), never a partial or silent launch. Published `ports:` are never published on the host: each fixed TCP mapping becomes a forward on the worker's own loopback to that service's address on the run's internal Compose network, and two services publishing one port reject the file. A compose edit made by the worker is never launched in the same run. |
| Release | Factory release policy | Agent prose and reviewer verdicts | Merge/deploy requires the complete evidence package and policy pass. |
| Control-plane API | `factoryd serve`'s daemon configuration (resource ceilings, release policy, sandbox image policy) | An authenticated `POST /runs` caller | A bearer token proves authentication, not trust. A caller-supplied field that selects what code runs inside the boundary is never accepted as given: `sandbox_image` is allow-listed by operator configuration (`-api-allowed-sandbox-images`), as `sandbox_docker` already is. No caller-supplied field can select host execution. |
| Workstation queue | Docker containment (always applied) | `factoryd submit`/`worker` requests (each ticket build runs from an in-memory entry) | No path has host execution: every ticket build resolves to a configured, digest-pinned `-sandbox-image` (built from source, no built-in default) or is refused before any container launches. This holds for every entry point (bare `factoryd <run>`, `serve`, the API, Temporal), not just `worker`. |
| Forge / PR review | Factory release policy and an operator-configured reviewer trust list | Branch pushes, draft-PR open, the `gh pr ready` flip, `gh pr edit --base` retargets, and reviewer comments | Each push, draft-PR open, and ready flip requires `release.Decision.Allowed`, re-checked immediately before the side effect — including a `factoryd retry` of a PR-open-only failure, which re-runs the decision against the run's frozen policy, verifies `refs/heads/<branch>` still equals the accepted `ResultSHA`, and pushes an explicit `<ResultSHA>:refs/heads/<branch>` refspec (the normal open path pins the same way). A reviewer comment triggers a corrective build only when its author is on the explicit `-pr-trusted-authors` allow-list; an ignore-list is not sufficient. A multi-ticket request's ticket N (N>1) draft PR may open stacked on ticket N-1's own still-open branch (`-pr-base`) rather than the default branch; `advancePRReadyOrApproved` refuses the ready flip while its base is another ticket's branch, and once ticket N-1 merges, `gh pr edit --base` retargets ticket N's PR onto ticket N-1's own base only after the ready flip's checks (a fresh `release.Decision.Allowed` and a PR head equal to that run's `ResultSHA`); a failed or refused retarget leaves the PR draft, is retried on the next poll, and never halts the request. |
| Console loopback writes | `factoryd serve`'s bind address and the operator's own local processes | Any page loaded in the operator's browser | On loopback with no override token, request-write routes accept same-origin JSON without a token; `POST /runs/{id}/override` and start-class routes always need a token. See [Console loopback writes](#console-loopback-writes) below. |
| Console-originated request creation | The operator's `workspaces:` session-config list and the factory's data directory | `POST /requests` | Takes the request-write gate, never `authorizeStart`; `workspace` is never accepted as an arbitrary host path. See [Console-originated request creation](#console-originated-request-creation) below. |
| MCP endpoint | The operator's `<config name>.mcp-token` file (created by `factoryd mcp`, mode 0600, beside the session config, one per profile) | `POST /mcp` and the model driving the MCP client | Off until the token file exists; every call needs the token, with no loopback relaxation. Its tools are reads and `POST /requests` only: no tool approves, rejects, retries, resumes, cancels, edits or overrides. See [MCP endpoint](#mcp-endpoint) below. |
| Untrusted worker output | `internal/sanitize` | Agent-authored log/report text reaching triage, halt reasons, notifications, `status`, or the console | Text is stripped (ANSI/OSC, control and Unicode format characters, invalid UTF-8) and secret-redacted (`sanitize.Text`/`sanitize.Line`; Python mirror `build_app.redact`/`single_line`) before display, and shown as a quoted log excerpt, never as a factory verdict or claimed provenance — the log-writing process shares the worker's container and uid, so the factory cannot attest who wrote a given line. |
| Model-listing credentials | Factory route-validation code, reused | `doctor -list-models`, `doctorCheckCopilotModelListed`, the Copilot token-exchange client | These host-side calls send only what a launch's route would admit: validated via `sandbox.RoutePolicy.ValidateUpstreamScheme` + `sandbox.CredentialSafeForUpstream` (the same check `RouteSpec.Validate` applies), redirects refused (`meter.NoRedirectCheckRedirect`), no credential over plaintext and none when `routes.<name>.allow_no_credential` is set. |
| Model routes | Session-config `routes:` and the host-side resolver in the process launching that route's worker | Request model choice; Temporal Workflow input | A route's credential is resolved host-side only for that route's own launch; Workflow input names a route, never a credential source, and the Worker refuses any policy that differs from its own route and role model in any field except a tightened ceiling, then resolves that route's credential itself. The meter policy a sandbox is created with is derived from that same checked policy (`RoutePolicy.MeterConfig`). The route's network policy admits the harnesses' model executables (`harness.ModelBinaries`, the union across harnesses), not one harness's. |
| Skills | The operator's `skill_dirs:` folders, buildgate's built-in skills, and `roles.<role>.skills` names | Request input; the target repo's own project skills; the worker between rounds | Only a host-side, hashed, read-only snapshot of the role's named skills reaches the worker (SC-016); a same-name repo skill refuses the launch; harness homes stay per-session and get fresh copies each round. Skill content is operator-trusted instruction text, vetted like a dependency. |

```text
  +----------+  mTLS   +----------+  Docker  +----------+
  | factoryd |-------->| OpenShell|--------->| worker   |
  | (host)   | sandbox | gateway  |  socket  | no       |
  |          | request | (VM      |          | network, |
  |          | from a  | loopback)|          | no       |
  |          | checked |          |          | socket,  |
  |          | spec    |          |          | no creds |
  +----------+         +----------+          +----------+
     |                      |                     |
     | pushes the route's   | credential     only egress
     | credential before    | store               v
     | each launch          |               +------------+   policy:    +-------+
     +--------------------->+-------------->| supervisor |--destination>| model |
                                            | (per       |  + binary    | API   |
                                            | sandbox)   |  + path      |       |
                                            +------------+              +-------+
                                                  |  every model request
                                                  v
                                            +------------+
                                            | meter      |
                                            | ceilings,  |
                                            | ledger     |
                                            +------------+
```

The selected workspace is the only mutable job input. The factory must reject
an unvalidated workspace, repository, ticket, or release target before a
worker starts. A human may inspect or override a quarantined run, but no human
response is required for the default path to continue.

### Console loopback writes

Threat scope: this defends against a *browser*: a hostile page or a
DNS-rebinding attacker reaching the loopback port through XHR, fetch, or a
`<form>` POST. It assumes a single-OS-user host. Another process running as
the same user can already forge these headers, or run `factoryd approve`/
`cancel` directly against the data directory.

Request writes (approve, reject, spec/ticket/oracle edits, retry, cancel,
create; `internal/api.Server.authorizeRequestWrite`):

- Without a token, a request is accepted only when all of these hold:
  - `Host` names this server's own loopback address and port
    (`127.0.0.1`/`localhost`/`[::1]`, `hostMatchesLoopback`). This is the
    DNS-rebinding defense.
  - `Origin` exactly matches that origin, or, when `Origin` is absent,
    `Sec-Fetch-Site` is exactly `same-origin`. A request with neither header
    is refused. This is the CSRF defense.
  - `Content-Type` is `application/json`, which blocks a `<form>` POST's
    encodings.
- The relaxation is disabled on a non-loopback bind or whenever an override
  token is configured. `serve` never opens write routes on a wider bind
  without a token.
- `POST /runs/{id}/override` (`authorizeOverride`) always requires the bearer
  token. It moves a quarantined run to accepted/halted and records a release
  `Decision`, which is stronger than the drafting-pipeline writes above.
- On loopback, the `Host` check applies to every route, reads included
  (`Server.ServeHTTP`). `-allowed-host <host[:port]>` (`hostAllowed`) adds
  extra `Host` values for reads only, e.g. through an ssh tunnel, reverse
  proxy, or `tailscale serve`. The write relaxation still consults only
  `hostMatchesLoopback`, so a write through an allowed host needs the token.
  `POST /mcp` is reachable through an allowed host and can submit a request
  there, behind its own token ([MCP endpoint](#mcp-endpoint)).

Start-class routes (`POST /runs`, daemon lifecycle, and the `GET` release
and stats routes; `authorizeStart`) get no relaxation. A missing or wrong
bearer token is always a 403. The token is handled as follows:

- **Per-process token.** When `FACTORYD_API_START_TOKEN` is unset and no
  stable token file exists, `serveMain` generates a 32-byte `crypto/rand`
  token that lasts for the life of the process. An operator-set env var
  always wins unchanged.
- **Delivery.** The token is delivered only as a URL *fragment* in the
  printed console link (`http://<addr>/#t=<token>`,
  `consoleLinkWithStartToken`). It is never a query parameter, because
  browsers never transmit fragments (RFC 3986 §3.5).
  `captureStartTokenFromLocation` (`console/src/platform/startToken.ts`) stores
  it in origin-scoped `localStorage`, then strips it from the address bar
  with `history.replaceState`. The token exists in only three places:
  `serve`'s own stdout, that fragment, and that browser's storage.
- **Stable token for a supervised `serve`.** `factoryd install-service`
  creates `<session config dir>/serve-start-token` once, so a restart does
  not invalidate stored tokens (`cmd/factoryd/serve_start_token_file.go`).
  - The file is mode `0600`. It is never written into the LaunchAgent plist,
    and never passed through a child's env or argv: `quickstart`'s `serve`
    child re-derives the path from `-config` and reads the file itself.
  - `serve`, `factoryd console`, and `install-service` all resolve the path
    through the one shared `resolveEffectiveConfigPath`/
    `serveStableStartTokenPathFor` pair.
  - `factoryd console` only reads the file, never creates it.
    `uninstall-service` leaves it in place.
- **Hardening of the stable token:**
  - The token is appended to a printed or opened link only when
    `consoleBaseIsOwnLoopbackServe` confirms the link points at this
    process's own loopback `serve`. A remote `FACTORYD_CONSOLE_URL` gets a
    link without the token.
  - `quickstart` reuses an already-listening `serve` only after
    `quickstartServeVerifiedOurs` attributes the listener to this session
    (`lsof` plus a same-uid pid check). A bare `/healthz` 200 is not enough.
  - `openInBrowser` never puts a token-bearing URL on argv. It opens a
    `0600` redirect file in a fresh `0700` temp dir, then removes it.
  - When the token comes from the stable file, `serve`'s startup log prints
    the link *without* the fragment, because a standing credential does not
    belong in a log.
  - `inspectServeStartTokenFile` `Lstat`s the file and trusts it only if it
    is a regular file owned by the current uid with `mode&0o077 == 0`. The
    file is created with `O_CREATE|O_EXCL|O_NOFOLLOW`, and creation is
    refused inside a git work tree (`tokenDirInsideGitWorkTree`).

### Console-originated request creation

- **Gate.** `POST /requests` (`internal/api.Server.createRequest`) only
  creates a request in `submitted`. It cannot move a run past a safety gate,
  so it takes `authorizeRequestWrite`, not `authorizeStart`.
- **Workspace allow-list.** `workspaceAllowed` requires the canonicalized
  path to meet both conditions:
  - It is a git repository root (`internal/requestsubmit.GitToplevel`).
  - It is either on the operator's `workspaces:` list or already the
    `Workspace` of an existing request in this data directory.
- **Shared validation.** `internal/requestsubmit.Submit` is the one
  validation path shared with `factoryd submit`, and its guards still apply
  to an allow-listed workspace:
  - the workspace is a directory;
  - `-data-dir` is not inside it;
  - no project-basename collision;
  - a resolvable verify command.
- **`GET /workspaces`.** This is a read route (`authorizeRead`). It returns
  the allow-list plus each workspace's verify-command hint. That is nothing
  `GET /requests` and the caller's own filesystem access don't already
  reveal.

### MCP endpoint

Threat scope: the caller is a model, steered by whatever it reads (a chat
message, a spec, a diff). The endpoint gives it the means to start work and
follow it, and no means to pass a gate.

- **Protocol.** JSON-RPC, version negotiation, the Streamable HTTP
  transport (stateless, JSON responses) and argument validation against
  each tool's schema are the official Go SDK's
  (`github.com/modelcontextprotocol/go-sdk`). `serveMCP` checks the token
  before the SDK sees a request. The SDK's own localhost `Host` check is
  off, because it would refuse every tunnelled request; `Server.ServeHTTP`'s
  check, which knows `-allowed-host`, has already run.
- **Off by default.** `POST /mcp` (`internal/api.Server.serveMCP`) answers
  404 until the operator's `<config name>.mcp-token` file exists
  (`config.mcp-token` for `config.yml`). The config's name is in the file
  name, so turning the endpoint on for one profile's `serve` leaves
  another's off. `serve` reads the file on every call (`mcpTokenSource`),
  so `factoryd mcp -rotate` and `-disable` take effect at once; a rotation
  that cannot write the new token leaves the old one working. A file that
  is a symlink, not owned by the operator or readable beyond its owner
  counts as absent. `factoryd mcp` prints the token only beside the
  address this data directory's own `serve` recorded.
- **Token on every call.** The bearer token is compared in constant time.
  `loopbackSameOriginWrite` is never consulted, and the start, read and
  override tokens do not open the endpoint.
- **Fixed tool table.** `mcpTools` is the whole surface:

  | Tool | Route replayed |
  |---|---|
  | `list_requests` | `GET /requests` |
  | `get_request` | `GET /requests/{id}` |
  | `get_run` | `GET /runs/{id}` |
  | `get_run_diff` | `GET /runs/{id}/diff` |
  | `list_workspaces` | `GET /workspaces` |
  | `submit_request` | `POST /requests` |

- **One route per tool.** Each tool is pinned to the mux pattern of its
  route, and `mcpReplay` refuses a call the mux would hand to any other.
  An id is caller text: `events` as a request id would otherwise reach
  `GET /requests/events`, a stream that never ends.
- **No gate decision.** A tool call is replayed through the server's own
  mux carrying an in-process mark (`mcpCaller`). Only `authorizeRead` and
  `createRequest` honour the mark; `authorizeRequestWrite`,
  `authorizeOverride` and `authorizeStart` do not, so the mark opens no
  approve, reject, retry, resume, cancel, editor, override, start or
  release route even if a tool were added that named one.
- **Submit keeps its guards.** `submit_request` passes through
  `workspaceAllowed` and `requestsubmit.Submit` unchanged, so a model can
  submit only against a workspace the operator listed, and the session's
  token, cost and budget ceilings bound what a submission can spend.
- **Submission limit.** At most 5 `submit_request` calls an hour create a
  request (`mcpSubmitAllowed`), counted per `serve` process: text a model
  reads can tell it to submit again and again, and each request has its
  own ceiling. The session's monthly budgets are the durable bound.
- **Reads.** The MCP token grants the reads above even when a read token
  is configured. Spec, ticket and diff text in a result is model-written;
  results are capped at 256 KiB (a request list keeps its newest rows).
- **Reach.** The endpoint is a route of `serve`, so the bind address and
  the `Host` check apply: from another machine it needs `-allowed-host`
  (a tunnel or `tailscale serve`) or a wider `-addr`. `serve` speaks plain
  HTTP; the tunnel carries the token.

## Threat model

| ID | Threat / failure mode | Preventive control | Required response | Current status |
|---|---|---|---|---|
| T-01 | Agent changes or substitutes the acceptance oracle | Keep the oracle outside the worker's writable boundary and hash the exact oracle used | Halt and quarantine on oracle drift or inaccessible oracle | partial |
| T-02 | Worker reads or exfiltrates host/cloud credentials | Do not inject credentials; the route's model credential is held in the OpenShell gateway's store and substituted by the supervisor outside the worker. Residual: a credential header the worker writes itself is forwarded | Halt, quarantine, and asynchronously notify; never wait for approval | partial |
| T-03 | Worker writes outside its selected workspace | Explicit writable-root allow-list and containment/escape checks | Terminate the complete process tree, then halt and quarantine | partial; a reproduced linked-worktree `.git` pointer escape was closed 2026-09-04 (`containment-matrix.md` Filesystem row) |
| T-04 | Worker reaches arbitrary network services | Network-denied baseline; the run's OpenShell network policy names every admitted destination and calling binary | Terminate the worker and quarantine on an escape or policy violation | partial |
| T-05 | Agent claims success without implementing the required change | Factory-owned oracle, diff, scope, and evidence checks | Treat prose as evidence only; fail the policy check | enforced in bounded slices |
| T-06 | A timeout or crash loses the real outcome | Durable attempt, event, artifact, and state records; recovery marks unknown work ambiguous | Halt and quarantine rather than rerun an ambiguous side effect | partial |
| T-07 | Two workers mutate one workspace concurrently | One worker lease / repository owner at a time; isolated runs share the repository, a shared-checkout run holds it alone | Queue the request or halt on an ownership violation | partial; covered for build execution (shared/exclusive repository flock, `factoryd-git.lock` for shared Git metadata / `RepositoryOwnerWorkflow`), the kill switch, and `factoryd intake` (`-pilot-dir` lock, 2026-09-16) |
| T-08 | A failure silently waits for a person | Explicit halt → quarantine → asynchronous notification path | Persist the failure and return; never block on a human | partial |
| T-09 | An override is unauditable or can bypass authentication | Require authenticated operator identity and reason; append durable history | Reject the override and retain the prior state | partial |
| T-10 | Merge/deploy occurs before safety evidence exists | Release policy checks the complete evidence package and phase prerequisites | Keep the run quarantined; no release side effect | partial; every accepted run gets a fail-closed durable decision (plus the `factoryd kill-switch`), which gates the forge/PR surface. No merge or deploy side effect exists, by design (SC-001) |
| T-11 | Cancellation leaves a child process or container running | Cancel the complete process/container tree and record cancellation: the sandbox is deleted through the gateway, with `docker rm -f` of its containers as the fallback when the gateway is unreachable | Halt; quarantine if outcome is unknown | partial |
| T-12 | Workspace history is rewritten during a run | Capture base revision at execution time and require it to remain an ancestor | Halt before evaluating gates or committing further work | enforced in bounded slices |
| T-13 | Target-controlled Compose content escapes the sidecar boundary or misroutes the worker | Parse only allow-listed fields; allow only Linux platforms; ignore fixed container names; materialize relative binds from the base commit as read-only regular files/directories; attach sidecars only to the factory-owned internal network, which the worker reaches by address through the supervisor and never joins; validate and inject operator worker endpoints without protected-key overrides | Reject the entire Compose configuration before build or worker launch and quarantine | enforced in bounded slices |
| T-14 | The sandbox runtime restarts mid-run and starts a worker's command a second time | Host control refuses to stop or restart the gateway while a request or sandbox is active; a per-launch guard lets a worker command run once, and a second start exits before touching the worktree (SC-017) | The step is recorded lost; a lost build keeps its worktree and waits in `resume_review` | partial |
| T-15 | The gateway, a supervisor or the meter is compromised | OpenShell images pinned by digest; mTLS for gateway clients; sandbox JWT; `factoryd doctor` checks the pinned images, the gateway's TLS and mTLS settings, and both services' health | The operator stops the gateway (`factoryd stop -all`) | partial |

## Safety invariants

These are contract identifiers, not suggestions. A change that weakens one
requires a new contract review and an updated machine-checkable test.

- **SC-001 — fail closed before release:** no unattended merge or deploy is
  ever allowed — permanently, by standing operator decision, not a
  precondition still to demonstrate.
- **SC-002 — no host credentials:** workers and their subprocesses receive no
  host SSH, cloud, GitHub, or factory override credentials.
- **SC-003 — no Docker control:** workers receive no Docker socket or equivalent
  host-container control interface.
- **SC-004 — selected workspace only:** worker writes are confined to the
  validated task workspace; control-plane data and the canonical oracle are
  outside that writable root.
- **SC-005 — network deny by default:** worker network access is denied unless
  an explicit, narrow proxy policy grants a particular route.
- **SC-006 — external oracle only:** agent-authored tests, prose, and reviewer
  verdicts are never sufficient to advance a state; canonical verification and
  factory-owned policy checks decide pass/fail.
- **SC-007 — durable attributable override:** every human override records the
  operator, reason, prior state, new state, and time; an override is never a
  required approval step.
- **SC-008 — durable reconstruction:** state, attempts, events, policy results,
  and artifact hashes are durable so a crash can be reconstructed; an unknown
  side effect is ambiguous, not an automatic success or safe-to-rerun result.
- **SC-009 — bounded attempts:** every attempt has time, token, retry, and cost
  limits, and retry classification distinguishes infrastructure, verification,
  oracle-drift, and implementation failures.
- **SC-010 — complete cancellation:** cancellation terminates the complete
  subprocess/container tree and does not leave an untracked child running.
- **SC-011 — exclusive ownership:** at most one worker owns a slice; each
  isolated execution mutates only its own worktree and branch; an execution
  that mutates a shared checkout holds its repository exclusively; factory
  mutations of shared Git metadata (worktree, branch, `info/exclude`) are
  serialized.
- **SC-012 — no oracle mutation:** the job cannot modify its canonical
  acceptance oracle; oracle and verify-surface hashes are recorded with the
  attempt.
- **SC-013 — asynchronous quarantine:** every failed or ambiguous policy check
  transitions to halt/quarantine and emits an out-of-band notification without
  waiting for a human response.
- **SC-014 — release evidence boundary:** an `allowed` release decision is
  reachable only after policy checks validate oracle results, diff
  scope/size, protected paths, and dependency changes; confirm that a
  rollback-plan reference and base/result commit SHAs are recorded; and
  account for override history. The decision gates the forge/PR surface:
  a denied run's branch is never pushed into a draft PR, and an open PR is
  never marked ready once its decision is denied or invalidated. No code
  acts on it toward merge or deploy — those remain human actions,
  permanently. See [Release-policy defaults](#release-policy-defaults).
- **SC-015 — harness and model are closed presets:** a request's harness
  resolves only to a compiled-in preset, and its model only to an
  operator-configured `models:` entry within its role's operator-configured
  `allowed` list; that model reaches an upstream only through an
  operator-configured route (`routes:`), chosen in the model's declared
  order by host-side pre-launch checks, never
  by the request. No request, model, alias, or role input can name an
  executable, interpreter, upstream URL, path prefix, route, or credential
  source, and every preset runs inside the same unconditional Docker sandbox.
- **SC-016 — skills are operator snapshots:** a worker sees only the skills
  its job's role names in `roles.<role>.skills`, resolved through the
  operator's `skill_dirs:` and then buildgate's built-in set (embedded in
  the binary and copied straight from it, never from a host folder),
  snapshotted and SHA-256'd host-side by the
  launch itself and mounted read-only at `/inputs/skills`; that digest is
  recorded on the attempt (or a drafting job's evidence). On the Temporal
  path the Activity mounts Workflow-input skills only when they equal its
  own Worker's resolution for that role, and computes the digest itself. No request input selects or names a
  skill, no host harness home (`~/.codex`, `~/.copilot`, `~/.claude`,
  `~/.pi`) is ever mounted, and a target-repo project skill sharing a
  configured skill's folder or front-matter name refuses the launch.
- **SC-017 — a lost Activity is never rerun on its own:** on the Temporal
  path every Activity of a new `RunWorkflow` execution runs at most once
  (`MaximumAttempts: 1`, `activity-retries` version 2). A step lost because
  its worker stopped (a heartbeat or start-to-close timeout) puts its
  request in `resume_review`, and nothing reruns until a human chooses
  `factoryd resume` (continue the lost build in its kept worktree from the
  last completed round, or rerun a lost drafting or planning step),
  `factoryd resume -from scratch` (rebuild the ticket) or `factoryd cancel`.
  A lost build keeps its worktree, never its harness session, and every
  gate reruns on the final commit. A resume runs only after that decision,
  only when the gateway, which must be reachable, reports no sandbox under
  the name the run recorded before launch, and no worker or supervisor
  container carrying that `openshell.ai/sandbox-name` is alive, and only when the worktree is on the run's own branch with HEAD equal to,
  or a descendant of, the HEAD its round state recorded; a decision answers
  one Generation of `resume_review`, and a decision for an earlier
  Generation is ignored. A workflow whose history recorded `activity-retries`
  version 1 still replays with its retry of the build, verify, full-suite,
  named-gate, review and post-oracle-verify Activities, at most once, only
  after an infrastructure failure or a Temporal timeout. Such an attempt
  after the first runs only after it takes the Activity execution's lease,
  its request's
  `-sandbox-docker` and `-data-dir` match the Worker's own, and every
  container labelled with the run and data dir is confirmed removed; the
  lease is re-checked before every sandbox launch, and a lower attempt's
  completed-checkpoint save is refused once a higher attempt holds it. Its
  meter ceilings are the configured ceilings minus the spend the run's
  meter ledger recorded since the execution's first attempt began
  (the whole ledger when that start is unknown; an unreadable ledger
  refuses). A retried build keeps the earlier attempt's worktree but never
  its harness session. Post-build and collect-evidence are never retried.

### Release-policy defaults

These rules set the starting values `internal/release.MergePolicyCheck`
receives. None of them changes what that check enforces.

- **Presence, not soundness.** `MergePolicyCheck` checks that the rollback
  plan and SHAs are *recorded*. It does not check that the plan is sound or
  that the commits were reviewed, which would need review-attribution
  evidence the factory doesn't collect yet.
- **Default limits.** An empty policy (no `release_max_files_changed`/
  `release_max_insertions`/`release_rollback_plan`) denies every PR by
  construction. To avoid that, `quickstart`, `init-config`, and
  `quickstart`'s reuse path default these to 25 files, 1000 insertions, and
  `git revert the merge commit on main`
  (`internal/sessionconfig.DefaultReleaseMaxFilesChanged` and siblings).
  - The reuse path only backfills a missing key, never overwrites one.
  - Operators may tighten, loosen, or clear any of them.
- **Policy snapshot per run.** The effective policy is persisted on the run
  at start (`run.Run.ReleasePolicy`), so later reconciliation evaluates
  against the policy the run started with. A legacy run without the field
  falls back to an always-deny `release.MergePolicy{}`, and the fallback is
  logged.
- **Flag precedence.** `factoryd serve`'s `-release-*` flags resolve
  explicit flag > session config, like every other Tier-1 flag.
- **Deny-all warning.** A policy that can never allow
  (`release.MergePolicy.CanNeverAllow`) is reported ahead of time, as a
  warning from `factoryd doctor` and as `release_policy_warning` in
  `GET /console-config.json`.
- **Full-suite fallback.** `RequiredGates` is `{canonical_verify,
  full_suite_verify}`. When no full-suite command resolves from any source,
  the run's canonical verify command is used as the full-suite command.
  - The gate still really runs that command in the sandbox and must pass.
  - The substitution is recorded (`FullSuiteSource = "verify_command"`) and
    shown in the PR evidence body.
  - The literal value `none` (`-full-suite-command none` / `.factory.yml
    full_suite_command: none`) opts out. The run is recorded as
    `FullSuiteSource = "none"`, is never re-substituted, and is denied
    release as before.

## State transitions

The following transitions are the complete normative state graph up to an
accepted, mergeable state. A transition is factory-owned and must be backed
by durable evidence; an agent report cannot create one. Merge and deploy
themselves are not part of this graph — they are human actions that happen
after it, informed by the recorded decision, never automated.

```text
  draft -> product_policy_check -> architecture_policy_check
        -> program_design_policy_check -> ready -> slice_running
        -> verifying -> slice_policy_check -> accepted
                                                  |
                              +-------------------+-------------------+
                              v                                       v
                         next_slice -> slice_running          merge_policy_check
                                                                       |
                                                                       v
                                                             decision_recorded (terminal)

  {slice_running, verifying, slice_policy_check}
    -- any execution --------------> halted
    -- failed/ambiguous check -----> quarantined
  halted -> quarantined
  quarantined -- policy-approved retry, or an
              authenticated human override --> resumed
```

The diagram is a reading aid. The edge lists below are normative and are
verified line-for-line by `internal/claims/safety_contract_test.go`.

### Normal progression

- `draft -> product_policy_check`
- `product_policy_check -> architecture_policy_check`
- `architecture_policy_check -> program_design_policy_check`
- `program_design_policy_check -> ready`
- `ready -> slice_running`
- `slice_running -> verifying`
- `verifying -> slice_policy_check`
- `slice_policy_check -> accepted`
- `accepted -> next_slice`
- `next_slice -> slice_running`

### Failure, recovery, and delivery

- `any execution -> halted`
- `halted -> quarantined`
- `any execution -> quarantined`
- `quarantined -> resumed`
- `accepted -> merge_policy_check`
- `merge_policy_check -> decision_recorded`

A failed policy check goes directly from execution to `quarantined`, not
via `halted`. This is deliberate (`cmd/factoryd/apply_run_result.go`): an
intermediate `halted` save would open a crash window that leaves the record
stuck at `halted`, where `factoryd override` cannot reach it, since override
accepts only a `quarantined` run. `halted -> quarantined` is the separate
path for a run an operator investigates and confirms should quarantine.

`resumed` is permitted only after a policy-approved retry or a durable,
authenticated human override; a quarantined run never silently resumes. A
failed or ambiguous policy check cannot skip `halted`/`quarantined` to reach
`decision_recorded`. `decision_recorded` is terminal: an allowed/denied
verdict, durable and evidence-backed, with no factory-owned edge onward to a
merged or deployed state.

### Request lifecycle: dismissing a dead end

Separate from the run-level graph above (`internal/run`, one slice at a
time): `internal/request`'s own request lifecycle runs `submitted ->
spec_drafting -> ... -> building -> pr_review -> done`, with `quarantined`
and `halted` reachable from any non-terminal state on a gate or
infrastructure failure (`internal/request.Quarantine`/`Halt`). A
quarantined or halted request could previously only be retried
(`internal/request.Retry`), never dismissed, so a request an operator has
no intention of retrying stayed red in `factoryd doctor`'s mount-visibility
check forever. `internal/request.Cancel` now also accepts these two states:

- `quarantined -> cancelled`
- `halted -> cancelled`

`cancelled` releases nothing by itself (`Cancel` never launches or resumes
anything); the request driver has already stopped driving a
quarantined/halted request before this transition runs. Its underlying run
may not have: a request halted on an unconfirmed run can still have a
Temporal execution in flight, and reclaim can still apply its result
(including opening a PR), the same gap `cancelRequest` in
`internal/api/server.go` documents for cancelling from `building`. Nothing
moves the request out of `cancelled` again. `done` is excluded from `Cancel`'s allowed
states: it is a genuine success outcome, not a dead end, and stays
immutable.

### Request lifecycle: a lost step waits for a human

A request step (a drafting or planning step, a ticket build, a PR-review
corrective round) whose worker stopped is never rerun on its own (SC-017).
`internal/request.EnterResumeReview` moves the request into `resume_review`,
a human wait with reminders, recording the lost state, the lost run and a
`Generation`:

- `spec_drafting -> resume_review`
- `oracle_drafting -> resume_review`
- `planning -> resume_review`
- `building -> resume_review`
- `pr_review -> resume_review`

`factoryd resume` / `POST /requests/{id}/resume` (`internal/request.ResumeRequest`)
records the decision and returns the request to the state of the lost step:

- `resume_review -> spec_drafting`
- `resume_review -> oracle_drafting`
- `resume_review -> planning`
- `resume_review -> building`
- `resume_review -> pr_review`
- `resume_review -> cancelled`

`cancelled` is `Cancel`, as above. A build resumed with `round` is checked
again before it launches; a failed check returns the request to
`resume_review` (`building -> resume_review`) with the reasons, and only
`-from scratch` or cancel are possible. `Quarantine` and `Halt` still reach
`quarantined` and `halted` from `resume_review`, a non-terminal state.
`retry` refuses a request in `resume_review`.

## Gate-retirement conditions

The pre-merge gates below are scaffolding and may be retired once the
following evidence exists for the target repository. **Human approval of
merge itself is not on this table.** By permanent operator decision, the
factory never retires it, whatever the evidence.

| Gate to retire | Required evidence before removal |
|---|---|
| Human approval of product/spec/architecture/design | Deterministic policy checks, durable inputs/outputs, fail-closed behavior, and an asynchronous notification path. |
| Human approval of a slice | Canonical oracle outside the worker, scope/protected-path checks, rollback pointer, and crash/retry recovery evidence. |
| Credential/network exception | Containment escape matrix and a policy and meter-ledger audit proving only explicitly allowed requests and no credential exposure. |

Until every applicable row is demonstrated, the factory must stop before the
corresponding unattended side effect. A passing unit test or reviewer-agent
verdict alone is not sufficient evidence for retiring a gate.

## Evidence and approval record

For each run, the durable record must identify the project, ticket, workspace,
base/result revisions, state transitions, attempt limits and outcomes, exact
oracle/verify commands and hashes, changed files, policy inputs/outputs,
notifications, overrides, and release/rollback data. Large logs and artifacts
may live on disk, but their path, hash, ownership, and timestamps belong in
the durable evidence store. The target single-machine store is SQLite in WAL
mode. The event log already uses it (`internal/run/events.go`), but run
records are still per-run JSON files (`internal/run/run.go`), so this
remains `partial`.

The machine-checkable test verifies that these sections, invariant IDs, and
state edges cannot be silently removed while the implementation claims to
satisfy this contract.
