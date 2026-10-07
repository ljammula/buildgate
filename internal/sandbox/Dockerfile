# Build from the repo root, not this directory, so the Go module-cache
# step below can see go.mod/go.sum:
#   docker build -f internal/sandbox/Dockerfile -t buildgate-worker:<tag> .
#
# Pinned by digest, not tag: a tag like "python:3.13-slim-bookworm" is
# mutable and can point at different content on a future pull without this
# file changing. Re-resolve each base deliberately (docker pull <tag> &&
# docker inspect --format '{{index .RepoDigests 0}}' <tag>) and update the
# corresponding FROM line explicitly when a base needs to move -- a version
# bump should be as visible in review as a digest bump. Last resolved
# 2026-09-03.
#
# node and golang are pulled in only to copy their toolchains into the
# final python-based image below (COPY --from). All three stages are the
# same Debian release (bookworm), so the copied binaries are glibc-ABI
# compatible with the final base, and each toolchain bump stays a one-line
# digest change here rather than trusting a third-party apt repo (e.g.
# NodeSource) at build time.
# One place for the Copilot CLI version: the npm install below and the
# pre-extracted package directory the final stage selects.
ARG COPILOT_VERSION=1.0.88
FROM node@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5 AS node
FROM golang@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS golang

# bg-forward (cmd/bg-forward): wraps the worker command when the run's
# compose file publishes ports, so localhost:<published port> reaches the
# sidecar. Standard library only; static, so the final image needs nothing
# else for it. The pifork and project images build FROM this one.
FROM golang AS bg-forward
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/bg-forward/ cmd/bg-forward/
RUN CGO_ENABLED=0 go build -trimpath -o /out/bg-forward ./cmd/bg-forward

# pi (the actual coding agent build_app.py drives via `pi -p`) is installed
# here, in its own Node stage, and only its installed tree is copied
# forward into the final image below -- keeps npm and its own transitory
# install-time state out of it. Exact version pinned, never "latest": a
# pinned base digest would otherwise still yield a different image on a
# later rebuild. Bump this alongside a deliberate, reviewed change, same as
# a digest bump.
#
# --ignore-scripts is safe here: `npm view @earendil-works/pi-coding-agent
# scripts` has no preinstall/postinstall/install script (checked
# 2026-09-03) -- if that ever changes, a broken `pi` at this file's own
# build-time smoke test below is exactly the failure that's meant to catch.
FROM node AS pi
RUN --mount=type=secret,id=build-ca,required=false \
    if [ -f /run/secrets/build-ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/build-ca; fi \
    && npm install -g --ignore-scripts @earendil-works/pi-coding-agent@0.84.4

# Codex CLI and Copilot CLI (the other per-role harnesses, see
# internal/harness): each in its own Node stage, exact version, no install
# scripts (`npm view <pkg>@<version> scripts` is empty for both, checked
# 2026-09-29; each ships its native binary as an optionalDependency for the
# build platform, so --ignore-scripts loses nothing). Neither carries a
# credential: the adapters point them at the run's relay with a placeholder
# key and turn their telemetry, update and plugin paths off.
FROM node AS codex
RUN --mount=type=secret,id=build-ca,required=false \
    if [ -f /run/secrets/build-ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/build-ca; fi \
    && npm install -g --ignore-scripts @openai/codex@0.154.0

FROM node AS copilot
ARG COPILOT_VERSION
RUN --mount=type=secret,id=build-ca,required=false \
    if [ -f /run/secrets/build-ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/build-ca; fi \
    && npm install -g --ignore-scripts @github/copilot@${COPILOT_VERSION}

# GNU make, built from source without posix_spawn. The sandbox runtime's
# seccomp filter denies every set-id system call, and a stock make spawns
# each recipe line with POSIX_SPAWN_RESETIDS (setegid/seteuid to its own
# ids), so it cannot run a single recipe there ("make: echo: Operation not
# permitted", found live 2026-10-04). Built on the final stage's own base so
# it links against the libc it will run with. The tarball is pinned by hash.
FROM python@sha256:ed86c82274b3c69b52fb5820f358f0bd7df0b603332063cb5c6e32bd220c3e6e AS gnumake
ARG MAKE_VERSION=4.4.1
ARG MAKE_SHA256=dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates make gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
RUN --mount=type=secret,id=build-ca,required=false \
    if [ -f /run/secrets/build-ca ]; then export SSL_CERT_FILE=/run/secrets/build-ca; fi \
    && cd /tmp \
    && python3 -c "import urllib.request; urllib.request.urlretrieve('https://mirrors.kernel.org/gnu/make/make-${MAKE_VERSION}.tar.gz', 'make.tar.gz')" \
    && echo "${MAKE_SHA256}  make.tar.gz" | sha256sum -c - \
    && tar xzf make.tar.gz && cd make-${MAKE_VERSION} \
    && ./configure --disable-posix-spawn --without-guile --disable-dependency-tracking \
    && make -j"$(nproc)" && install -m 0755 make /usr/local/bin/make-no-posix-spawn

FROM python@sha256:ed86c82274b3c69b52fb5820f358f0bd7df0b603332063cb5c6e32bd220c3e6e

# git: build_app.py shells straight to it (rev-parse, init, add, commit --
# see scripts/build_app.py in pi-harness-hardening; pure stdlib subprocess
# calls, no GitPython). gcc/libc6-dev: `make verify`'s own `go test -race`
# needs cgo and a C compiler, and slim has neither. make (this repo's own
# Makefile entry point, and most target repos' verify command) comes from the
# gnumake stage above, not from apt.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
COPY --from=gnumake /usr/local/bin/make-no-posix-spawn /usr/local/bin/make

RUN groupadd --gid 65532 worker \
    && useradd --uid 65532 --gid worker --create-home --shell /usr/sbin/nologin worker

# Node + pi, copied from the pi stage above. The copied node_modules stays
# root-owned (the COPY --from default) -- the worker must not be able to
# rewrite its own agent. internal/sandbox/docker.go sets a fixed
# PATH=/usr/local/bin:/usr/bin:/bin at launch unconditionally, so these are
# symlinks into that path rather than a PATH/ENV entry, which the launch
# path would silently discard.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=pi /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=codex /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=copilot /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s /usr/local/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js /usr/local/bin/pi \
    && ln -s /usr/local/lib/node_modules/@openai/codex/bin/codex.js /usr/local/bin/codex \
    && ln -s /usr/local/lib/node_modules/@github/copilot/npm-loader.js /usr/local/bin/copilot \
    && ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm

# --- project profile: Go -------------------------------------------------
# Baked into this "generic worker" image, not split into a separate
# overlay, deliberately: with no image registry, an overlay's own
# `FROM buildgate-worker-base@sha256:...` would force a build ->
# push -> inspect -> repin cycle on every base change, for exactly one
# consumer today. The block below is one COPY, two symlinks, and a baked
# module cache -- splitting it out later is mechanical. GOMODCACHE is what
# actually forces that split eventually: it's baked against *this* target
# repo's own go.sum below, so a second target repo with a different module
# set is the right trigger to split this into its own image, not a guess
# made now.
COPY --from=golang /usr/local/go /usr/local/go
COPY --from=bg-forward /out/bg-forward /usr/local/bin/bg-forward
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
# GOTOOLCHAIN=local (not Go's default "auto"): go.mod declares "go 1.26".
# If this image's own Go were ever older than that, Go's default behavior
# is to try to *download* a newer toolchain -- which fails offline
# (internal/sandbox launches with --network none or the relay-only
# network) with a confusing error far from its real cause. GOTOOLCHAIN=local
# instead fails immediately and legibly right here, at the version mismatch.
ENV GOTOOLCHAIN=local
ENV GOMODCACHE=/usr/local/gomodcache
COPY go.mod go.sum /tmp/modcache-src/
RUN --mount=type=secret,id=build-ca,required=false \
    if [ -f /run/secrets/build-ca ]; then export SSL_CERT_FILE=/run/secrets/build-ca; fi \
    && cd /tmp/modcache-src && go mod download && rm -rf /tmp/modcache-src
# ---------------------------------------------------------------------------

# $HOME must be writable at runtime despite the container's own --read-only
# root filesystem: `go test` links its compiled test binary under GOTMPDIR
# (defaulted here into $HOME) and execs it directly, and internal/sandbox/
# docker.go's own /tmp mount is deliberately noexec, so /tmp can't be
# GOTMPDIR's home. `go build`/`go test` also need a writable GOCACHE, and
# pi needs a writable ~/.pi config/state dir (its own package.json declares
# "piConfig":{"configDir":".pi"}, relative to $HOME, not XDG). All three
# resolve under the /home/worker tmpfs docker.go mounts at launch --
# ENV-baked here as the image's own default so a caller that ever forgets
# that mount gets a legible failure rather than a silently wrong path.
ENV HOME=/home/worker
ENV GOCACHE=/home/worker/.cache/go-build
ENV GOTMPDIR=/home/worker
ENV npm_config_cache=/home/worker/.npm
# Copilot CLI would otherwise fetch newer builds of itself at launch.
ENV COPILOT_AUTO_UPDATE=false
# Copilot's launcher unpacks its bundled package (~8 s) into
# $XDG_CACHE_HOME/copilot/pkg/<platform>/<version> on every first launch in a
# fresh container. Unpack it once here, root-owned and read-only at runtime,
# and select it with COPILOT_CLI_DIST_DIR (scoped to Copilot, unlike setting
# XDG_CACHE_HOME image-wide, which every other cache-using tool would follow).
ARG COPILOT_VERSION
RUN XDG_CACHE_HOME=/opt/copilot-cache copilot --version \
    && ln -s /opt/copilot-cache/copilot/pkg/*/${COPILOT_VERSION} /opt/copilot-pkg \
    && test -f /opt/copilot-pkg/index.js
ENV COPILOT_CLI_DIST_DIR=/opt/copilot-pkg

RUN mkdir -p /sandbox && chown 65532:65532 /sandbox
USER 65532:65532
WORKDIR /sandbox

# Build-time smoke test: a broken symlink or missing toolchain fails the
# image build right here, not a sandboxed run three weeks from now.
RUN pi --version && codex --version && copilot --version && git --version && go version && node --version \
    && printf 'all:\n\t@true\n' | make -f - \
    && BG_COMPOSE_FORWARDS= bg-forward -- true
