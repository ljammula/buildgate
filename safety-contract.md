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
| Supervisor | OpenShell's pinned supervisor image; on a network that re-signs TLS, a local image built from it whose one change is its trust file | Worker process | One per sandbox, host-networked, uid 65534, read-only root. It terminates the worker's TLS, holds resolved credentials in memory, and dials upstreams from the VM's network namespace. It opens no TCP listener on the VM: only Unix sockets under `/run/openshell`. It trusts the public roots built into it and its image's `/etc/ssl/certs/ca-certificates.crt`. On a network that re-signs TLS (a corporate proxy), where `make install` recorded the proxy's CA with the system's roots in `~/.config/factoryd/build-ca.pem`, `factoryd` builds a local image from the pinned one with that bundle as the trust file (`hostcontrol.EnsureSupervisorImage`, named after the pin and the bundle) and the gateway starts supervisors from it: the supervisor then accepts the proxy's certificate for the route's one admitted upstream, so the model credential crosses that proxy, as every other tool's on that network does. The policy's host, path and executables are unchanged, and a network that does not re-sign TLS runs the pinned image. |
| Worker image | The operator's `sandbox_image` (for an API-started run, the daemon's image allow-list) and factoryd's one derivation from it | The target repository's toolchain declarations (`go.mod`, `.python-version`, `pyproject.toml`) | A repository cannot name an image. Its declarations are read as version numbers only (`internal/toolchain`). When the configured image's Go or Python does not satisfy one, the run derives an image from the configured one by a fixed rule (`toolchainImageFor`; `internal/sandbox/Dockerfile.project`, stage `toolchains`): the official `golang` or `python` image of that version, copied over the configured image's toolchain. No file of the repository enters that build and none of its code runs in it; when Python is replaced the build runs buildgate's own script tests on the new interpreter and fails unless they pass. The derived image is pushed to the local registry and launched by digest, the digest the run records. The derivation runs inside the run, after an API caller's `sandbox_image` passed the allow-list, so an allowed image is the only base of one. Node is never replaced: the coding agents run on it and it is the executable the network policy admits. `FACTORYD_AUTOSTART=0` turns the derivation off. Residual: the toolchain is an official image named by version tag, resolved when the image is built, not a digest buildgate pins. |
| Go modules | The operator's own `go` command and Go settings on the host (`GOPROXY`, `GOPRIVATE`, git's credential helper), and the registry proxy | The module paths, versions and hashes the target repository's `go.sum` files list | A build reaches a module only the operator can fetch without any credential entering a container. As a run with a registry proxy is dispatched, `factoryd` reads the `go.sum` files of the commit the build starts from (never the checkout's working tree, and not those under `vendor/` or `testdata/`) as module paths, exact versions and `h1:` hashes only (`parseGoSums`: anything else is dropped). Of those it keeps the ones the operator's Go settings mark as this machine's to fetch (`GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`; every listed module when `GOPROXY` is the operator's own), and the operator's `go` fetches exactly those on the host, in an empty directory, so it reads no file of the repository (`repositoryGoModuleDir`). A fetched module is served only when it has the hash `go.sum` gives it. The files are kept in one cache under `~/buildgate/gomodules`; a run is given a view holding its own list and no other repository's, never changed once a run has it, mounted read-only into its registry-proxy container. The proxy answers the Go route from the view first, opened as an `os.Root`, and answers 404 itself for anything else about a module the view holds, so a private module's name is not sent to the public proxy (`registryproxy.Route.LocalDir`). The worker's `go` is made to use the proxy for every module (`GONOPROXY=none`) and verifies what it is served against `go.sum` as always. The view's path is in the Workflow input, so a Worker that takes over the run serves the same modules. Residuals: a `go.sum` on the build's base commit that names, with its real hash, a private module of another repository the operator can read gets that module served to the build (the list is the committed file's, never the agent's edit of it during a build); the host's `go` contacts whatever host a listed private module path resolves to, as the operator's own `go mod download` would. `FACTORYD_AUTOSTART=0` turns the fetch off. |
| Spend meter | `factoryd-meter` (`internal/meter`), called by the supervisor for every model request | Request and response bodies of the worker's model calls | Counts each run's tokens and cost and refuses requests past the ceiling. It parses untrusted bodies, holds no credential, writes each sandbox's ledger under `~/buildgate/meter-ledgers/` where no worker and no other run's sandbox writes, fails closed (`on_error: fail_closed`), denies WebSocket upgrades, and refuses a response it cannot read as a stream. Exposure as it is: a plaintext gRPC listener published on VM loopback only (`127.0.0.1:17672`), with no TLS and no caller check; no worker or sidecar can reach it, and any process on the VM or the Mac's loopback can. |
| Compose sidecars | The operator's `compose_services_*` settings and `internal/composeservices`' field allow-list | The target repo's compose file and `.env` at the run's base commit | Repo content can only select allow-listed images, Linux platforms (`linux/amd64` or `linux/arm64`), and fields; fixed `container_name` values are inert and never emitted; relative bind inputs are read from the immutable base commit, reject links/special files/path escapes, and are mounted read-only (a source the base commit lacks becomes an empty per-run named volume); it never widens a sidecar's containment (a service's own `mem_limit` is accepted only at or below the operator's `compose_services_memory`). Operator-supplied worker endpoint variables are host/session configuration, sorted and collision-checked, cannot override factory/sandbox/credential/cache/network variables, and never enter Temporal history; a value that names a service as a host is refused, since the worker resolves no service name. They are stored in the gateway's sandbox record and readable through the gateway API by any holder of the factory certificate. Sidecars stay on the factory-owned internal network; the worker does not join it and reaches each sidecar by address and port through the supervisor. A compose file the allow-list rejects halts the whole run before its build (`compose_services_rejected`, naming each service and the fix), never a partial or silent launch. Published `ports:` are never published on the host: each fixed TCP mapping becomes a forward on the worker's own loopback to that service's address on the run's internal Compose network, and two services publishing one port reject the file. A compose edit made by the worker is never launched in the same run. |
| Repository commands | The committed `.factory.yml` as read before the workflow starts | The worktree copy; the build | `verify_command`, `gates:`, `setup:` and `autofix:` run only in the sandbox (`autofix:` only in the build sandbox, inside a round, with its changes confined to files the ticket already changed), from values resolved before the workflow starts, with whatever network that step already has (none for verify and gates beyond the registry proxy and Compose sidecars; the model route in the build sandbox); `setup:` and `gates:` are given to no planner or reviewer (a setup command that fails inside a build round is named, with its output, to that build's own agent), and nothing from the file runs in a review sandbox; `.factory/` is mounted read-only into every sandbox that runs a repository command, as the commit `.factory.yml` was read from holds it, and a run that changes it is refused at release |
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
| Skills | The operator's `skill_dirs:` folders, buildgate's built-in skills, and `roles.<role>.skills` names | Request input; the target repo's own project skills; the worker between rounds | Only a host-side, hashed, read-only snapshot of the role's named skills reaches the worker (SC-016); a same-name repo skill refuses the launch; harness homes stay per-session and get fresh copies each round. Skill content is operator-trusted instruction text, vetted like a dependency. A review launch sees the repository's instruction files as the base commit holds them, read-only, and what the build changed in them only as quoted data (SC-019). |
| Repository memory | The operator's `memory.repositories` switch (off for every repository not listed) and the project's kill switch; the operator's approval at spec review; the section text the host renders | The build agent's notes; the memory ticket's build; a reviewer's comment on its pull request | A note becomes a candidate line only if it passes a fixed text rule (`memory.ValidateReason`: one line, 120 characters, a fixed character set with no Markdown emphasis, link or list syntax, commands only inside backticks under the command rule, no URL in any form, address, absolute path inside or outside a command, home path, unbroken token of 20 or more characters of `A-Za-z0-9+/_-` that holds both a letter and a digit, or anything redaction would change); a refused note is never repaired or stored. A candidate is collected only from a run recorded against this repository's root. A section line holding a control or format character is not read, and every line is printed with escapes removed. Candidates are shown to the operator only (`factoryd memory list`/`show`, `GET /projects/{project}/memory`). The section reaches the repository only as a request the operator approves and merges (SC-020). No command a note names is ever run by the factory, and nothing is collected, proposed or dropped without a person running the command. A memory request already in flight is stopped at release by the kill switch and by `factoryd memory off`; removing the repository from `memory.repositories` stops new proposals only. |

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
    link without the token. So does a loopback address whose listener
    `quickstartServeVerifiedOurs` cannot attribute to this data dir
    (`TestQuickstartPrintAndOpenConsoleLinkOmitsTokenForAnUnverifiedListener`).
  - `quickstart` reuses an already-listening `serve` only after
    `quickstartServeVerifiedOurs` attributes the listener to this session
    (`lsof` plus a same-uid pid check). A bare `/healthz` 200 is not enough.
    The pids it checks are this data dir's own: its `quickstart-serve.pid`,
    the pid in the `console-address` record its `serve` wrote (for the
    address that record names), and the `serve` LaunchAgent's when the
    service is installed for this data dir
    (`TestServeVerifiedOursIgnoresAnotherDataDirsLaunchAgent`).
  - A listener it cannot attribute (another data dir's `serve` on the
    default port) never gets the token: this data dir's `serve` is started
    on a free loopback port, and the token is returned only once that
    `serve` has recorded itself
    (`TestQuickstartEnsureServeTakesAFreePortWhenTheDefaultIsHeld`,
    `TestQuickstartEnsureServeGivesNoTokenUntilItsOwnServeIsRecorded`).
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
| T-16 | A build, or the repository it builds, plants text in failing output, a file name or a reviewer's finding, or its own notes, to steer the next attempt or its reviewer | The record of an earlier attempt is built by the factory from the run record; every value from a build or a reviewer is one line, length-capped, cleaned and quoted as data; it goes only to a build's first prompt, never into a spec or a review (SC-018); the hash and state are checked before use | The corrective build is a new run whose result every gate and an independent review judge against the unchanged spec; its record, the handoff it was given and the prompts its launches saved are kept in the run and request directories (a saved prompt is what the session folder held when the host copied it: a build can alter its own, and the host empties the folder before every launch so an earlier step's file is never kept as a later launch's) | partial |
| T-17 | A build edits, adds or links the repository's instruction files (AGENTS.md-style files, harness folders, project skills, MCP or hook configuration) so the reviewer that reads its worktree is told to pass it | Every model review launches with the base commit's instruction files mounted read-only over the worktree; what the build changed in them reaches the reviewer only as fenced data; instruction-named paths the worktree holds beyond the result commit are removed first; a shape the mask cannot carry halts the run before the review launches (SC-019) | The run halts with `review_instructions_failed`; the cause is in the review attempt's record for the operator and in no handoff, spec or later build | partial; the table of instruction paths is a list, and a harness may load a path that is not on it; the table was measured against the pinned pi, codex and copilot harnesses, and against pifork with the pinned pi behind its launcher (what an operator's own fork adds is not measured), by a recorded probe (`make probe-instruction-paths`, result in `agent/pi/tests/fixtures/instruction_paths.json`), which found copilot loading `.github/copilot-instructions.md` and `.github/instructions` from the directory of a file it read, so every table entry matches at any depth; `.github/mcp.json`, which copilot runs only in a trusted project, is on the table although a worker turn never grants that trust |
| T-18 | A build edits or replaces a script under `.factory/` that a repository command runs, so a gate judges the build with the build's own script | Every build, verify and gate sandbox mounts `.factory/` read-only from the commit `.factory.yml` was read from; a worktree shape the mount cannot carry halts the run (`factory_dir_failed`) before the sandbox starts | The snapshot's SHA-256 is on each attempt; `.factory/` is a protected path, so a result commit that changes it is refused at release | partial; a script that calls files outside `.factory/` runs the build's copy of them, as `verify_command: make test` does |
| T-19 | A build plants an instruction in its notes so that it becomes a line of `AGENTS.md` every later agent obeys | The text rule refuses markup, quotes of shell syntax, URLs and anything over one short line; the host renders the line and the whole file; one request changes at most five lines; the operator reads the exact section in the spec at spec review; the release check passes only a byte-equal `AGENTS.md` of mode 100644 under that one name and no other changed file, only while the base still holds the `AGENTS.md` the text was rendered from, and refuses any other run that changes the file of a repository with a memory section or adds the marker text to one without; the memory request's own reviews read the base commit's instructions (SC-019); a reviewer's comment starts no build that rewords a line (SC-020) | A line in force is removed by a memory request (`factoryd memory propose -remove`), or by an ordinary human commit; `factoryd memory off` and the kill switch stop further proposals and the release of a memory request already in flight; the `memory.repositories` switch stops further proposals only | partial; a plausible but wrong line can be approved by a person, and no command a line names is checked |
| T-20 | A secret, a hostname or a personal path reaches a public repository's `AGENTS.md` through a memory line | The text rule refuses URLs in any form (`//`, `www.`), address-shaped tokens, home paths, absolute paths (outside a quoted command a `/` followed by a letter or `.` unless a letter, digit, `.`, `_` or `/` precedes it; inside one a `/` followed by a letter, `.` or `/` at its start or after a space or `=`), e-mail addresses, any unbroken run of 20 or more characters of `A-Za-z0-9+/_-` that holds at least one letter and at least one digit (key ids, hashes, encoded secrets; a long word, relative path or variable name with no digit passes) and any text secret redaction would change; a repository's candidates, stop marker and proposals are kept under a key of its name and root path, never shared with a repository of the same name, and candidates are collected only from runs recorded against that root; memory is off for a repository until the operator lists it; the operator reads every line at spec review and again on the pull request | The line is removed by a memory request or a human commit; history rewriting is the operator's own action | partial; the rule is pattern-based: a bare internal name, a secret of letters only or digits only, and a path after a character other than a space or `=` inside a command pass it |

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
  attempt; the verify surface includes the committed `.factory.yml` the run
  read (its SHA-256 on the run record), its `setup:` commands, which run
  before the command of every build, verify and gate sandbox and never in a
  review sandbox, and the `.factory/` directory of the commit that file was
  read from, mounted read-only in each of those sandboxes (its snapshot
  SHA-256 on each attempt).
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
- **SC-018 — only a build is told about an earlier attempt, and only from
  the factory's record:** a ticket run saved quarantined or halted leaves a
  handoff (`handoff.json` in its run directory, its SHA-256 on the run
  record): its rounds, each failed check with the factory's own sentence
  about it, the failing lines of a command gate's output, and the bin of
  each check. Every value in it that a build, a repository command or a
  review model wrote is one line, cut to a fixed length and stripped of
  escapes, backticks and recognisable secrets when it is built, and it
  carries nothing a reference oracle asserted: that gate gets no finding,
  no other finding names an oracle, and the operator's triage sentence is
  not copied in. It is used only when the file's hash is the one the run
  recorded and it describes the state the run is in now. It, and the
  notes in it, reach a model in one way: rendered as text and staged read-only for a later
  build attempt of the same ticket (`-earlier-attempt`, the build script's
  `--earlier-attempt`), which puts it in that build's first prompt. It is
  never part of a spec, never an input of a review, verify or gate step,
  and never given to a planner, another ticket or a pull request. It is
  not left where a later step of the same run works: the build script
  never writes it into its round-state file, nor the notes into its
  round-state, evidence or report files, its output or the progress feed,
  and the build's harness
  session, which holds its prompts, is removed from the worktree when the
  build step returns (its rounds' saved output and the prompts it saved are
  copied to the run directory first, where only the operator's prompts route
  and `factoryd logs -prompt` read them); a session that cannot be removed, or is not a plain
  directory, fails the build step, so no later step runs beside it.
  A saved prompt is what the build's session folder held when the host
  copied it; a build can alter its own before that, because the build script
  and the coding agent run as one user in one sandbox and the script has no
  channel to the host that the agent cannot also write (its output file sits
  in a directory the sandbox writes). No second user can be given to the
  agent (the sandbox's seccomp filter denies every set-id system call), a
  child of the same user can open its parent's descriptors through `/proc`,
  and every directory the host shares with the sandbox is shared with all of
  its processes. So the host verifies no saved prompt
  and attributes no line of the output to the script: a `FACTORY_PROGRESS`
  line is recorded as the worker's (`source: worker`), with the host's time,
  in the `round` and `agent` stages only, is shown and never read by a gate,
  a policy or a state transition, and cannot be recorded as one of the
  factory's own lines. The operator surfaces label a saved prompt "as saved
  by the build", never "sent". Before a build or a review is launched the host
  removes whatever its session's prompts folder holds, making a folder left
  read-only removable first; a folder that cannot be shown gone fails the step
  as an infrastructure error, so no launch runs beside a prompt it did not
  save and a file an earlier step planted is never kept as a later launch's
  prompt. What the build agent itself chooses to write into the
  workspace is an ordinary part of its diff, judged like the rest.
  A build that ends without passing is asked once, in its own session, for
  notes under five fixed headings. The factory copies the reply out of the
  session folder before removing it, splits it into one-line items cleaned and
  capped like every other value, and stores them in the handoff apart from its
  own facts and after them. They are labelled the agent's view, never decide a
  bin, are absent from the run record and every MCP tool, and are otherwise
  readable only through the operator's own handoff route, or, inside a saved
  build prompt that carries them, the operator's prompts route. The items
  under the fifth heading (things worth knowing about the repository) may
  also be listed to the operator as memory candidates, each only if it
  passes the memory text rule; they still reach no review, no planner and no
  other ticket's build except under SC-020.
  A ticket rebuilt after `factoryd retry` is given the record of its own
  quarantined run on the same terms, and only while the ticket's spec is the
  one that run was built from.
  That rebuild continues on the quarantined run's branch, from its commit,
  only when it is given the record, and its gates, reviews and release
  decision then judge the ticket's whole change from that run's diff base;
  a run it is given no record of is never built on, and `retry -from
  scratch` and `resume -from scratch` rebuild from the base in every case.
  A build that follows one which was given the record and never finished
  (lost and resumed in its kept worktree, or halted and built again) is
  given the record of that same quarantined run: the unfinished run names
  it (`earlier_attempt_of` in its run record), and the record is rendered
  again from that run's handoff under the same hash, state, request and
  spec checks, never copied from what the unfinished run was handed.
  A corrective build follows a quarantined ticket run only when the handoff
  sorts every judged failed check as one a build can fix when told about it:
  `canonical_verify`, `full_suite_verify`, the named command gates, a
  repository's own gates, `diff_scope`, `required_files_changed`,
  `required_content_present`, and the two reviews when they gave a verdict.
  `reference_oracle` counts only where the build is already shown the oracle
  (a request's ticket). `tests_added`, a review that gave no verdict, a
  repository gate that never ran, a check whose step a repository setup
  command stopped and any check with no bin never do, and a
  halt never does. A check on the diff of an attempt that committed nothing
  (canonical verification never passed) is not judged either way. Such a
  build is a new run on the quarantined run's branch, started after the
  quarantine was recorded and notified (SC-013): every gate runs again on
  its result, it counts against `review_corrective_rounds` (one budget per
  ticket build, shared with the review round) and the launch budget, and
  it changes nothing about who merges (SC-001).
- **SC-019 — a review reads the base commit's instructions:** every model review of a run launches with the repository's instruction files (AGENTS.md-style files, harness folders, project skills, MCP and hook configuration, at any depth of the repository) as the commit the request started from holds them (for a single-ticket run, the run's own base), mounted read-only over the worktree; what the build changed in them reaches the reviewer only as quoted data; an instruction-named path the worktree holds beyond the result commit is removed before the review; a tracked instruction file that no longer matches the result commit, a link from an instruction path to a directory that is not one, a checkout-converted instruction file, or any other shape the mask cannot carry halts the run before the review launches, and a build is never told why. Each review attempt records the snapshot's SHA-256, the masked paths and the removed paths.
- **SC-020 — memory reaches another ticket only approved or merged, and only inside the fence:** a line derived from one ticket's runs (a build agent's note that passed the fixed text rule, rendered by the host as one list line) reaches a model working on another ticket in two ways only: inside the spec and ticket of a memory request, which stops at spec review until the operator approves it, or as the fenced section of root `AGENTS.md` at the commit that work starts from. A run of a memory request is released only when its result commit holds exactly one root path whose name folds to `AGENTS.md`, spelled `AGENTS.md`, a regular file of mode 100644 that is byte-for-byte the text the factory rendered when the change was proposed, and the run changed no other file; and only when root `AGENTS.md` at the run's diff base is the file the proposal was rendered from (its SHA-256 is the proposal's `base_blob_sha256`, or neither has a file), so a change a person made to the file after the proposal is never overwritten: the release is refused with `AGENTS.md changed after the memory change was proposed; propose it again`, and the worker halts such a request with the same sentence before its build when the checkout's HEAD already differs. Once a repository's root `AGENTS.md` holds a memory section (at the run's diff base, a root file of that name in any letter case holds the text `buildgate:memory`), a run that is not a memory request's may not change that file in any way (its text outside the markers, its mode, its type, a move or a delete) nor add a case variant of its name; before a section exists, such a run may change or create the file but may not leave the marker text in it (written plainly or as HTML character references), a second spelling of its name, or a symlink, submodule or directory in its place. The host reads this from git objects, never the worktree, records it on the run, and refuses the release on any read failure; the operator's override computes it on the host before its decision, and the HTTP override refuses a run that lacks it. With pull requests on, a ticket whose release decision is not allowed halts its request before a later ticket is built on its commit; with pull requests off nothing is pushed or opened, the tickets are built in order and each run's decision is on its record. A memory request changes at most five lines, and when the section is full the proposal is refused: the factory never chooses a line to drop. Candidates are shown only to the operator (`factoryd memory list` and `show`, the project's memory route); nothing collects, proposes or drops one without a person running the command, the factory never runs a command a line names, and a reviewer's comment on a memory request's pull request is answered and starts no build. The section is read only when every line of it is free of control and format characters, and a file with no section is given one only when it does not end inside a code fence: whatever the factory accepts, the file it renders reads back. Memory is off for a repository until the operator lists it under `memory.repositories`, and the project's kill switch or `factoryd memory off` stops every memory action that changes anything. Of the three switches, two stop a memory request already in flight: its run is not released while the kill switch is engaged (as for any run) or while the store holds the off marker `factoryd memory off` wrote (`repository memory is switched off (factoryd memory off)`). Removing a repository from `memory.repositories` stops new proposals and every other changing subcommand, and does not stop the release of a request already in flight.

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
