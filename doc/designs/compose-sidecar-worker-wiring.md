# Compose Sidecar Worker Wiring

How a target repository's Compose sidecars (Postgres, Redis, Kafka, a
migration job) are launched and reached from the sandboxed worker, for a
target whose canonical `make test` needs them.

## Compose fields

| Source field | Handling |
|---|---|
| `platform` | Accepted only as `linux/amd64` or `linux/arm64`; emitted in the synthesized file |
| `container_name` | Accepted as inert metadata, never emitted: Buildgate's project names, labels and service aliases stay the only identities |
| Relative bind, source in the base commit | Copied with `git archive <baseSHA>` into `<run>/compose/bind-inputs/` and mounted `:ro` |
| Relative bind, source absent from the base commit | Becomes an empty per-run named volume (what Docker would create on the host for `./pgdata`), removed by `down -v` |
| Absolute path, `~`, `..`, `.git`, symlink, special file, bind options | Rejects the whole Compose configuration |

The worker's mutable checkout is never a sidecar input. `bind-inputs/` is
cleared before each lifecycle materializes it and removed after confirmed
Compose and network cleanup.

## Worker network

```text
worker (NetworkMode none) --> supervisor --> <service address>:<port> on bg-compose-<run>
                              policy: one allowed_ips endpoint per service
```

`bg-compose-<run>` is `--internal` and the worker never joins it: the
sandbox's policy admits each service's address and port, read after each
attempt's start, and nothing else. A service on a port the supervisor never
admits by address (2379, 2380, 6443, 10250, 10255) rejects the compose
file.

## Worker localhost

Nothing is published on the host. Each fixed TCP `ports:` mapping becomes a
forward on the worker's own loopback instead:

```text
compose:   postgres ports "5433:5432"
lifecycle: BG_COMPOSE_FORWARDS=5433=<postgres address>:5432      (sorted by port)
worker:    bg-forward -- <command>
             binds 127.0.0.1:5433 and [::1]:5433, then runs <command> as its child
             localhost:5433  ->  <postgres address>:5432 on bg-compose-<run>, through the supervisor
```

- A test that defaults to what `docker compose up` publishes on a developer
  machine reaches the sidecar with no configuration. A Kafka broker that
  advertises `localhost:<published port>` works too: the client's reconnect
  comes back through the same forward.
- `bg-forward` (`cmd/bg-forward`) is baked into the worker image; the pifork
  and project images build `FROM` it. It dials only the run's own services,
  which the worker's policy already admits by address, so it adds no route.
- Not forwarded: a mapping with no host port (`"6379"`, a random host port)
  or a UDP one. Two services publishing the same host port reject the
  compose file, as `docker compose up` would fail.
- A compose file that publishes nothing leaves the worker command unwrapped.
- Binding `::1` matters: Go dials `[::1]` first for `localhost`.

## Worker environment

| Variable | Source |
|---|---|
| `BG_SERVICE_<NAME>` (the service's address), `BG_SERVICE_<NAME>_PORT`, `BG_COMPOSE_SERVICES` | Generated from the launched services |
| `BG_COMPOSE_FORWARDS` | Generated from the published ports; read by `bg-forward` |
| `compose_services_worker_env` entries (e.g. `PSQL_URL`, `REDIS_URL`) | Operator session config, read by the host process that launches the sidecars |

Operator entries:

- are rejected when the key is a factory, sandbox, harness, credential,
  cache, proxy or `SF_*` name (compared case-insensitively), collides with a
  generated service variable, or is already set on the launch;
- are rejected when the value names a service as a host: the worker resolves
  no service name, so the target repo reads `BG_SERVICE_<NAME>` instead;
- are sorted and kept out of the recorded command, so never in `run.json`
  attempts, Temporal workflow input or Temporal activity results. They are
  in the gateway's sandbox record, readable through the gateway API by a
  holder of the factory certificate.

## Worker scratch

Every worker launch gets its own disk-backed scratch directory
(`<data-dir>/scratch/<run>/<container>`, mounted at `/scratch`), with or
without the registry proxy. `GOCACHE` and `GOTMPDIR` point into it; with the
proxy's Go route, `GOMODCACHE` does too. Build, canonical verify, full-suite
verify and each retry use separate directories, removed when the container
exits.
