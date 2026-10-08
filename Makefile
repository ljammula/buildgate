.PHONY: meter-proto with-spinner-test console-walk test vet fmt-check verify verify-live coverage console-test console-build console-build-optional agent-pi-test ci install-prereqs install-prereqs-test live-smoke live-compose live-smoke-results live-smoke-test live-round live-round-results live-round-test bar bar-test proving-ground proving-ground-results proving-ground-test install temporal-up local-images sandbox-image pifork-image project-sandbox-image meter-image openshell-images registry-proxy-image .local-registry

# data/ is gitignored runtime state (queue entries, workspaces, tickets --
# see AGENTS.md's repo-layout table) that can contain arbitrary .go files
# copied in from a target repo's own build, not this module's source. `go
# list ./...`/`gofmt -l .` walking into it makes verify depend on what
# happens to be sitting in an operator's data/ dir instead of this repo's
# own source tree, so every Go target below is scoped away from it.
GO_PACKAGES = $(shell go list ./... | grep -v '/data/')
GO_COVERPKG = $(shell echo $(GO_PACKAGES) | tr ' ' ',')

# Optional PEM bundle for a host TLS-interception proxy. It is passed to
# Docker only as a BuildKit secret for host-side dependency downloads.
# pip (project images) reads it in place of its own roots, so it must hold every CA pip needs.
# The console's npm reads it as NODE_EXTRA_CA_CERTS, beside Node's own roots.
# Not given, it is found on first use: the hidden `factoryd build-ca-bundle`
# subcommand prints the bundle it wrote when this network intercepts TLS,
# nothing otherwise. The result is exported, so a sub-make does not ask again.
BUILD_CA_BUNDLE ?= $(eval export BUILD_CA_BUNDLE := $(shell go run ./cmd/factoryd build-ca-bundle))$(BUILD_CA_BUNDLE)
# Without the proxy's CA, npm cannot verify the registry; under some Node
# versions it then dies with "Exit handler never called!" instead of naming
# the certificate.
CONSOLE_NPM_CA = $(if $(BUILD_CA_BUNDLE),NODE_EXTRA_CA_CERTS="$(abspath $(BUILD_CA_BUNDLE))")

# go test -race over GO_PACKAGES, with cmd/factoryd (~9 minutes serially
# under -race) split into parallel processes: see scripts/test-sharded.sh.
test:
	scripts/test-sharded.sh $(GO_PACKAGES)

vet:
	go vet $(GO_PACKAGES)

fmt-check:
	@unformatted="$$(git ls-files '*.go' | xargs gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

# Regenerates the committed OpenShell supervisor-middleware Go stubs from the
# vendored protos. buf runs through go run at a pinned version and is not in go.mod.
meter-proto:
	cd internal/meter/middlewarepb && go run github.com/bufbuild/buf/cmd/buf@v1.50.0 generate

verify: fmt-check vet test

# verify-live: the live counterpart to `verify` -- every DOCKER_SANDBOX_LIVE=1
# test (real Docker sandbox) plus every Temporal-live test (real
# `temporal server`, no self-skip), the same coverage ci.yml's own steps get.
# Not part of `verify` or CI's default job: needs Docker, several minutes, and
# (ideally) a running Temporal dev server -- see scripts/verify-live.sh's own
# top-of-file comment. Intended for the nightly self-hosted-runner job and for
# a developer machine with Docker.
verify-live:
	scripts/verify-live.sh

# coverage reports statement coverage across the whole module, merging two
# halves that a plain `go test -cover` cannot see together: in-process
# test coverage, and coverage from inside the compiled `factoryd` binary
# that most cmd/factoryd TestIntegration* tests drive as a real subprocess
# rather than in-process (see cmd/factoryd/integration_test.go's own
# runTests doc comment on FACTORYD_TEST_SUBPROCESS_COVERDIR -- without
# this, a function like run_temporal.go's
# rollbackIsolatedWorkspaceIfTerminated reports 0% despite having real,
# passing coverage). FACTORYD_TEST_SUBPROCESS_COVERDIR set before `go
# test` runs makes runTests build that binary with -cover too, writing
# its own counters into the requested directory and setting GOCOVERDIR to
# it explicitly itself -- a separate env var from GOCOVERDIR on purpose:
# go test's own coverage machinery can redirect GOCOVERDIR for the test
# process before runTests ever sees it, at least on some Go versions
# (found via Codex review of an earlier version of this target, which
# used GOCOVERDIR directly and reproduced empty on Go 1.25), so the
# directory a caller actually asked for needs a name go test's own setup
# has no reason to touch. go tool covdata converts the counters that
# land there to a text profile, and the two mode:count profiles are
# combined by concatenation (the standard technique for merging coverage
# profiles from separate runs of identical source -- go tool cover sums
# counts for a block it sees more than once, it does not overwrite), not
# covdata merge, since only one of the two ever started as covdata's own
# binary format.
coverage:
	rm -rf .coverage
	mkdir -p .coverage/subprocess
	FACTORYD_TEST_SUBPROCESS_COVERDIR=$(CURDIR)/.coverage/subprocess go test $(GO_PACKAGES) -covermode=count -coverpkg=$(GO_COVERPKG) -coverprofile=.coverage/inprocess.out
	go tool covdata textfmt -i=.coverage/subprocess -o=.coverage/subprocess.out
	{ head -1 .coverage/inprocess.out; tail -n +2 .coverage/inprocess.out; tail -n +2 .coverage/subprocess.out; } > .coverage/merged.out
	go tool cover -func=.coverage/merged.out | tail -1
	@echo "per-function detail: go tool cover -func=.coverage/merged.out"
	@echo "HTML: go tool cover -html=.coverage/merged.out"

# The console's checks (typecheck, lint, format, Vitest). Node and npm are a
# separate toolchain and intentionally not part of verify.
console-test:
	cd console && $(CONSOLE_NPM_CA) npm ci && npm run check

# Builds the React console (console/dist) and embeds it into
# internal/consoleweb/dist so `factoryd serve` serves it directly, same
# origin as the API (see that package's own doc comment) -- needs Node 20+
# and npm, so this is never part of `verify`. The bundle calls the API by
# relative URL, which is exactly right once the same origin serves both.
# npm runs no install scripts (console/.npmrc).
console-build:
	cd console && $(CONSOLE_NPM_CA) npm ci && npm run build
	rm -rf internal/consoleweb/dist
	mkdir -p internal/consoleweb/dist
	cp -R console/dist/. internal/consoleweb/dist/
	touch internal/consoleweb/dist/.gitkeep

# Best-effort console-build: the binary serves internal/consoleweb's built-in
# placeholder page when npm isn't on PATH or the build fails (an npm that
# crashes under the machine's Node, a registry it cannot reach), so
# `make install` still installs factoryd on a machine with no working Node
# toolchain. A failed build empties internal/consoleweb/dist first: a bundle
# left by an earlier build would otherwise be embedded in the new binary.
console-build-optional:
	@if ! command -v npm >/dev/null 2>&1; then \
		echo "npm not installed -- factoryd will serve the console placeholder page (see console/README.md for make console-build)"; \
	elif ! $(MAKE) console-build; then \
		rm -rf internal/consoleweb/dist; \
		mkdir -p internal/consoleweb/dist; \
		touch internal/consoleweb/dist/.gitkeep; \
		echo "warning: console build failed (node $$(node --version 2>/dev/null || echo not found), npm $$(npm --version 2>/dev/null || echo unknown); it needs Node 20+) -- factoryd will serve the console placeholder page. Fix the error above and re-run 'make install' for the console; 'factoryd doctor' says whether this network intercepts TLS." >&2; \
	fi

# agent/pi/'s own Python toolchain, same reasoning as console-test above --
# intentionally not part of verify.
agent-pi-test:
	python3 -m pytest agent/pi/tests/

# ci runs, on this machine, exactly what .github/workflows/ci.yml's `verify`
# job runs (fmt-check/vet/test against a live Temporal dev server -- see that
# job's own comments for why -- plus the live Docker test set), and adds
# the two toolchains ci.yml doesn't cover yet: the Python harness test suite
# (agent-pi-test above; it covers both engines) and, when npm is
# installed, the console's own test suite. Not a Makefile alias for `verify`:
# it needs Docker and (ideally) Temporal, same prerequisites as verify-live,
# and takes minutes rather than verify's seconds.
ci: fmt-check vet verify-live agent-pi-test with-spinner-test install-prereqs-test
	@if command -v npm >/dev/null 2>&1; then \
		$(MAKE) console-test; \
	else \
		echo "npm not installed -- skipping console-test (see console/README.md)"; \
	fi

# live-smoke runs factoryd's real pipeline (real sandbox through the
# OpenShell gateway, real model route, real git) end to end against a small fixed set of
# tickets -- see AGENTS.md's "Live validation" section and
# scripts/live-smoke.sh's own top-of-file comment for why this exists
# separately from verify: every gate/build_app.py/conformity-review bug
# this repo has hit live was invisible to unit tests and code review
# alike, and only surfaced on a real run. Not part of `verify` (needs
# Docker, a configured model route, and ~5-10 minutes, unlike verify's
# seconds) -- run explicitly before merging any change that touches the
# build/gate/sandbox-runtime/policy pipeline.
live-smoke:
	scripts/live-smoke.sh

# live-compose runs one real todo-kafka-service ticket with its postgres,
# kafka and redis compose sidecars (see scripts/live-compose.sh's header).
# Periodic, and before merging a compose-services change; not part of
# live-smoke, which stays fast.
live-compose:
	scripts/live-compose.sh

# console-walk is the console's live browser walk: a real factoryd serving a
# seeded data directory, headless Chrome through every screen and action,
# each write checked against the server's own record (console/test/walk/
# run.sh's header). Needs Go, Node and Playwright's Chromium. The walk that
# drives one real request through drafting and a build with a model route is
# scripts/console-walk/run.sh (its header lists the prerequisites).
console-walk:
	console/test/walk/run.sh

# live-smoke-results prints the last 20 recorded live-smoke runs (see
# LIVE_SMOKE_RESULTS_FILE in scripts/live-smoke.sh's own top-of-file comment)
# as a table. Needs no Docker, factoryd, or network.
live-smoke-results:
	scripts/live-smoke.sh --results

# live-smoke-test: offline unit test for live-smoke.sh's own result-recording
# logic (the LIVE_SMOKE_RESULTS_FILE JSONL append, and --results) -- no
# Docker, no real factoryd, no network; drives the real live-smoke.sh against
# a throwaway git fixture repo and a stub FACTORYD_BIN. See
# scripts/tests/test_live_smoke_recording.py. Not folded into bar-test, which
# is scoped to bar's own summariser/verdict logic.
live-smoke-test:
	python3 -m unittest discover -s scripts/tests -p 'test_live_smoke_recording.py' -v

# live-round: one real PR-review corrective round on a real pull request
# (scripts/live_round.py's doc comment): submit a fixture's spec and plan,
# comment on its pull request as LIVE_ROUND_REVIEWER, and pass only if every
# comment is answered by a pushed commit within max_review_rounds. It opens a
# pull request on LIVE_ROUND_REPO's origin, so it is never part of verify.
live-round:
	python3 scripts/live_round.py

# live-round-results: the recorded runs and the share of reviewer comments
# resolved within the cap.
live-round-results:
	python3 scripts/live_round.py --results

# live-round-test: offline unit tests for live_round.py's own logic.
live-round-test:
	python3 -m unittest scripts/tests/test_live_round.py -v

# install-prereqs is `make install`'s first step: Homebrew installs the
# tools the later steps need and this machine lacks, Docker's buildx plugin
# is linked, and a Docker that does not answer is started through colima
# (scripts/install-prereqs.sh's header has the table).
install-prereqs:
	@scripts/install-prereqs.sh

# install-prereqs-test: offline tests for scripts/install-prereqs.sh and
# scripts/install-finish.sh against stand-in brew, docker, colima, gh and
# factoryd. Installs and starts nothing.
install-prereqs-test:
	python3 -m unittest scripts/tests/test_install_prereqs.py scripts/tests/test_install_finish.py -v

# with-spinner-test: offline test for scripts/with-spinner.sh, the wrapper
# `make install` runs its long builds through (exit-status passthrough, the
# plain path, and the terminal path under a pty). No Docker.
with-spinner-test:
	python3 -m unittest scripts/tests/test_with_spinner.py -v

# bar measures the staged-oracle default-on bar (`-draft-oracles`): BAR_ROUNDS
# rounds (default 2) of scripts/bar/fixtures.json, each flag off then on, one
# run at a time, pausing at oracle_review for the operator, then a dated
# markdown results table and a verdict on the four bar criteria. Hours, a
# human in the loop, Docker + model + Temporal -- never part of verify. See
# scripts/bar.sh's top-of-file comment; `scripts/bar.sh --list` needs nothing.
bar:
	scripts/bar.sh

# bar-test: offline unit tests for bar's summariser/verdict logic (no Docker,
# no model). Not folded into verify, which is the Go toolchain only.
bar-test:
	python3 -m unittest discover -s scripts/tests

# proving-ground: the standing corpus of pre-approved ticket specs on
# testdata/fixtures (scripts/proving-ground/fixtures.json, plus an operator's
# own via PROVING_GROUND_EXTRA_FIXTURES), including deliberate
# should-quarantine fixtures, driven one at a time through factoryd. Every run is auto-classified against its fixture's declared outcome
# and appended to PROVING_GROUND_RESULTS_FILE; the confidence bar (false
# accepts, factoryd-caused false quarantines, non-acceptance classification
# coverage, one-shot acceptance rate) prints at the end. Needs Docker and a
# working model route and sandbox runtime, each fixture's source repo, and makes
# real model calls -- not part of verify or live-smoke. See
# scripts/proving-ground.sh's own top-of-file comment; `scripts/proving-ground.sh
# --list` and `make proving-ground-test` need neither.
proving-ground:
	scripts/proving-ground.sh

# proving-ground-results prints recent recorded proving-ground runs, grouped
# by run date/SHA, with the confidence bar for each group. Needs no Docker,
# factoryd, or network.
proving-ground-results:
	scripts/proving-ground.sh --results

# proving-ground-test: offline tests for the classifier (scripts/
# proving_ground_lib.py), the fixtures file's own schema and every fixture's
# ticket spec parse-ability (factoryd check-ticket), and proving-ground.sh's
# result-recording plumbing against a stub FACTORYD_BIN -- no Docker, no real
# factoryd route, no real fixture repos, no model calls. See
# scripts/tests/test_proving_ground.py.
proving-ground-test:
	python3 -m unittest discover -s scripts/tests -p 'test_proving_ground.py' -v

# Brings up the local Temporal server (docker-compose.temporal.yml:
# Postgres-backed, `restart: unless-stopped`) so it's already running by
# the time `factoryd quickstart`/`<run>` looks for it at -temporal-address's
# default -- best-effort and silently skipped without Docker, same style
# as console-build-optional above, so `make install` keeps working on a
# machine that never installed Docker (builds there need a Temporal server of
# its own). Idempotent: `up -d`
# against containers already running from a previous `make install` is a
# no-op.
temporal-up:
	@if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then \
		echo "Starting Temporal (docker compose -f docker-compose.temporal.yml up -d)..."; \
		docker compose -f docker-compose.temporal.yml up -d; \
		echo "Waiting for Temporal to become ready on localhost:7233..."; \
		for i in $$(seq 1 60); do \
			if nc -z 127.0.0.1 7233 2>/dev/null; then echo "Temporal is ready."; exit 0; fi; \
			sleep 1; \
		done; \
		echo "Temporal did not become ready within 60s -- check 'docker compose -f docker-compose.temporal.yml logs temporal'."; \
	else \
		echo "Docker not available -- skipping Temporal bring-up (builds need Temporal and will halt until it is running, see USAGE.md, "Temporal: what runs every build")."; \
	fi

# Builds the sandbox/meter/registry-proxy images locally (via sandbox-image/
# meter-image/registry-proxy-image below, each already pushing through
# .local-registry and printing a real digest-pinned ref) and writes those
# refs into the session config (factoryd configure-images) so they
# become this machine's default sandbox_image/meter_image/
# registry_proxy_image. Every image factoryd launches is built from
# source (see AGENTS.md) -- there is no published fallback -- so unlike
# temporal-up/console-build-optional above, this is NOT best-effort:
# Docker is required, and `make install` fails loudly without it rather
# than silently leaving factoryd with no usable image.
#
# With FACTORYD_CONFIG empty the refs go to every profile (`factoryd use`
# lists them), so no profile keeps pinning a superseded image.
# FACTORYD_CONFIG (optional) is forwarded to configure-images as -config,
# so the refs land in that exact session config file instead of always
# the default path -- `factoryd quickstart`'s own stale-image rebuild
# passes its own effective config path here, so a config it is already
# using (not necessarily the default one) gets the fresh digests. It may
# name several space-separated configs (`make install
# FACTORYD_CONFIG="~/.config/factoryd/config.yml ~/.config/factoryd/codex-luna.yml"`):
# each is re-pointed, so no config is left pinning a superseded image
# (paths must not contain spaces).
local-images: docker-buildx-check
	@: one lookup for the image builds below and the console build after them: $(BUILD_CA_BUNDLE)
	@if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then \
		echo "Docker is required to build the sandbox/meter/registry-proxy images (every image factoryd launches is built from source, never published) -- start Docker (colima: 'colima start --memory 4') and re-run 'make install'." >&2; exit 1; \
	fi
	@echo "Building sandbox/meter/registry-proxy images locally (several minutes)..."; \
	sandbox_ref="$$($(MAKE) sandbox-image | grep -oE 'localhost:5050/buildgate-worker@sha256:[0-9a-f]+' | tail -1)"; \
	meter_ref="$$($(MAKE) meter-image | grep -oE 'localhost:5050/factoryd-meter@sha256:[0-9a-f]+' | tail -1)"; \
	registry_proxy_ref="$$($(MAKE) registry-proxy-image | grep -oE 'localhost:5050/factoryd-registry-proxy@sha256:[0-9a-f]+' | tail -1)"; \
	if [ -z "$$sandbox_ref" ] || [ -z "$$meter_ref" ] || [ -z "$$registry_proxy_ref" ]; then \
		echo "one or more local image builds did not produce a digest -- see output above" >&2; exit 1; \
	fi; \
	$(if $(FACTORYD_CONFIG),for config in $(FACTORYD_CONFIG); do ,)go run ./cmd/factoryd configure-images -sandbox-image "$$sandbox_ref" -meter-image "$$meter_ref" -registry-proxy-image "$$registry_proxy_ref" $(if $(FACTORYD_CONFIG),-config "$$config" || exit 1; done,-all-profiles)

# Every image build uses BuildKit (RUN --mount, --secret); the legacy builder
# fails each one with "the --mount option requires BuildKit". A Homebrew
# CLI-only Docker (docker + colima) has no buildx plugin, so check before
# `install` pulls Temporal or starts a build. Skipped when Docker is absent --
# local-images reports that case itself.
docker-buildx-check:
	@if command -v docker >/dev/null 2>&1 && ! docker buildx version >/dev/null 2>&1; then \
		echo "docker buildx (BuildKit) is required to build the images -- install it, then re-run:" >&2; \
		if [ "$$(uname)" = Darwin ]; then \
			echo "  brew install docker-buildx && mkdir -p ~/.docker/cli-plugins && ln -sf \"\$$(brew --prefix)/lib/docker/cli-plugins/docker-buildx\" ~/.docker/cli-plugins/docker-buildx" >&2; \
		else \
			echo "  install your distribution's docker-buildx-plugin: https://docs.docker.com/build/install-buildx/" >&2; \
		fi; \
		exit 1; \
	fi

# Installs to $(go env GOBIN), else the first $(go env GOPATH) entry's bin
# (usually ~/go/bin); warns if another factoryd wins. The one command a
# machine needs: install-prereqs before it installs missing tools and starts
# Docker, and scripts/install-finish.sh after it puts that directory on PATH,
# logs gh in and runs `factoryd doctor -fix`. INSTALL_FINISH=0 leaves the
# last step out, for `factoryd upgrade`, which restarts and checks the
# machine itself.
#
# The version is stamped from THIS checkout's HEAD: Go's own vcs.revision
# stamping treats only a .git directory as a repository root, so in a git
# worktree (whose .git is a file) it walks up and stamps the parent
# checkout's HEAD instead. The check after install fails the target if the
# installed binary reports anything else.
FACTORYD_VERSION = $(shell git rev-parse --short=12 HEAD)$(shell test -z "$$(git status --porcelain --untracked-files=no)" || echo -dirty)
install: install-prereqs docker-buildx-check temporal-up local-images openshell-images console-build-optional
	scripts/with-spinner.sh "installing factoryd" go install -ldflags "-X main.version=$(FACTORYD_VERSION)" ./cmd/factoryd
	@bindir="$$(go env GOBIN)"; [ -n "$$bindir" ] || bindir="$$(go env GOPATH | cut -d: -f1)/bin"; \
	installed="$$bindir/factoryd"; \
	got="$$("$$installed" version)"; \
	if [ "$$got" != "factoryd version $(FACTORYD_VERSION)" ]; then \
		echo "error: $$installed reports '$$got', want 'factoryd version $(FACTORYD_VERSION)'" >&2; exit 1; \
	fi; \
	resolved="$$(command -v factoryd 2>/dev/null || true)"; \
	if [ -n "$$resolved" ] && ! [ "$$resolved" -ef "$$installed" ]; then \
		echo "warning: just installed $$installed, but 'factoryd' on your PATH resolves to $$resolved instead -- that earlier PATH entry will keep winning. Remove/rename $$resolved, or move $$installed earlier on PATH, then re-run 'factoryd doctor' to confirm (see its PATH shadowing check)." >&2; \
	fi; \
	"$$installed" install-skill || echo "warning: buildgate skill not installed; run 'factoryd install-skill'" >&2; \
	if [ -d "$$HOME/.claude/skills/buildgate" ]; then \
		"$$installed" install-skill -dir "$$HOME/.claude/skills" || echo "warning: buildgate skill not refreshed in ~/.claude/skills" >&2; \
	fi; \
	if [ "$(INSTALL_FINISH)" != 0 ]; then scripts/install-finish.sh "$$installed"; fi

# A local, ephemeral Docker registry these targets push through to get a
# real, addressable digest for a local build -- mirrors ci.yml's own
# "Local Docker registry" step, and exists for the
# same reason: `docker inspect ...Id` is not reliably addressable as
# name@sha256:<id> on every Docker engine/storage-driver combination
# (found via a real GitHub Codex App review of PR #51 -- this repo's own
# CI runner is one of the ones that rejects it), while a real registry
# push always produces a digest every engine resolves the same way, and
# the pushed image is already in the local Docker image store under that
# exact digest -- addressable by it (via -sandbox-image) with no further
# network round trip needed, registry container included or not.
# Idempotent: a second call reuses an already-running instance. Host port
# 5050, not the registry's own default 5000 (also what ci.yml's Linux
# runner uses): macOS's AirPlay Receiver squats on 5000 by default, so a
# Mac dev machine would otherwise silently fail to bind it.
.local-registry:
	@docker inspect factoryd-local-registry >/dev/null 2>&1 || \
	  docker run -d --restart=always -p 127.0.0.1:5050:5000 --name factoryd-local-registry registry:2 >/dev/null
	@for i in $$(seq 1 30); do \
	  curl -sf http://localhost:5050/v2/ >/dev/null 2>&1 && exit 0; \
	  sleep 1; \
	done; \
	echo "local Docker registry did not become healthy within 30s" >&2; exit 1

# Builds the sandbox worker image (internal/sandbox/Dockerfile) locally
# and pushes it through the local registry above, printing a real,
# digest-pinned reference usable with -sandbox-image. This is the only way
# to get a sandbox worker image -- every image factoryd launches is built
# from source, never published or pulled from a registry (see AGENTS.md).
# `make install`'s own local-images target calls this and records the
# printed ref via `factoryd configure-images`. Stamped with a
# buildgate.inputs-hash label (internal/imageinputs.Hash, via the
# hidden `factoryd image-inputs-hash` subcommand) so a later `factoryd
# doctor`/`quickstart` can tell whether this image is stale against the
# checkout it was built from, and a buildgate.image=worker label
# naming which kind of image this is to hash against -- Docker inherits
# labels through FROM, so a derived image (project-sandbox-image,
# pifork-image) must override this label with its own kind, or the
# staleness check would wrongly hash it as a plain worker.
#
# Skips `docker build` when the hidden `factoryd image-reuse` subcommand
# reports the local image already carries this exact inputs hash (see
# cmd/factoryd/image_reuse.go for why that is safe for
# worker/meter/registry-proxy but not pifork). The push still runs:
# it is a no-op for an unchanged image, returns the same digest, and puts
# the image back if the local registry lost it. FORCE_IMAGE_BUILD=1 always
# rebuilds.
sandbox-image: .local-registry
	@set -e; \
	ca_bundle="$(BUILD_CA_BUNDLE)"; \
	if [ -n "$$ca_bundle" ] && [ ! -f "$$ca_bundle" ]; then echo "$$ca_bundle is not a readable CA bundle" >&2; exit 1; fi; \
	inputs_hash="$$(go run ./cmd/factoryd image-inputs-hash -repo-root . worker)"; \
	if [ -z "$$inputs_hash" ]; then echo "image-inputs-hash failed or printed nothing for worker" >&2; exit 1; fi; \
	if [ "$$(go run ./cmd/factoryd image-reuse -inputs-hash "$$inputs_hash" $(if $(filter 1,$(FORCE_IMAGE_BUILD)),-force) worker localhost:5050/buildgate-worker:local)" != reuse ]; then \
	  scripts/with-spinner.sh "building sandbox image" docker build $${ca_bundle:+--secret "id=build-ca,src=$$ca_bundle"} -f internal/sandbox/Dockerfile \
	    --label buildgate.inputs-hash="$$inputs_hash" \
	    --label buildgate.image=worker \
	    -t localhost:5050/buildgate-worker:local .; \
	fi; \
	push_output="$$(docker push localhost:5050/buildgate-worker:local)"; \
	echo "$$push_output"; \
	digest="$$(printf '%s\n' "$$push_output" | grep -oE 'sha256:[0-9a-f]+' | tail -1)"; \
	if [ -z "$$digest" ]; then echo "could not read a digest from docker push output" >&2; exit 1; fi; \
	echo "localhost:5050/buildgate-worker@$$digest"

# Builds a worker image for the pifork harness (a fork of Pi) from the
# operator's own Dockerfile, on top of the plain sandbox worker image
# (BASE_IMAGE; built fresh via `make sandbox-image` when left unset).
# PIFORK_DOCKERFILE is mandatory; PIFORK_CONTEXT is the build context
# (default: the Dockerfile's directory); PIFORK_BUILD_ARGS is passed to
# `docker build` unchanged (build args, secrets). The Dockerfile starts with
# `ARG BASE_IMAGE` / `FROM ${BASE_IMAGE}` and must meet the image contract in
# doc/designs/pifork-harness.md. Nothing about the fork lives in this
# repository, so the stamped inputs hash covers the base worker image only.
pifork-image: .local-registry
	@if [ -z "$(PIFORK_DOCKERFILE)" ]; then \
	  echo "usage: make pifork-image PIFORK_DOCKERFILE=<your Dockerfile> [PIFORK_CONTEXT=<build context>] [PIFORK_BUILD_ARGS='--build-arg ...'] [BASE_IMAGE=<digest-pinned worker ref>]" >&2; exit 1; \
	fi
	@set -e; \
	test -f "$(PIFORK_DOCKERFILE)" || { echo "$(PIFORK_DOCKERFILE) is not a readable Dockerfile" >&2; exit 1; }; \
	context="$(PIFORK_CONTEXT)"; \
	if [ -z "$$context" ]; then context="$$(dirname "$(PIFORK_DOCKERFILE)")"; fi; \
	test -d "$$context" || { echo "$$context is not a directory" >&2; exit 1; }; \
	base_image="$(BASE_IMAGE)"; \
	if [ -z "$$base_image" ]; then \
	  if ! sandbox_build_out="$$($(MAKE) sandbox-image)"; then \
	    echo "make sandbox-image failed while resolving BASE_IMAGE:" >&2; \
	    printf '%s\n' "$$sandbox_build_out" >&2; \
	    exit 1; \
	  fi; \
	  base_image="$$(printf '%s\n' "$$sandbox_build_out" | tail -1)"; \
	fi; \
	case "$$base_image" in \
	  *@sha256:*) ;; \
	  *) echo "BASE_IMAGE resolved to \"$$base_image\", which is not a digest-pinned ref (@sha256:...)" >&2; exit 1 ;; \
	esac; \
	base_inputs_hash="$$(docker image inspect --format '{{ index .Config.Labels "buildgate.inputs-hash" }}' "$$base_image")"; \
	if [ -z "$$base_inputs_hash" ] || [ "$$base_inputs_hash" = "<no value>" ]; then \
	  base_inputs_hash="$$(docker image inspect --format '{{ index .Config.Labels "software-factory.inputs-hash" }}' "$$base_image")"; \
	fi; \
	if [ -z "$$base_inputs_hash" ] || [ "$$base_inputs_hash" = "<no value>" ]; then \
	  echo "BASE_IMAGE $$base_image has no buildgate.inputs-hash label -- it wasn't built by this Makefile's own sandbox-image target, so the pifork image's stamped hash could not be tied to it" >&2; exit 1; \
	fi; \
	inputs_hash="$$(go run ./cmd/factoryd image-inputs-hash -repo-root . -base-inputs-hash "$$base_inputs_hash" pifork)"; \
	if [ -z "$$inputs_hash" ]; then echo "image-inputs-hash failed or printed nothing for pifork" >&2; exit 1; fi; \
	docker build --build-arg BASE_IMAGE="$$base_image" $(PIFORK_BUILD_ARGS) \
	  --label buildgate.inputs-hash="$$inputs_hash" \
	  --label buildgate.image=pifork \
	  -f "$(PIFORK_DOCKERFILE)" -t localhost:5050/buildgate-pifork:local "$$context"; \
	docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges --network none --user 65532 \
	  --tmpfs /home/worker:rw,exec,nosuid,mode=1777,size=64m --env PI_CODING_AGENT_DIR=/home/worker/.pi/agent \
	  --entrypoint /bin/sh localhost:5050/buildgate-pifork:local -c 'pifork --version' >/dev/null \
	  || { echo "the built image does not run \`pifork --version\` as the worker (uid 65532, read-only root, no network): see the image contract in doc/designs/pifork-harness.md" >&2; exit 1; }; \
	push_output="$$(docker push localhost:5050/buildgate-pifork:local)"; \
	echo "$$push_output"; \
	digest="$$(printf '%s\n' "$$push_output" | grep -oE 'sha256:[0-9a-f]+' | tail -1)"; \
	if [ -z "$$digest" ]; then echo "could not read a digest from docker push output" >&2; exit 1; fi; \
	echo "localhost:5050/buildgate-pifork@$$digest"

# Builds buildgate's own meter image (internal/meter/Dockerfile) locally and
# pushes it through the local registry above, printing a real, digest-pinned
# reference usable with -meter-image. `make install`'s own local-images target
# calls this and records the printed ref via `factoryd configure-images`.
#
# Skips `docker build` (but still pushes) when `factoryd image-reuse`
# reports an unchanged inputs hash; see sandbox-image above.
# FORCE_IMAGE_BUILD=1 always rebuilds.
meter-image: .local-registry
	@set -e; \
	ca_bundle="$(BUILD_CA_BUNDLE)"; \
	if [ -n "$$ca_bundle" ] && [ ! -f "$$ca_bundle" ]; then echo "$$ca_bundle is not a readable CA bundle" >&2; exit 1; fi; \
	inputs_hash="$$(go run ./cmd/factoryd image-inputs-hash -repo-root . meter)"; \
	if [ -z "$$inputs_hash" ]; then echo "image-inputs-hash failed or printed nothing for meter" >&2; exit 1; fi; \
	if [ "$$(go run ./cmd/factoryd image-reuse -inputs-hash "$$inputs_hash" $(if $(filter 1,$(FORCE_IMAGE_BUILD)),-force) meter localhost:5050/factoryd-meter:local)" != reuse ]; then \
	  scripts/with-spinner.sh "building meter image" docker build $${ca_bundle:+--secret "id=meter-ca,src=$$ca_bundle"} -f internal/meter/Dockerfile \
	    --label buildgate.inputs-hash="$$inputs_hash" \
	    --label buildgate.image=meter \
	    -t localhost:5050/factoryd-meter:local .; \
	fi; \
	push_output="$$(docker push localhost:5050/factoryd-meter:local)"; \
	echo "$$push_output"; \
	digest="$$(printf '%s\n' "$$push_output" | grep -oE 'sha256:[0-9a-f]+' | tail -1)"; \
	if [ -z "$$digest" ]; then echo "could not read a digest from docker push output" >&2; exit 1; fi; \
	echo "localhost:5050/factoryd-meter@$$digest"

# Pulls the three OpenShell images buildgate pins by digest (the gateway, the
# sandbox runtime and the supervisor). These are the only images pulled rather
# than built from source; the digests match internal/hostcontrol's
# OpenShell*Image constants, which TestOpenShellPinnedImagesMatchTheEmbeddedFiles
# keeps in step with docker-compose.openshell.yml and openshell-gateway.toml.tmpl.
openshell-images:
	@if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then \
		echo "Docker is required to pull the OpenShell images -- install/start Docker and re-run 'make install'." >&2; exit 1; \
	fi
	docker pull ghcr.io/nvidia/openshell/gateway@sha256:2fe4dad9118e14ab80a8258b545ea6e6cd74c3469e24ad4e6610f964d98913a2
	docker pull ghcr.io/nvidia/openshell/sandbox@sha256:bf4797b6c511f2d8ba02955dbba4bf76c1f0dd6d83531420c5408d5f1fb9d72f
	docker pull ghcr.io/nvidia/openshell/supervisor@sha256:d7b5264bb6bc56f4796e6fa3617b8e4a8d785be0b7293542efd8cc250b0fb67a

# Builds the per-run read-only package-registry proxy image
# (internal/registryproxy/Dockerfile) locally and pushes it through the
# local registry above, printing a real, digest-pinned reference usable
# with -registry-proxy-image. This is the only way to get a registry-proxy
# image -- built from source, never published or pulled from a registry
# (see AGENTS.md). `make install`'s own local-images target calls this and
# records the printed ref via `factoryd configure-images`.
#
# Skips `docker build` (but still pushes) when `factoryd image-reuse`
# reports an unchanged inputs hash; see sandbox-image above.
# FORCE_IMAGE_BUILD=1 always rebuilds.
registry-proxy-image: .local-registry
	@set -e; \
	ca_bundle="$(BUILD_CA_BUNDLE)"; \
	if [ -n "$$ca_bundle" ] && [ ! -f "$$ca_bundle" ]; then echo "$$ca_bundle is not a readable CA bundle" >&2; exit 1; fi; \
	inputs_hash="$$(go run ./cmd/factoryd image-inputs-hash -repo-root . registry-proxy)"; \
	if [ -z "$$inputs_hash" ]; then echo "image-inputs-hash failed or printed nothing for registry-proxy" >&2; exit 1; fi; \
	if [ "$$(go run ./cmd/factoryd image-reuse -inputs-hash "$$inputs_hash" $(if $(filter 1,$(FORCE_IMAGE_BUILD)),-force) registry-proxy localhost:5050/factoryd-registry-proxy:local)" != reuse ]; then \
	  scripts/with-spinner.sh "building registry-proxy image" docker build $${ca_bundle:+--secret "id=build-ca,src=$$ca_bundle"} -f internal/registryproxy/Dockerfile \
	    --label buildgate.inputs-hash="$$inputs_hash" \
	    --label buildgate.image=registry-proxy \
	    -t localhost:5050/factoryd-registry-proxy:local .; \
	fi; \
	push_output="$$(docker push localhost:5050/factoryd-registry-proxy:local)"; \
	echo "$$push_output"; \
	digest="$$(printf '%s\n' "$$push_output" | grep -oE 'sha256:[0-9a-f]+' | tail -1)"; \
	if [ -z "$$digest" ]; then echo "could not read a digest from docker push output" >&2; exit 1; fi; \
	echo "localhost:5050/factoryd-registry-proxy@$$digest"

# Builds a project-specific sandbox image (internal/sandbox/Dockerfile.project)
# by layering PROJECT_DIR's own Go/npm dependency manifests onto BASE_IMAGE
# (required -- the digest-pinned worker ref `make sandbox-image` prints, or
# another project image) -- see that Dockerfile's own doc comment for why
# and when you need this instead of the plain worker image (a target
# project needing a dependency the worker image doesn't already bake
# cannot resolve it inside a sandboxed run, which has no package-registry
# network at all). Prints a real, digest-pinned reference usable with
# -sandbox-image, via the same local registry as sandbox-image above.
#
# Stages only the known manifest files into a clean, throwaway build
# context -- never PROJECT_DIR itself -- before invoking `docker build`
# (found via a real GitHub Codex App review of PR #51): PROJECT_DIR is a
# real project checkout, so it can contain `.git`, `.env`, credentials, or
# anything else gitignored, and a straight `docker build ... PROJECT_DIR`
# would hand the whole tree to Docker as build context, reachable by
# `COPY .` inside the Dockerfile regardless of what that Dockerfile
# actually declares it copies -- a later layer removing a copied secret
# does not remove it from the image's own history, and this target then
# pushes the result to a registry. Relying on Dockerfile.project's own
# COPY list (or a .dockerignore) to filter PROJECT_DIR itself would still
# leave one missed entry away from the same leak; building from a context
# that structurally contains nothing else does not.
#
# requirements.txt gets an extra pass beyond the plain manifest-file copy
# every other ecosystem above gets (found via a real GitHub Codex App
# review of PR #68): a real Python project's requirements.txt commonly
# references other files via `-r requirements/base.txt` or
# `-c constraints.txt` rather than declaring every dependency inline, and
# staging only the top-level file left `pip install -r requirements.txt`
# unable to find them, silently breaking this support for exactly the
# layout most real projects use. Resolved relative to the referencing
# file's own directory (matching pip's own resolution rule, not this
# recipe's invocation cwd) and followed transitively -- a staged file can
# itself reference further ones, and not necessarily under a `.txt` name:
# pip's own -r/-c syntax imposes no required extension (a pip-compile
# project's requirements.in -> requirements/base.in chain is exactly as
# common as an all-.txt one), so every pass re-scans every staged file,
# not just ones already named `*.txt` -- an earlier version of this
# recipe restricted the re-scan that way and silently missed a
# non-`.txt`-named file's own further references (found via a real
# GitHub Codex App review of PR #69). A backslash-continued directive
# (pip's own line-continuation syntax, e.g. `-r \` on one line and the
# actual path on the next) is joined before extraction, not left for two
# separate lines neither of which looks like a real -r/-c directive on
# its own (found via a real GitHub Codex App review of PR #69) -- joined
# with no inserted separator, matching pip's own plain-concatenation
# join exactly (found via a second real GitHub Codex App review pass on
# the same PR: the first version of this join always inserted a space,
# which is exactly right for the common, visually-indented continuation
# style, `-r \` then an indented path on the next line, since the
# continuation's own leading whitespace already separates them either
# way -- but wrong for a continuation splitting a single reference
# mid-word, e.g. `-r requirements/ba\` then `se.txt`, where an inserted
# space corrupts the real, single filename into two words instead of
# splicing it back into one). A value wrapped in a matching pair of
# `"`/`'` quote characters (pip's own req-file parser accepts either, per
# a real GitHub Codex App review comment on the same pass) has them
# stripped after extraction -- this still can't recover a quoted value
# containing an embedded space (the whitespace-delimited extraction
# above stops at the first one regardless of quoting, a real but
# considerably narrower gap than the two this closes), so a requirements
# file relying on that specific combination is still not supported. The
# short flags (-r/-c, never the long --requirement/--constraint forms)
# also match with no separator at all before the value, e.g.
# `-rrequirements/base.txt` -- pip's own option parser (an argparse/
# optparse-style single-character-flag convention, not unique to pip)
# accepts an attached value there exactly the same as a space or "="
# separated one (found via a real GitHub Codex App review of PR #69).
# Bounded
# to 50 passes, loudly (not silently) failing the whole build if the
# chain is still growing when that bound is hit, rather than truncating
# a real, valid deep chain and leaving `pip install` to fail downstream
# with no clue why (found via a real GitHub Codex App review of PR #69:
# each pass only discovers files staged in an *earlier* pass, so a valid
# chain needing more passes than a hard cap allows -- unlikely, but
# possible -- must say so, not go quiet).
#
# An absolute-path reference is refused outright. A reference containing
# a ".." component is only refused if it actually resolves outside
# PROJECT_DIR once normalized (lexically, without touching the
# filesystem -- collapsing e.g. "requirements/../constraints.txt" to
# "constraints.txt" the same way pip itself does) -- not by the mere
# presence of "..", which an earlier version of this recipe rejected
# unconditionally and in doing so broke the common, legitimate pattern
# of a nested requirements file sharing a top-level constraints file via
# `-c ../constraints.txt` (found via a real GitHub Codex App review of
# PR #69). Escaping PROJECT_DIR this way is exactly the leak the
# paragraph above already exists to prevent for PROJECT_DIR's own raw
# contents, reopened here through the manifest file's own content
# instead of PROJECT_DIR's file tree directly (found via this recipe's
# own live adversarial testing, not an external review, for the
# original unconditional-rejection version of this check).
#
# Normalizing the reference's own text is still not enough on its own
# (found via a real GitHub Codex App review of PR #69, P1): a reference
# whose own (even normalized) text never leaves PROJECT_DIR can still
# resolve outside it if any path component along the way is a symlink
# planted somewhere under PROJECT_DIR pointing elsewhere -- `[ -f ... ]`
# and `cp` both follow symlinks transparently, live-confirmed to copy a
# real file from outside PROJECT_DIR into the staged image content this
# way. Closed by walking every path component of the *normalized* path
# from PROJECT_DIR down to the referenced file (not just the final
# component, and not the raw pre-normalization text, which could still
# contain a ".." a naive walk would mishandle) and refusing the whole
# build if any of them is a symlink, live-confirmed against both a
# symlinked file and a symlinked intermediate directory.
#   make project-sandbox-image PROJECT_DIR=~/code/calc-app BASE_IMAGE=<worker ref from `make sandbox-image`>
#
# Stamped buildgate.image=project, overriding the buildgate.image
# label BASE_IMAGE's own image carries (Docker inherits labels through
# FROM) -- factoryd's staleness check reads this label to know what kind
# of image it's looking at, and must never treat a project image as a
# plain worker: it would otherwise be hashed against this repo's own
# go.sum/Dockerfile, always show "stale", and get silently rebuilt as a
# plain worker by a staleness-triggered `make local-images`, discarding
# the project's own baked dependencies. No buildgate.inputs-hash
# label: a project image's real inputs are PROJECT_DIR's own manifests,
# outside this repo, so there is nothing here to hash it against --
# staleness is reported as unverifiable for every project image instead.
project-sandbox-image: .local-registry
	@if [ -z "$(PROJECT_DIR)" ] || [ -z "$(BASE_IMAGE)" ]; then \
	  echo "usage: make project-sandbox-image PROJECT_DIR=<path> BASE_IMAGE=<digest-pinned worker ref, e.g. from make sandbox-image>" >&2; exit 1; \
	fi
	@stage="$$(mktemp -d)"; \
	trap 'rm -rf "$$stage"' EXIT; \
	set -e; \
	if [ -f "$(PROJECT_DIR)/package.json" ] && [ ! -f "$(PROJECT_DIR)/package-lock.json" ]; then \
	  echo "$(PROJECT_DIR) has package.json but no package-lock.json -- generate and commit the lockfile before baking an offline sandbox image" >&2; exit 1; \
	fi; \
	copied=0; \
	for f in go.mod go.sum package.json package-lock.json requirements.txt; do \
	  if [ -f "$(PROJECT_DIR)/$$f" ]; then cp "$(PROJECT_DIR)/$$f" "$$stage/$$f"; copied=1; fi; \
	done; \
	if [ "$$copied" = 0 ]; then \
	  echo "no supported manifest file (go.mod/go.sum/package.json/package-lock.json/requirements.txt) found under $(PROJECT_DIR) -- nothing to bake" >&2; exit 1; \
	fi; \
	if [ -f "$$stage/requirements.txt" ]; then \
	  pass=0; \
	  while [ "$$pass" -lt 50 ]; do \
	    pass=$$((pass + 1)); \
	    added=0; \
	    for reqfile in $$(find "$$stage" -type f); do \
	      reldir="$$(dirname "$$reqfile" | sed "s|^$$stage||")"; \
	      refs="$$(awk '{ if (sub(/\\[ \t]*$$/, "")) { printf "%s", $$0; next } print }' "$$reqfile" 2>/dev/null | grep -oE '^[[:space:]]*(-r|--requirement|-c|--constraint)([[:space:]]+|=)[^[:space:]]+|^[[:space:]]*(-r|-c)[^[:space:]=][^[:space:]]*' | sed -E 's/^[[:space:]]*(-r|--requirement|-c|--constraint)([[:space:]]+|=)?//')"; \
	      for ref in $$refs; do \
	        case "$$ref" in \
	          \"*\") ref="$${ref#\"}"; ref="$${ref%\"}" ;; \
	          \'*\') ref="$${ref#\'}"; ref="$${ref%\'}" ;; \
	        esac; \
	        case "$$ref" in \
	          /*) \
	            echo "requirements file $$reldir references \"$$ref\", an absolute path -- refusing to stage it" >&2; exit 1 ;; \
	        esac; \
	        raw="$$(printf '%s/%s' "$$reldir" "$$ref" | sed 's|^/||')"; \
	        normalized=""; escaped=0; oldifs="$$IFS"; IFS='/'; \
	        for comp in $$raw; do \
	          case "$$comp" in \
	            ''|.) : ;; \
	            ..) \
	              if [ -z "$$normalized" ]; then escaped=1; else \
	                case "$$normalized" in \
	                  */*) normalized="$${normalized%/*}" ;; \
	                  *) normalized="" ;; \
	                esac; \
	              fi ;; \
	            *) \
	              if [ -z "$$normalized" ]; then normalized="$$comp"; else normalized="$$normalized/$$comp"; fi ;; \
	          esac; \
	        done; \
	        IFS="$$oldifs"; \
	        if [ "$$escaped" = 1 ]; then \
	          echo "requirements file $$reldir references \"$$ref\", which escapes $(PROJECT_DIR) -- refusing to stage it" >&2; exit 1; \
	        fi; \
	        cur="$(PROJECT_DIR)"; symlinked=0; oldifs="$$IFS"; IFS='/'; \
	        for comp in $$normalized; do \
	          cur="$$cur/$$comp"; \
	          if [ -L "$$cur" ]; then symlinked=1; fi; \
	        done; \
	        IFS="$$oldifs"; \
	        if [ "$$symlinked" = 1 ]; then \
	          echo "requirements file $$reldir references \"$$ref\", which resolves through a symlink somewhere under $(PROJECT_DIR) -- refusing to stage it (a symlink could point outside $(PROJECT_DIR) even though the reference text itself looks safe)" >&2; exit 1; \
	        fi; \
	        src="$(PROJECT_DIR)/$$normalized"; \
	        dst="$$stage/$$normalized"; \
	        if [ -f "$$src" ] && [ ! -f "$$dst" ]; then \
	          mkdir -p "$$(dirname "$$dst")"; cp "$$src" "$$dst"; added=1; \
	          echo "staged referenced requirements file: $$normalized"; \
	        fi; \
	      done; \
	    done; \
	    if [ "$$added" = 0 ]; then break; fi; \
	  done; \
	  if [ "$$pass" -ge 50 ] && [ "$$added" != 0 ]; then \
	    echo "requirements include chain is still growing after 50 passes -- a cyclic reference, or a real chain deeper than this recipe supports" >&2; exit 1; \
	  fi; \
	fi; \
	ca_bundle="$(BUILD_CA_BUNDLE)"; \
	if [ -n "$$ca_bundle" ] && [ ! -f "$$ca_bundle" ]; then echo "$$ca_bundle is not a readable CA bundle" >&2; exit 1; fi; \
	docker build $${ca_bundle:+--secret "id=build-ca,src=$$ca_bundle"} -f internal/sandbox/Dockerfile.project \
	  --build-arg BASE_IMAGE=$(BASE_IMAGE) \
	  --label buildgate.image=project \
	  -t localhost:5050/project-worker:local "$$stage"; \
	push_output="$$(docker push localhost:5050/project-worker:local)"; \
	echo "$$push_output"; \
	digest="$$(printf '%s\n' "$$push_output" | grep -oE 'sha256:[0-9a-f]+' | tail -1)"; \
	if [ -z "$$digest" ]; then echo "could not read a digest from docker push output" >&2; exit 1; fi; \
	echo "localhost:5050/project-worker@$$digest"
