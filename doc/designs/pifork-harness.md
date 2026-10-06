# pifork harness: running a fork of Pi

**Status:** opt-in harness. The operator builds the worker image from their own
Dockerfile; nothing about a particular fork lives in this repository. Runs on
any route the sandbox runtime serves
([`openshell-sandbox-runtime.md`](openshell-sandbox-runtime.md)).

## Scope and resolution

The harness is chosen per role (`roles.<role>.harness`), from a closed,
compiled-in registry (`internal/harness`), not a plugin mechanism. A role
with no `harness` keeps the existing embedded Pi adapter, image, and route
defaults unchanged. `harness: pifork` on a role runs that role's jobs through
the shared `agent/pi/scripts` with `--harness pifork` (`PiforkAdapter` in
`harness_adapters.py`) and the operator-built worker image.

Resolution is shared by single-ticket runs, `worker`, API-started runs, daemon
fallback configuration, and Temporal submission. Explicit command-line or
request values win over the registry defaults. In particular, an explicit
worker image, route upstream/path, credential source, or model id is never
silently replaced. A `models:` entry's `id` remains required
and is never inferred.

The effective order is:

1. hard defaults (existing Pi behavior);
2. session configuration;
3. the role's harness registry entry for fields the operator did not set;
4. project configuration where the existing run path already supports it;
5. explicit command-line/API values.

The resolver validates the complete result before creating a durable run
record. The rollback is to drop `harness: pifork` from the roles; that selects the
existing Pi path without changing its defaults.

## Worker API selection

The harness defaults to Pi's `openai-completions` client. A model served only on
the Responses API (a `chatgpt-codex` route, or `api: openai-responses` on a
`models:` entry) makes the resolver generate `api: openai-responses` and
meter `usage.input_tokens`/`usage.output_tokens` from the Responses
response. The worker base path remains independently configurable; an
explicit path wins, and a Responses setting without a worker model id is
rejected.

```yaml
models:
  luna:
    id: gpt-5.6-luna
    routes: [chatgpt]
roles:
  execution: { model: luna, harness: pifork }
```

## Processes and credentials

The host `factoryd` process is the only process that reads credential
material:

```text
host:    the route's credential (a static key, ~/.codex/auth.json, or
         the GitHub login token of a github-copilot route)
           |  resolveRouteCredentials (host only, fresh per launch)
           v
gateway: the credential is pushed to the OpenShell gateway's store
           v
worker:  FACTORY_MODEL_* env rendered into a models.json whose model entry
         points at the route's upstream; the key is a placeholder the
         sandbox's supervisor substitutes -- no real credential, ever
```

For every run the credential-free route policy travels in
`RunWorkflowInput`; the executing Worker resolves the route's credential
itself, so Temporal input/history never contain one. factoryd passes the
route as `FACTORY_MODEL_BASE_URL`/`_ID`/`_API` (and optional `_EXTRA_JSON`,
`FACTORY_MODEL_KEY_ENV`, `FACTORY_MODEL_HEADERS_JSON`) environment
variables; `PiforkAdapter.prepare` renders `models.json` from them into the
worker's isolated `PI_CODING_AGENT_DIR`. The adapter lets `id`/`api`/
`baseUrl` win over the extra JSON, and the sandbox's policy admits only the
route's upstream, so the extra JSON cannot redirect the worker.

## Worker containment

The worker runs as the existing dedicated non-root sandbox user, under the
same filesystem policy as every worker, with its workspace as the only
writable project mount; runtime state (including the generated
`models.json`) is on an isolated tmpfs. No network interface — the
sandbox's supervisor admits only the route's upstream and, when configured,
the package-registry proxy; verification/full-suite processes get no model
route. No
`auth.json`, developer home directory, OAuth value, Copilot access token,
session database, or host package cache is mounted in. What the worker image
contains beyond the base worker image is the operator's own Dockerfile.

## Image contract

buildgate knows three things about the image, and nothing about the fork's
name, layout or configuration:

| The image provides | Why |
|---|---|
| An executable `pifork` on `PATH` that takes Pi's flags (`--print --mode json --session-dir --provider --model --thinking --continue --skill`) and emits Pi's JSON event stream | `PiforkAdapter` reuses `PiAdapter`'s invocation and parser unchanged |
| That executable reads its agent directory from `PI_CODING_AGENT_DIR` (the worker sets it to `/home/worker/.pi/agent`) | `models.json`, the route to the model, is written there before every job |
| The model is called by `/usr/local/bin/node` | The sandbox's network policy admits the route only from the harness registry's `ModelBinaries` |

Optional: a directory `/opt/pifork-agent-seed`. Before a job the adapter copies
its contents into the agent directory (an `npm` folder is symlinked, existing
files are kept), then writes an empty `auth.json` if there is none. Put there
whatever the fork must find on first start: settings, extensions, a
non-interactive permission config.

A fork that reads another variable or needs its own environment gets a small
launcher as `/usr/local/bin/pifork`:

```sh
#!/bin/sh
# This fork is rebranded "tau" and reads TAU_CODING_AGENT_DIR.
export TAU_CODING_AGENT_DIR="${PI_CODING_AGENT_DIR:-$HOME/.tau/agent}"
exec /usr/local/bin/node /opt/pifork/packages/coding-agent/dist/cli.js "$@"
```

The launcher assigns the fork's own variables, never defaults them
(`${VAR:-...}` on the fork's variable would keep an inherited value): a target
project's compose configuration may set worker environment variables outside
buildgate's protected prefixes (`PI_`, `PIFORK_`, ...).

The image runs as the worker user (`65532`), with a read-only root filesystem
and no network: the launcher must work from an empty home with `/home/worker`
as its only writable directory.

## Building the image

```text
your Dockerfile + build context        base worker image (make sandbox-image)
              |                                   |
              +-------- make pifork-image --------+
                              |
        localhost:5050/buildgate-pifork@sha256:<digest>
        labels: buildgate.image=pifork, buildgate.inputs-hash=<base worker hash>
```

```sh
make pifork-image \
  PIFORK_DOCKERFILE=<your Dockerfile> \
  PIFORK_CONTEXT=<build context, default: the Dockerfile's directory> \
  PIFORK_BUILD_ARGS='<extra docker build flags, e.g. --secret id=ca,src=ca.pem>' \
  BASE_IMAGE=<digest-pinned worker ref, default: a fresh make sandbox-image>
```

The Dockerfile starts from the base worker image and ends as the worker user:

```dockerfile
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
USER root
COPY . /opt/pifork
RUN cd /opt/pifork && npm ci --ignore-scripts && npm run build \
    && install -m 0755 /opt/pifork/buildgate/pifork /usr/local/bin/pifork
USER 65532:65532
WORKDIR /sandbox
RUN --network=none pifork --version
```

| The target does | Detail |
|---|---|
| Resolves and checks `BASE_IMAGE` | Digest-pinned and built by `make sandbox-image` |
| Stamps the kind label | `buildgate.image=pifork`, so `make install` and `configure-images` keep a pifork `sandbox_image` rather than replace it with the plain worker image |
| Stamps the inputs hash | The base worker image's hash only: `factoryd doctor` reports the image stale when the worker image's sources change, and cannot see a change to the fork or its Dockerfile. Rebuild after either |
| Checks the contract | `pifork --version` must run in the built image with no network |
| Prints the reference | The digest-pinned `sandbox_image` value |

Keep credentials, a developer's agent directory and session files out of the
build context: the image is not a place for them, and the worker never needs
one.

Target-project dependencies stay separate from the harness image — a
go-service acceptance run must use an approved project image or the
registry-proxy route; the harness image is not a dependency cache.

## Entry points

- **Single-ticket run:** resolve the preset and host credential, then run
  the build as a `RunWorkflow` whose Activities launch the normal sandbox
  and run the tracked adapter.
- **Worker:** resolve the session config once, pass the request's execution
  harness choice (`-execution-harness`) and explicit overrides to each child
  invocation, and run the same doctor preflight.
- **API:** POST /requests accepts a per-role `harnesses` field (within
  `allowed_harnesses`); POST /runs uses the daemon's own roles. API callers
  cannot select host credential paths.
- **Daemon:** resolve session defaults and host credential discovery at
  startup. Per-execution Temporal input remains authoritative for a request;
  static Worker values are only safe fallbacks.
- **Workflow input (every run, with or without `-repository`):** carry
  adapter/interpreter/image and route policy as credential-free
  per-execution fields. Keep the OAuth
  credential and CA path on the Worker process only.
- **Doctor:** when `pifork` is selected, check the configured model id, the
  local or explicitly supplied image, and the same host-only auth source
  resolution without exposing credentials.

## Failure and rollback

Missing model entitlement, missing auth, malformed auth, unavailable local
  worker image, contradictory explicit build-script settings, a refused
  credential, and network-policy violations fail before acceptance and
  never downgrade to direct worker internet access. A failed launch or
  cleanup is recorded through the existing halt/quarantine paths with
  credential-free diagnostics.

To roll back, remove `harness: pifork` from the roles in session
configuration (or set `harness: pi`). Existing Pi adapter,
canonical-image, and route behavior remain the default.
