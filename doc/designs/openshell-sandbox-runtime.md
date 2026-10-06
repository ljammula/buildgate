# The sandbox runtime: OpenShell gateway, supervisor and meter

Every worker container is created by the NVIDIA OpenShell gateway (0.1.2).
A worker has no network interface. Its model calls leave through the
supervisor OpenShell starts beside it, under a policy buildgate writes per
launch, and are counted by buildgate's own meter. Normative statements are
in [`safety-contract.md`](../../safety-contract.md) and
[`containment-matrix.md`](../../containment-matrix.md); operator steps are
in [`USAGE.md`](../../USAGE.md#the-sandbox-runtime-openshell-gateway-and-meter).

## Parts

```text
  Mac (host)                         Docker VM (colima)
  +-------------------+   mTLS    +-------------------------------+
  | factoryd          |---------->| gateway   127.0.0.1:8080      |
  |  internal/sandbox |           |  Docker socket, credential    |
  |  internal/openshell           |  store, /var/lib/openshell    |
  +---------+---------+           +---------------+---------------+
            |                                     | creates
            | reads                               v
            | ~/buildgate/meter-ledgers   +---------------+     +--------------+
            | <run dir>/launch/.../output | worker        |---->| supervisor   |
            v                             | NetworkMode   |     | policy,      |
  +-------------------+                   | none          |     | credential   |
  | meter ledgers,    |<------------------+---------------+     +------+-------+
  | worker output     |    host binds                                  |
  +-------------------+                               each model call  v
                                   +---------------------+      +-----------+
                                   | meter 127.0.0.1:50051|<-----| upstream  |
                                   | ceilings, ledger     |      | model API |
                                   +---------------------+      +-----------+
```

| Part | Code | Runs as |
|---|---|---|
| Stack start, stop, certificates | `internal/hostcontrol/openshell*.go`, `docker-compose.openshell.yml`, `openshell-gateway.toml.tmpl` | Compose project `buildgate-openshell`: `gateway` (host-networked, UID 0) and `meter` (UID 65532, read-only root) |
| Launch sequence, guard, route and meter policy | `internal/sandbox/runtime*.go`, `sandbox_request.go`, `route_provider.go`, `meter_usage.go` | In `factoryd` |
| Gateway client | `internal/openshell` (the only importer of the OpenShell Go SDK) | In `factoryd` |
| Meter | `internal/meter`, `cmd/factoryd-meter` | Its own container; a supervisor middleware (gRPC) |
| Test stand-in | `internal/sandbox/sandboxtest`; `sandbox.Run` under `-tags factorydtest` | Tests only |

## One launch

`sandbox.RunWorkerThroughRuntime` then `sandbox.RunThroughRuntime`:

| Step | What happens | Why |
|---|---|---|
| 1 | Resolve the route: provider name, endpoint, admitted path, worker environment with placeholders (`RoutePolicy.RouteAccess`) | A route the runtime cannot serve is refused before anything starts |
| 2 | Build the meter policy from the checked route policy, with the ceilings the run has left (`RoutePolicy.MeterConfig`) | Each sandbox's meter counts from zero |
| 3 | Append the sandbox name to `<run dir>/sandboxes.jsonl` | A crash after this point leaves a name to reconcile |
| 4 | Push the credential to the gateway (`Runtime.PushCredential`); refuse one that expires within the step's budget | A pushed credential reaches only sandboxes created after it |
| 5 | Create a per-launch workload template (image, environment, mounts, CPU, memory) and the sandbox from it; delete the template. For a route with a credential, wait until the gateway reports the provider `READY` in the sandbox (`Runtime.waitRouteReady`, at most a minute) | The Docker driver takes resource limits only from a template. The supervisor rebuilds its proxy once, at its first settings poll; a worker released before that loses the model request it has in flight |
| 6 | Record the sandbox id, the worker container's Docker `StartedAt` and the ledger file | The restart guard compares against this start time |
| 7 | Write the guard's `go` file; the worker's wrapper starts the command | Nothing runs before factoryd has recorded the launch |
| 8 | When the command's output file appears, write the guard's `started` file; relay the output to the step's log | A second start of the command exits at `started` |
| 9 | Wait for the exit; delete the sandbox on every path | A failed delete is reported and reconciled by label |
| 10 | Read the sandbox's ledger; a refusal at a ceiling or an unreadable ledger ends the step with the ceiling error | Spend comes from the meter's file, never from the worker |

Sandbox names are `bg-` plus 16 hex characters (the gateway allows 19
characters); `sandboxes.jsonl` maps a name back to its run.

## The restart guard

A gateway restart starts every sandbox's command again. The worker's
command is wrapped (`workerWrapperScript`):

| Wrapper exit | Meaning |
|---|---|
| 96 | The `go` file never appeared (factoryd did not release the launch) |
| 97 | `started` exists: this is a second start; the worktree is not touched |
| 98 | `/workspace` is missing |

factoryd treats exit 97, or a Docker `StartedAt` that moved, as
`ErrSandboxRerun`: the step is recorded lost and a build keeps its worktree
(SC-017). `hostcontrol.StopOpenShell` refuses to stop the gateway while a
request or sandbox is active.

## Model route and credential

| Route | Under the runtime |
|---|---|
| `static` with a key | Provider holds the key; the worker's key variable is a placeholder; path admitted by prefix |
| `static`, `allow_no_credential` | No provider; the endpoint is admitted with no credential |
| `chatgpt-codex` | Provider holds the access token and account id read fresh from `~/.codex/auth.json` (never refreshed or written); upstream pinned to `https://chatgpt.com/backend-api/codex`; only the Responses path is admitted |
| `github-copilot` | Provider holds the operator's GitHub login token, which the Copilot API accepts as a bearer token (checked live 2026-10-04) and which does not expire during a step; only the exact API path is admitted. The short-lived exchanged token is used only by the host-side model listing: a sandbox cannot be given a refreshed one. The meter overwrites the client headers the API requires and applies the Copilot body rewrite |
| A credential to a plaintext upstream; a route with no worker model id | Refused at launch |

The policy admits the union of every harness's model executables
(`harness.ModelBinaries`). The supervisor substitutes a placeholder only on
a request to the endpoint the route's profile binds. It does not remove a
credential header the worker writes itself.

## The meter

| Property | How |
|---|---|
| Ceilings | Token and cost ceilings, sliding-window budgets, requests per minute and maximum request size, from the session config's `meter_*` keys |
| Usage | Parsed from the response (OpenAI completions, Responses, Anthropic); a cached input token counts at 10% toward token limits |
| Ledger | One fsync'd JSON line per event in `~/buildgate/meter-ledgers/<run name>/<sandbox id>.jsonl`; reloaded on a sandbox's first request |
| Failure | `on_error: fail_closed` in the policy; a ledger that cannot be opened denies the request; WebSocket upgrades are denied |
| A dropped request | An admitted request with no completion record is charged its estimate; the step's spend is marked partial |
| Transport | Plaintext gRPC on the VM's loopback; the gateway registers it with `allow_insecure_transport` |

## Sidecars

The registry proxy and compose services stay on factory-owned `--internal`
Docker networks. The worker joins none: each sidecar is one `allowed_ips`
endpoint in the sandbox's policy, and the worker is given addresses
(`BG_SERVICE_<NAME>`, the package managers' proxy URLs), read after each
attempt's start.

## Limits of OpenShell 0.1.2 that shaped this

| Limit | Consequence |
|---|---|
| The sandbox's seccomp filter denies every set-id system call | The worker image carries GNU make built without `posix_spawn` |
| No Docker read-only root or swap setting through the Docker driver; a process limit only for every sandbox at once | Recorded as residuals in the containment matrix; the gateway configuration sets one process limit (`sandbox_pids_limit`, 1024) and there is no per-run setting |
| Bind mounts need the gateway's resource admission off | `LaunchSpec.Validate` is the only check on a launch's mounts |
| The Docker driver refuses a bind whose source the gateway cannot see | The gateway container mounts the operator's home directory read-only: a worktree binds its repository's `.git`, wherever the repository is |
| The filesystem policy denies every unnamed path | `/usr`, `/etc`, `/opt`, `/proc`, `/dev` are listed read-only |
| The runtime sets `HOME` to the image's `WORKDIR` | The wrapper exports `HOME=/home/worker` |
| A middleware must be reachable from the host-networked gateway | The gateway is host-networked and bound to loopback |
| The supervisor installs a provider's credential a second time at its first settings poll, 10 s after it starts, and rebuilds its proxy to do it | A step on a credentialed route starts its command about 10 s after its sandbox. A route with no credential has no provider to ask about: a request in flight at that moment is cut, retried by the harness and charged its estimate |

## Verifying a change here

```sh
go test ./internal/sandbox ./internal/openshell ./internal/meter
OPENSHELL_LIVE=1 OPENSHELL_LIVE_IMAGE=<worker ref> go test ./internal/openshell -run Live -v
make live-smoke
make live-compose      # when compose services or the registry proxy change
```

An OpenShell upgrade repeats the live tests with `OPENSHELL_LIVE_RESTART=1`
and `OPENSHELL_LIVE_CHATGPT_MODEL=<model>`.
