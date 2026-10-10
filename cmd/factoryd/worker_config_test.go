package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/meter"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/testfixture"
)

// TestBuildTicketRunArgsIsAcceptedByRunMainWithReadysOwnFlagSet closes the
// gap every other buildTicketRunArgs test shares: they assert on the argv
// as strings, and the worker's tests inject a stub ticketRunner, so
// nothing ever fed a real buildTicketRunArgs result to the flag.FlagSet
// that actually has to parse it. A flag renamed, removed, or given a
// different arity in run_ticket.go would leave every one of those tests
// green while every queued entry failed at parse time -- which is
// exactly the class of break PR #89's review had to find by hand.
//
// Every field of workerConfig and every optional QueueEntry field is
// set, so the argv exercises every branch buildTicketRunArgs has. The
// entry's own required identifiers are deliberately left empty: that
// makes runMainWithReady's very first post-parse check ("-ticket,
// -workspace, and -spec are required") the point it stops, so this test
// proves the whole argv parsed without running git, Docker, or a model.
// Any parse failure surfaces instead as flag's own "flag provided but
// not defined" error, which is what this asserts against.
func TestBuildTicketRunArgsIsAcceptedByRunMainWithReadysOwnFlagSet(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n"+
		"models:\n  m:\n    id: some-model\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")
	cfg := requestdriver.WorkerConfig{
		OpenPullRequest:    true,
		BuildAppScript:     "/harness/build_app.py",
		SandboxImage:       "registry.example/org/img@sha256:deadbeef",
		RegistryProxy:      true,
		RegistryProxyImage: "registry.example/org/rp@sha256:deadbeef",
	}
	// Empty ID/Workspace/SpecPath, but Project, PreflightProfile and
	// IssueRef set, so -project, -preflight-profile and -pr-closes-issue
	// are all in the argv.
	entry := &requestdriver.QueueEntry{Project: "payments", PreflightProfile: "brownfield", IssueRef: "acme/widgets#42", SpecAcceptanceCriteria: "/repo/tickets/001/criteria.md"}

	err := runMainWithReady(dp, context.Background(), requestdriver.BuildTicketRunArgs("data", entry, cfg), nil)
	if err == nil {
		t.Fatal("runMainWithReady(buildTicketRunArgs(...)) = nil, want the required-flags error it stops at")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("runMainWithReady rejected an argv buildTicketRunArgs produced: %v", err)
	}
	if !strings.Contains(err.Error(), "-ticket, -workspace, and -spec are required") {
		t.Fatalf("err = %v, want the required-flags error, meaning every other flag parsed", err)
	}
}

// TestWorkerConfigRejectsRegistryProxyWithoutSandboxImage proves the
// session-wide misconfiguration is caught once at startup rather than
// failing every drained entry one at a time inside runMainWithReady --
// run_ticket.go requires -sandbox-image for -registry-proxy just as much
// as -registry-proxy-image (found missing from this check via a local
// ai-stack code-review pass on PR #93: without it, this exact combination
// passed worker's own startup check cleanly and only failed deep
// inside run_ticket.go, once per drained entry).
func TestWorkerConfigRejectsRegistryProxyWithoutSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-registry-proxy", "-registry-proxy-image", "registry.example/org/rp@sha256:deadbeef"})
	if err == nil {
		t.Fatal("loadTestWorkerConfig(-registry-proxy without -sandbox-image) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "-registry-proxy requires -sandbox-image") {
		t.Fatalf("err = %v, want it to name the missing -sandbox-image", err)
	}
}

func TestWorkerConfigRejectsResponsesWithoutWorkerModelWhenDoctorSkipped(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n"+
		"models:\n  m:\n    api: "+meter.RequestFormatOpenAIResponses+"\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-skip-doctor",
		"-max-review-rounds", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "models.m: id is required") {
		t.Fatalf("workerMain with Responses and no worker model = %v, want the pairing error before doctor or queue drain", err)
	}
}

// TestWorkerConfigHasNoEngineFlag pins that the per-run -engine flag is
// gone: the harness is chosen per role (roles.<role>.harness), so the old
// flag is refused like any other unknown one.
func TestWorkerConfigHasNoEngineFlag(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-skip-doctor", "-engine", "pifork", "-max-review-rounds", "0"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -engine") {
		t.Fatalf("workerMain -engine = %v, want the unknown-flag refusal", err)
	}
}

// TestWorkerConfigRequiresSandboxImageForARoleHarnessThatNeedsOne: a session
// whose execution role runs pifork without an explicit sandbox_image is
// refused at startup, naming the role.
func TestWorkerConfigRequiresSandboxImageForARoleHarnessThatNeedsOne(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "roles:\n  execution:\n    model: m\n    harness: pifork\nmodels:\n  m:\n    id: gpt-x\n    routes: [r]\nroutes:\n  r:\n    credential_mode: static\n    upstream: https://r.example.invalid\n    credential_env: QR_TEST_KEY\n")
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-skip-doctor", "-max-review-rounds", "0"})
	if err == nil || !strings.Contains(err.Error(), "roles.execution: harness pifork requires an explicit digest-pinned sandbox_image") {
		t.Fatalf("workerMain = %v, want the role-naming sandbox image refusal", err)
	}
}

// TestWorkerConfigAcceptsPlanTicketsFlags covers the "any new argv is
// parsed through the real flag set" requirement for the two new
// -plan-tickets-script/-plan-tickets-timeout-minutes worker flags:
// passing them must reach past flags.Parse to the
// -registry-proxy/-sandbox-image refusal (the same fast-fail probe
// TestWorkerConfigRejectsRegistryProxyWithoutSandboxImage uses), not stop
// at "flag provided but not defined".
func TestWorkerConfigAcceptsPlanTicketsFlags(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-plan-tickets-script", "/harness/plan_tickets.py",
		"-plan-tickets-timeout-minutes", "20",
		"-registry-proxy", "-registry-proxy-image", "registry.example/org/rp@sha256:deadbeef",
	})
	if err == nil {
		t.Fatal("loadTestWorkerConfig(-plan-tickets-script/-plan-tickets-timeout-minutes, -registry-proxy without -sandbox-image) = nil, want an error")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("workerMain rejected a real flag: %v", err)
	}
	if !strings.Contains(err.Error(), "-registry-proxy requires -sandbox-image") {
		t.Fatalf("err = %v, want it to name the missing -sandbox-image (proving flag parsing got past -plan-tickets-*)", err)
	}
}

// TestWorkerConfigRejectsMalformedModelExtraJSON: models.<name>.extra_json
// is Model's own map[string]any field (internal/sessionconfig/routing.go)
// -- a value that isn't a YAML/JSON object is rejected by
// sessionconfig.Load's own strict decode as a session-wide
// misconfiguration, so it fails once at startup, not once per drained
// entry when the worker launches.
func TestWorkerConfigRejectsMalformedModelExtraJSON(t *testing.T) {
	dp := newTestDeps(t)
	for _, bad := range []string{"not json", "[1]"} {
		path := isolateSessionConfig(t)
		writeSessionConfig(t, path, "models:\n  m:\n    extra_json: "+bad+"\n")
		_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-sandbox-image", "img@sha256:aaaa"})
		if err == nil {
			t.Fatalf("loadTestWorkerConfig(models.m.extra_json: %s) = nil, want an error", bad)
		}
		if !strings.Contains(err.Error(), "map[string]interface {}") {
			t.Fatalf("err = %v, want it to name the malformed models.m.extra_json field", err)
		}
	}
}

func containsFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func containsArg(args []string, name, value string) bool {
	for i, a := range args {
		if a == name && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// isolateSessionConfig points the default session-config lookup at an
// empty HOME so a developer's real ~/.config/factoryd/config.yml cannot
// leak into a test, and returns the path init-config would write.
func isolateSessionConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	return sessionconfig.DefaultPaths()[0]
}

// loadTestWorkerConfig is loadWorkerConfig with a dummy Temporal address
// when args name none: tests keep autostart off, under which the worker
// requires one.
func loadTestWorkerConfig(dp *deps, args []string) (requestdriver.WorkerConfig, string, error) {
	if !slices.Contains(args, "-temporal-address") {
		args = append(slices.Clone(args), "-temporal-address", "127.0.0.1:1")
	}
	return loadWorkerConfig(dp, args)
}

// stubWorkerDoctorChecks replaces the docker.workerChecks seam for one
// test, recording the inputs it was called with and returning checks.
func stubWorkerDoctorChecks(dp *deps, t *testing.T, checks []doctorCheck) (got **doctorInputs) {
	t.Helper()
	got = new(*doctorInputs)
	prev := fakeDockerOf(dp).workerChecksFn
	fakeDockerOf(dp).workerChecksFn = func(_ context.Context, in doctorInputs) []doctorCheck {
		*got = &in
		return checks
	}
	t.Cleanup(func() { fakeDockerOf(dp).workerChecksFn = prev })
	return got
}

func writeSessionConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerConfigRefusesWithoutConfigFileOrFlags(t *testing.T) {
	dp := newTestDeps(t)
	defaultPath := isolateSessionConfig(t)
	// -open-pull-request and -build-app-script configure what happens
	// around a run, not how it executes, so alone they must not count as
	// "configured" (PR #101 review): every drained entry would otherwise
	// fail on the missing sandbox/relay values.
	for _, args := range [][]string{{}, {"-data-dir", t.TempDir()}, {"-open-pull-request=false"}, {"-build-app-script", "/x/build_app.py"}} {
		_, _, err := loadTestWorkerConfig(dp, args)
		if err == nil {
			t.Fatalf("loadTestWorkerConfig(%v) with no config = nil, want a refusal", args)
		}
		for _, want := range []string{defaultPath, "factoryd init-config", "sandbox_image:"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("loadTestWorkerConfig(%v) err = %q, want it to mention %q", args, err, want)
			}
		}
	}
}

// TestWorkerConfigRefusesForbiddenAPITokenInEnvironment: worker must
// refuse to start at all when a FACTORYD_API_* control-plane token is
// set in its own environment, the same refuseAPITokensInEnvironment
// guard realMain and daemonMain already apply -- not discover it only
// once the first ticket reaches build, 17 minutes into a live run (see
// TestRefuseAPITokensInEnvironment for the guard's own unit test).
func TestWorkerConfigRefusesForbiddenAPITokenInEnvironment(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	t.Setenv(overrideTokenEnvironmentVariable, "")
	t.Setenv(readTokenEnvironmentVariable, "")
	t.Setenv(startTokenEnvironmentVariable, "secret")
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), startTokenEnvironmentVariable) {
		t.Fatalf("loadTestWorkerConfig() with %s set = %v, want a refusal naming it", startTokenEnvironmentVariable, err)
	}
}

// TestWorkerConfigHonorsExplicitRegistryProxyOptOut is the regression test
// for the second half of a Codex review finding on PR #129, P2:
// -registry-proxy now defaults on for the default, model-backed
// build_app.py (both here and in run_ticket.go), but an operator's own
// explicit -registry-proxy=false must still be honored end to end --
// worker's own mandatory doctor preflight must not check an image the
// operator explicitly opted out of, matching what
// TestBuildTicketRunArgsThreadsRegistryProxyConfig already proves at the
// buildTicketRunArgs level alone (that the opt-out survives into the argv
// a drained entry's child process receives).
func TestWorkerConfigHonorsExplicitRegistryProxyOptOut(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	got := stubWorkerDoctorChecks(dp, t, []doctorCheck{{Name: "relay upstream", Err: errors.New("404")}})
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-registry-proxy=false"})
	if err == nil || !strings.Contains(err.Error(), "refusing to drain: 1 doctor check(s) failed") {
		t.Fatalf("workerMain = %v, want the doctor refusal", err)
	}
	if *got == nil {
		t.Fatal("doctor seam was not invoked")
	}
	if (**got).registryProxyImage != "" {
		t.Fatalf("registryProxyImage = %q, want empty: an explicit -registry-proxy=false must not be silently turned back on by the default-on resolution", (**got).registryProxyImage)
	}
}

func TestWorkerConfigRejectsUnknownConfigKey(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "relay_imaeg: x\n")
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "relay_imaeg") {
		t.Fatalf("workerMain with an unknown config key = %v, want an error naming relay_imaeg", err)
	}
}

// TestWorkerConfigConfigFileFillsUnsetFlagsAndDoctorFailureRefusesToDrain
// proves the precedence (explicit flag > config file > flag default) by
// inspecting exactly what reaches the doctor seam, and that a failing
// check stops worker before it ever takes the drain lock.
// sandbox_tmpfs_size is its own regression test for a Codex review
// finding on this same PR (runWorkerDoctorPreflight's own doctorInputs
// left it empty even though it is daemon-wide, resolved via cfg.settings
// exactly like sandboxDocker just above it -- doctorChecksFor skips an
// empty value's check entirely, so worker could report a clean
// preflight and start draining with a configured tmpfs size too small
// for a real build).
func TestWorkerConfigConfigFileFillsUnsetFlagsAndDoctorFailureRefusesToDrain(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, `sandbox_image: from-config@sha256:aaaa
sandbox_tmpfs_size: 2g
`)
	got := stubWorkerDoctorChecks(dp, t, []doctorCheck{{Name: "relay upstream", Err: errors.New("404")}})
	// Not t.TempDir() itself (always pre-created) -- a not-yet-existing
	// child of it, so the assertion below can actually distinguish
	// "left uncreated" from "happened to already exist".
	dataDir := filepath.Join(t.TempDir(), "fresh-data-dir")
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", dataDir, "-sandbox-image", "from-flag@sha256:cccc"})
	if err == nil || !strings.Contains(err.Error(), "refusing to drain: 1 doctor check(s) failed") {
		t.Fatalf("workerMain = %v, want the doctor refusal", err)
	}
	if *got == nil {
		t.Fatal("doctor seam was not invoked")
	}
	// doctorInputs.settings mirrors cfg.settings exactly (see
	// runWorkerDoctorPreflight's own doctorInputs construction) --
	// computed here the same way (Load + ApplySettings against the same
	// config file) rather than hand-duplicated, so this assertion can't
	// drift from what that function actually does.
	loadedCfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantSettings, err := loadedCfg.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	want := doctorInputs{
		sandboxDocker:    "docker",
		sandboxTmpfsSize: "2g",
		sandboxImage:     "from-flag@sha256:cccc",
		settings:         wantSettings,
		// registryProxyImage is empty here: -registry-proxy-image has no
		// built-in default and this invocation names neither it nor a
		// session-config registry_proxy_image, even though -registry-proxy
		// itself still defaults on for the default, model-backed
		// build_app.py this bare invocation implies (found via Codex
		// review of PR #129, P2 -- this test's own want literal used to
		// omit registryProxyImage's presence entirely, asserting the stale
		// "off by default" behavior that finding flagged).
		registryProxyImage: "",
		// relayUpstream/relayWorkerModelID/etc all stay zero here: routes:/
		// models:/roles: is the only session-config schema now, and this
		// preflight's own doctorInputs construction never sets any of
		// those fields -- only doctorListModels populates them, from a
		// real modelrole.SelectRoute resolution, which the stubbed doctor
		// seam here never runs.
	}
	// reflect.DeepEqual, not !=: doctorInputs.settings embeds
	// sessionconfig.Settings, which holds map fields (Routes, Models),
	// making the struct no longer comparable with ==/!=.
	if !reflect.DeepEqual(**got, want) {
		t.Fatalf("doctor inputs = %+v, want %+v", **got, want)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "queue", hostcontrol.WorkerLockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker took the drain lock despite a failed doctor check (stat err = %v)", err)
	}
	// -data-dir must not be created by this preflight at all here: the
	// stubbed check that failed ("relay upstream") is unrelated to
	// -data-dir, so the -data-dir mount probe (which needs -data-dir to
	// already exist) never runs, and runWorkerDoctorPreflight must not
	// MkdirAll it unconditionally up front either -- see that function's
	// own doc comment (found via adversarial review of this same PR).
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: err = %v, want -data-dir left uncreated: the failing check here is unrelated to it", dataDir, err)
	}
}

// TestRunWorkerDoctorPreflightForwardsReleasePolicy is the regression
// test for the worker half of a doctor preflight bug:
// runWorkerDoctorPreflight's own doctorInputs left
// releaseMaxFilesChanged/releaseMaxInsertions/releaseRollbackPlan at
// their zero value regardless of cfg.settings' own
// resolved release policy, so doctorCheckReleasePolicy (always appended by
// doctorChecksFor) reported "denies every PR unconditionally" even when
// the session config actually in use already had usable release_* keys --
// the same false-alarm class as quickstartPullDoctorInputs' own version of
// this bug.
func TestRunWorkerDoctorPreflightForwardsReleasePolicy(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, `sandbox_image: from-config@sha256:aaaa
release_max_files_changed: 10
release_max_insertions: 500
release_rollback_plan: "git revert"
`)
	got := stubWorkerDoctorChecks(dp, t, []doctorCheck{{Name: "relay upstream", Err: errors.New("404")}})
	dataDir := filepath.Join(t.TempDir(), "fresh-data-dir")
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", dataDir})
	if err == nil || !strings.Contains(err.Error(), "refusing to drain: 1 doctor check(s) failed") {
		t.Fatalf("workerMain = %v, want the doctor refusal", err)
	}
	if *got == nil {
		t.Fatal("doctor seam was not invoked")
	}
	in := **got
	if in.releaseMaxFilesChanged != 10 || in.releaseMaxInsertions != 500 || in.releaseRollbackPlan != "git revert" {
		t.Errorf("release policy forwarded to the doctor seam = (%d, %d, %q), want (10, 500, \"git revert\") from the session config -- a zero-value release policy here makes doctorCheckReleasePolicy always warn regardless of what the config actually says", in.releaseMaxFilesChanged, in.releaseMaxInsertions, in.releaseRollbackPlan)
	}
}

// TestWorkerConfigRefusesWhenDataDirIsNotMountVisible is the regression
// test for a bug where worker's doctor preflight never checked
// -data-dir's own colima mount visibility at all, so a -data-dir the
// configured Docker backend doesn't actually share into its containers
// only surfaced minutes into
// a drained entry's real sandboxed build. Every other check is stubbed
// to pass cleanly (docker.workerChecks), isolating this test to the
// -data-dir mount probe itself -- a second phase runWorkerDoctorPreflight
// runs for real even with that seam stubbed, deliberately not folded
// into doctorInputs (see that function's own doc comment on why: it
// needs to know every other check already passed before creating
// -data-dir to test it). -sandbox-docker is pointed at a nonexistent
// binary via session config (no CLI flag exists for it) so the mount
// probe itself fails deterministically, the same
// "factoryd-doctor-test-nonexistent-binary" fixture pattern
// doctorCheckMountVisibility's own existing unit tests use.
func TestWorkerConfigRefusesWhenDataDirIsNotMountVisible(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_docker: factoryd-doctor-test-nonexistent-binary\n")
	stubWorkerDoctorChecks(dp, t, nil)
	dataDir := filepath.Join(t.TempDir(), "fresh-data-dir")
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", dataDir})
	if err == nil || !strings.Contains(err.Error(), "refusing to drain: 1 doctor check(s) failed") {
		t.Fatalf("workerMain = %v, want a doctor refusal naming exactly 1 failed check", err)
	}
	// -data-dir itself must exist by now: it's the one check that needed
	// it created to run the mount probe, and every other (stubbed) check
	// passed -- see runWorkerDoctorPreflight's own doc comment.
	if _, statErr := os.Stat(dataDir); statErr != nil {
		t.Fatalf("stat %s: %v, want -data-dir created ahead of the mount probe", dataDir, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "queue", hostcontrol.WorkerLockFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worker took the drain lock despite a failed doctor check (stat err = %v)", statErr)
	}
}

// TestWorkerConfigSkipDoctorSkipsTheDoctorChecks proves -skip-doctor loads
// the config without ever calling the doctor seam.
func TestWorkerConfigSkipDoctorSkipsTheDoctorChecks(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: img@sha256:aaaa\n")
	got := stubWorkerDoctorChecks(dp, t, []doctorCheck{{Name: "docker", Err: errors.New("down")}})
	if _, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-skip-doctor"}); err != nil {
		t.Fatalf("loadTestWorkerConfig(-skip-doctor) = %v, want nil", err)
	}
	if *got != nil {
		t.Fatal("-skip-doctor still invoked the doctor checks")
	}
}

// TestLoadWorkerConfigResolvesTheTemporalAddress: worker settles its
// Temporal address once (auto-started when none is given) and every drained
// entry's build is given exactly that address.
func TestLoadWorkerConfigResolvesTheTemporalAddress(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: img@sha256:aaaa\n")
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	old := fakeTemporalOf(dp).ensureFn
	t.Cleanup(func() { fakeTemporalOf(dp).ensureFn = old })
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "127.0.0.1:1234" }

	cfg, _, err := loadWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-skip-doctor"})
	if err != nil {
		t.Fatalf("loadWorkerConfig: %v", err)
	}
	if cfg.TemporalAddress != "127.0.0.1:1234" {
		t.Fatalf("cfg.temporalAddress = %q, want the auto-started address", cfg.TemporalAddress)
	}
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	if args := requestdriver.BuildTicketRunArgs("data", entry, cfg); !containsArg(args, "-temporal-address", "127.0.0.1:1234") {
		t.Errorf("args = %v, want -temporal-address 127.0.0.1:1234", args)
	}
}

func TestLoadWorkerConfigRefusesNoTemporalAddressWithAutostartOff(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: img@sha256:aaaa\n")
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	_, _, err := loadWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-skip-doctor"})
	want := "no Temporal address: pass -temporal-address (FACTORYD_AUTOSTART=0 starts nothing)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// TestApplySessionConfigRefusesInvalidRolesBlock proves worker's own
// startup path validates roles: once, explicitly (applySessionConfig
// calls validateRoles right after ApplySettings) -- an unknown
// model_aliases entry named by a role must refuse the whole invocation,
// naming the offending role, exactly like any other structurally invalid
// session config.
func TestApplySessionConfigRefusesInvalidRolesBlock(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, `roles:
  execution:
    model: does-not-exist
`)
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("relay-allowed-path-prefix", "/v1/messages", "")
	fs.String("relay-credential-header", meter.CredentialHeaderXAPIKey, "")
	_, _, err := applySessionConfig(fs, "")
	if err == nil {
		t.Fatal("applySessionConfig: want an error for the invalid roles: block")
	}
	if !strings.Contains(err.Error(), "roles.execution") {
		t.Errorf("applySessionConfig error = %v, want it to name roles.execution", err)
	}
}

// TestWorkerConfigParsesPRPollIntervalAndIgnoreAuthorsFlags proves
// -pr-poll-interval/-pr-ignore-authors parse cleanly through worker's
// own real flag set and validation (the "argv for a new flag is parsed
// through the real flag set" rule every WP's ground rules require),
// reaching the worker -- asserted the same way
// TestWorkerConfigSkipDoctorGoesStraightToDraining already proves a
// flag set was fully accepted: pre-acquiring the drain lock turns
// "the worker was reached" into an assertable error instead of a hang.
func TestWorkerConfigParsesPRPollIntervalAndIgnoreAuthorsFlags(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	dataDir := t.TempDir()

	cfg, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", dataDir,
		"-skip-doctor",
		"-sandbox-image", "img@sha256:aaaa",
		"-pr-poll-interval", "10m",
		"-pr-ignore-authors", "octobot, release-bot ,octobot",
		"-pr-trusted-authors", "alice, bob ,alice",
	})
	if err != nil {
		t.Fatalf("loadWorkerConfig = %v, want the flags accepted", err)
	}
	if cfg.PrPollInterval != 10*time.Minute {
		t.Errorf("prPollInterval = %v, want 10m", cfg.PrPollInterval)
	}
	if want := []string{"octobot", "release-bot", "octobot"}; !reflect.DeepEqual(cfg.PrIgnoreAuthors, want) {
		t.Errorf("prIgnoreAuthors = %v, want %v", cfg.PrIgnoreAuthors, want)
	}
	if want := []string{"alice", "bob", "alice"}; !reflect.DeepEqual(cfg.PrTrustedAuthors, want) {
		t.Errorf("prTrustedAuthors = %v, want %v", cfg.PrTrustedAuthors, want)
	}
}

func TestSplitCommaListTrimsAndDropsEmptyEntries(t *testing.T) {
	if got := splitCommaList(""); got != nil {
		t.Errorf("splitCommaList(\"\") = %v, want nil", got)
	}
	got := splitCommaList("octobot, release-bot ,,octobot")
	want := []string{"octobot", "release-bot", "octobot"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitCommaList = %v, want %v", got, want)
	}
}

func TestWorkerConfigRejectsPRPollIntervalBelowOneMinute(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-sandbox-image", "img@sha256:aaaa",
		"-pr-poll-interval", "30s",
	})
	if err == nil || !strings.Contains(err.Error(), "-pr-poll-interval must be at least 1m") {
		t.Fatalf("loadTestWorkerConfig(-pr-poll-interval 30s) = %v, want the minimum-interval refusal", err)
	}
}

// TestWorkerConfigRejectsNegativeReviewCorrectiveRounds mirrors
// TestWorkerConfigRejectsPRPollIntervalBelowOneMinute for
// -review-corrective-rounds: 0 disables the
// corrective round and is legal, but a negative value is rejected the same
// fail-fast-at-startup way every other worker knob's bad value is.
func TestWorkerConfigRejectsNegativeReviewCorrectiveRounds(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-sandbox-image", "img@sha256:aaaa",
		"-review-corrective-rounds", "-1",
	})
	if err == nil || !strings.Contains(err.Error(), "-review-corrective-rounds must not be negative") {
		t.Fatalf("loadTestWorkerConfig(-review-corrective-rounds -1) = %v, want the negative-value refusal", err)
	}
}

// TestApplySessionConfigFillsReviewCorrectiveRounds mirrors
// TestApplySessionConfigFillsPRPollIntervalAndIgnoreAuthors: proves the
// review_corrective_rounds session-config key reaches
// -review-corrective-rounds via applySessionConfig's own flag map.
func TestApplySessionConfigFillsReviewCorrectiveRounds(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "review_corrective_rounds: 2\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Int("review-corrective-rounds", defaultReviewCorrectiveRounds, "")
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("review-corrective-rounds").Value.String(); got != "2" {
		t.Errorf("-review-corrective-rounds = %q, want %q (session-config value silently dropped)", got, "2")
	}
}

// TestApplySessionConfigFillsPRPollIntervalAndIgnoreAuthors mirrors
// TestApplySessionConfigFillsRelayAllowedPathPrefixAndCredentialHeader:
// exercises applySessionConfig directly against a flag set registering
// just the two new flags, proving the session-config keys reach them.
func TestApplySessionConfigFillsPRPollIntervalAndIgnoreAuthors(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, `pr_poll_interval: 2m
pr_ignore_authors:
  - octobot
  - release-bot
pr_trusted_authors:
  - alice
  - bob
`)
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Duration("pr-poll-interval", 5*time.Minute, "")
	fs.String("pr-ignore-authors", "", "")
	fs.String("pr-trusted-authors", "", "")
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("pr-poll-interval").Value.String(); got != "2m0s" {
		t.Errorf("-pr-poll-interval = %q, want %q (session-config value silently dropped)", got, "2m0s")
	}
	if got := fs.Lookup("pr-ignore-authors").Value.String(); got != "octobot,release-bot" {
		t.Errorf("-pr-ignore-authors = %q, want %q (session-config value silently dropped)", got, "octobot,release-bot")
	}
	if got := fs.Lookup("pr-trusted-authors").Value.String(); got != "alice,bob" {
		t.Errorf("-pr-trusted-authors = %q, want %q (session-config value silently dropped)", got, "alice,bob")
	}
}

// TestApplySessionConfigLeavesPRTrustedAuthorsAtExplicitFlagValue proves an
// explicit command-line flag wins over the session config for
// -pr-trusted-authors, mirroring
// TestApplySessionConfigLeavesPRPollIntervalAtExplicitFlagValue.
func TestApplySessionConfigLeavesPRTrustedAuthorsAtExplicitFlagValue(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "pr_trusted_authors:\n  - octobot\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("pr-trusted-authors", "", "")
	if err := fs.Parse([]string{"-pr-trusted-authors", "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("pr-trusted-authors").Value.String(); got != "alice" {
		t.Errorf("-pr-trusted-authors = %q, want the explicit flag value %q preserved", got, "alice")
	}
}

// TestApplySessionConfigLeavesPRPollIntervalAtExplicitFlagValue proves an
// explicit command-line flag wins over the session config, the same
// explicit-flag-beats-config precedence every other Tier-1 flag has.
func TestApplySessionConfigLeavesPRPollIntervalAtExplicitFlagValue(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "pr_poll_interval: 2m\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Duration("pr-poll-interval", 5*time.Minute, "")
	if err := fs.Parse([]string{"-pr-poll-interval", "15m"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("pr-poll-interval").Value.String(); got != "15m0s" {
		t.Errorf("-pr-poll-interval = %q, want the explicit flag value %q", got, "15m0s")
	}
}

// TestWorkerConfigParsesHITLReminderIntervalFlag mirrors
// TestWorkerConfigParsesPRPollIntervalAndIgnoreAuthorsFlags for
// -hitl-reminder-interval: proves it parses cleanly through
// worker's real flag set and validation, reaching the worker.
func TestWorkerConfigParsesHITLReminderIntervalFlag(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	dataDir := t.TempDir()

	cfg, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", dataDir,
		"-skip-doctor",
		"-sandbox-image", "img@sha256:aaaa",
		"-hitl-reminder-interval", "10m",
	})
	if err != nil {
		t.Fatalf("loadWorkerConfig = %v, want the flags accepted", err)
	}
	if cfg.HitlReminderInterval != 10*time.Minute {
		t.Errorf("hitlReminderInterval = %v, want %v", cfg.HitlReminderInterval, 10*time.Minute)
	}
}

// TestWorkerConfigRejectsHITLReminderIntervalBelowOneMinute mirrors
// TestWorkerConfigRejectsPRPollIntervalBelowOneMinute.
func TestWorkerConfigRejectsHITLReminderIntervalBelowOneMinute(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-sandbox-image", "img@sha256:aaaa",
		"-hitl-reminder-interval", "30s",
	})
	if err == nil || !strings.Contains(err.Error(), "-hitl-reminder-interval must be at least 1m") {
		t.Fatalf("loadTestWorkerConfig(-hitl-reminder-interval 30s) = %v, want the minimum-interval refusal", err)
	}
}

// TestApplySessionConfigFillsHITLReminderInterval mirrors
// TestApplySessionConfigFillsPRPollIntervalAndIgnoreAuthors.
func TestApplySessionConfigFillsHITLReminderInterval(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "hitl_reminder_interval: 20m\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Duration("hitl-reminder-interval", 15*time.Minute, "")
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("hitl-reminder-interval").Value.String(); got != "20m0s" {
		t.Errorf("-hitl-reminder-interval = %q, want %q (session-config value silently dropped)", got, "20m0s")
	}
}

// TestApplySessionConfigLeavesHITLReminderIntervalAtExplicitFlagValue
// mirrors TestApplySessionConfigLeavesPRPollIntervalAtExplicitFlagValue.
func TestApplySessionConfigLeavesHITLReminderIntervalAtExplicitFlagValue(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "hitl_reminder_interval: 20m\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Duration("hitl-reminder-interval", 15*time.Minute, "")
	if err := fs.Parse([]string{"-hitl-reminder-interval", "5m"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("hitl-reminder-interval").Value.String(); got != "5m0s" {
		t.Errorf("-hitl-reminder-interval = %q, want the explicit flag value %q", got, "5m0s")
	}
}

// TestWorkerConfigParsesAdvanceOnFlag mirrors
// TestWorkerConfigParsesHITLReminderIntervalFlag for -advance-on: proves
// it parses cleanly through worker's real flag set and validation,
// reaching the worker.
func TestWorkerConfigParsesAdvanceOnFlag(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	dataDir := t.TempDir()

	cfg, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", dataDir,
		"-skip-doctor",
		"-sandbox-image", "img@sha256:aaaa",
		"-advance-on", "pr_approved",
	})
	if err != nil {
		t.Fatalf("loadWorkerConfig = %v, want the flags accepted", err)
	}
	if cfg.AdvanceOn != "pr_approved" {
		t.Errorf("advanceOn = %v, want %v", cfg.AdvanceOn, "pr_approved")
	}
}

// TestWorkerConfigRejectsInvalidAdvanceOn covers -advance-on's own
// validation: only "accepted" or "pr_approved" are legal.
func TestWorkerConfigRejectsInvalidAdvanceOn(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-sandbox-image", "img@sha256:aaaa",
		"-advance-on", "immediately",
	})
	if err == nil || !strings.Contains(err.Error(), `-advance-on must be "accepted" or "pr_approved"`) {
		t.Fatalf("loadTestWorkerConfig(-advance-on immediately) = %v, want the invalid-value refusal", err)
	}
}

// TestApplySessionConfigFillsAdvanceOn mirrors
// TestApplySessionConfigFillsHITLReminderInterval for advance_on.
func TestApplySessionConfigFillsAdvanceOn(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "advance_on: pr_approved\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("advance-on", requestdriver.AdvanceOnAccepted, "")
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("advance-on").Value.String(); got != requestdriver.AdvanceOnPRApproved {
		t.Errorf("-advance-on = %q, want %q (session-config value silently dropped)", got, requestdriver.AdvanceOnPRApproved)
	}
}

// TestApplySessionConfigLeavesAdvanceOnAtExplicitFlagValue mirrors
// TestApplySessionConfigLeavesHITLReminderIntervalAtExplicitFlagValue.
func TestApplySessionConfigLeavesAdvanceOnAtExplicitFlagValue(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "advance_on: pr_approved\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("advance-on", requestdriver.AdvanceOnAccepted, "")
	if err := fs.Parse([]string{"-advance-on", requestdriver.AdvanceOnAccepted}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("advance-on").Value.String(); got != requestdriver.AdvanceOnAccepted {
		t.Errorf("-advance-on = %q, want the explicit flag value %q", got, requestdriver.AdvanceOnAccepted)
	}
}

func TestWorkerConfigExplicitConfigPathMustExist(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	missing := filepath.Join(t.TempDir(), "nope.yml")
	_, _, err := loadTestWorkerConfig(dp, []string{"-config", missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("loadTestWorkerConfig(-config missing) = %v, want an error naming the path", err)
	}
}

func TestInitConfigWritesExampleAndRefusesToOverwrite(t *testing.T) {
	path := isolateSessionConfig(t)
	if err := initConfigMain(nil); err != nil {
		t.Fatalf("initConfigMain: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != sessionconfig.Example {
		t.Fatalf("read %s: %v, content matches Example: %v", path, err, string(data) == sessionconfig.Example)
	}
	err = initConfigMain(nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second initConfigMain = %v, want an already-exists refusal", err)
	}
}

// TestDoctorChecksForAlwaysChecksDocker: worker never runs unsandboxed,
// so its doctor preflight always includes the Docker daemon and sandbox
// image checks.
func TestDoctorChecksForAlwaysChecksDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checks := doctorChecksFor(ctx, doctorInputs{sandboxDocker: filepath.Join(t.TempDir(), "no-docker")})
	if len(checks) == 0 || !strings.Contains(checks[0].Name, "docker daemon") {
		t.Errorf("sandboxed inputs did not start with the docker check: %+v", checks)
	}
}

// TestWorkerConfigAcceptsConformityPolicyFlag mirrors
// TestWorkerConfigAcceptsPlanTicketsFlags: passing this flag must reach
// past flags.Parse to the -registry-proxy/-sandbox-image refusal, not
// stop at "flag provided but not defined".
func TestWorkerConfigAcceptsConformityPolicyFlag(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-conformity-policy", "advisory",
		"-registry-proxy", "-registry-proxy-image", "registry.example/org/rp@sha256:deadbeef",
	})
	if err == nil {
		t.Fatal("loadTestWorkerConfig(-conformity-policy, -registry-proxy without -sandbox-image) = nil, want an error")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("workerMain rejected a real flag: %v", err)
	}
	if !strings.Contains(err.Error(), "-registry-proxy requires -sandbox-image") {
		t.Fatalf("err = %v, want it to name the missing -sandbox-image (proving flag parsing got past -conformity-policy)", err)
	}
}

// TestWorkerConfigRejectsInvalidConformityPolicy covers -conformity-policy's
// own validation, at worker's own startup rather than deep inside a
// drained entry (acceptance criterion 3, issue #164): only "required" or
// "advisory" are legal, mirroring build_app.py's own --conformity-policy
// argparse choices.
func TestWorkerConfigRejectsInvalidConformityPolicy(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-sandbox-image", "img@sha256:aaaa",
		"-conformity-policy", "sometimes",
	})
	if err == nil || !strings.Contains(err.Error(), `-conformity-policy must be "required" or "advisory"`) {
		t.Fatalf("loadTestWorkerConfig(-conformity-policy sometimes) = %v, want the invalid-value refusal", err)
	}
}

// TestApplySessionConfigFillsConformityPolicy covers the session-config
// key (conformity_policy) issue #164 also asks for, mirroring
// TestApplySessionConfigFillsAdvanceOn.
func TestApplySessionConfigFillsConformityPolicy(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "conformity_policy: advisory\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("conformity-policy", "required", "")
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("conformity-policy").Value.String(); got != "advisory" {
		t.Errorf("-conformity-policy = %q, want %q (session-config value silently dropped)", got, "advisory")
	}
}

// TestApplySessionConfigLeavesConformityPolicyAtExplicitFlagValue mirrors
// TestApplySessionConfigLeavesAdvanceOnAtExplicitFlagValue: an explicit
// CLI flag always wins over the session-config value.
func TestApplySessionConfigLeavesConformityPolicyAtExplicitFlagValue(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "conformity_policy: advisory\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("conformity-policy", "required", "")
	if err := fs.Parse([]string{"-conformity-policy", "required"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("conformity-policy").Value.String(); got != "required" {
		t.Errorf("-conformity-policy = %q, want the explicit flag value %q", got, "required")
	}
}

// TestWorkerConfigRejectsInvalidCodeReviewPolicy mirrors
// TestWorkerConfigRejectsInvalidConformityPolicy for -code-review-policy.
func TestWorkerConfigRejectsInvalidCodeReviewPolicy(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-sandbox-image", "img@sha256:aaaa",
		"-code-review-policy", "sometimes",
	})
	if err == nil || !strings.Contains(err.Error(), "-code-review-policy must be one of off/advisory/required") {
		t.Fatalf("loadTestWorkerConfig(-code-review-policy sometimes) = %v, want the invalid-value refusal", err)
	}
}

// TestApplySessionConfigFillsCodeReviewPolicy mirrors
// TestApplySessionConfigFillsConformityPolicy for the code_review_policy
// session-config key.
func TestApplySessionConfigFillsCodeReviewPolicy(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "code_review_policy: advisory\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("code-review-policy", "off", "")
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("code-review-policy").Value.String(); got != "advisory" {
		t.Errorf("-code-review-policy = %q, want %q (session-config value silently dropped)", got, "advisory")
	}
}

// TestApplySessionConfigLeavesCodeReviewPolicyAtExplicitFlagValue mirrors
// TestApplySessionConfigLeavesConformityPolicyAtExplicitFlagValue: an
// explicit CLI flag always wins over the session-config value.
func TestApplySessionConfigLeavesCodeReviewPolicyAtExplicitFlagValue(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "code_review_policy: advisory\n")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("code-review-policy", "off", "")
	if err := fs.Parse([]string{"-code-review-policy", "off"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applySessionConfig(fs, ""); err != nil {
		t.Fatalf("applySessionConfig: %v", err)
	}
	if got := fs.Lookup("code-review-policy").Value.String(); got != "off" {
		t.Errorf("-code-review-policy = %q, want the explicit flag value %q", got, "off")
	}
}

// runQueueEntryWithPolicyGateFixture is the shared setup for
// TestTicketRunPolicyGating (acceptance criteria 1/2, issue #164):
// drives a single QueueEntry through the REAL runQueueEntry/
// buildTicketRunArgs/runMainWithReady path (not a stub runner), sandboxed
// through testdata/fake_docker.sh, against
// testdata/fake_build_app_policy_gate.py -- a fixture that simulates
// build_app.py's own fail-closed behavior on an unparseable/unreachable
// conformity verdict (exit 1 when -conformity-policy is "required", exit
// 0 and commit otherwise) -- and returns the resulting run's terminal
// state.
func runQueueEntryWithPolicyGateFixture(dp *deps, t *testing.T, conformityPolicy string) run.State {
	t.Helper()
	workspace := testfixture.NewGitRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- policy-gate fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app_policy_gate.py")
	if err != nil {
		t.Fatalf("resolve fixture script: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	apiSandboxTestMu.Lock()
	settings := sessionconfig.DefaultSettings()
	settings.SandboxDocker = fakeSandboxDockerBinary(t)
	settings.SandboxImage = fakeSandboxImage
	tier2SettingsOverride = &settings
	t.Cleanup(func() {
		tier2SettingsOverride = nil
		apiSandboxTestMu.Unlock()
	})

	dataDir := t.TempDir()
	entry := &requestdriver.QueueEntry{
		// "fixture-ticket": matches testfixture.NewGitRepo's own scaffolded
		// spec/tickets/001-fixture-ticket.md via resolvePiTicketPath's
		// "???-<ticket>.md" glob fallback (see runFactoryd's own -ticket
		// value in integration_direct_and_serve_test.go for the same
		// trick) -- an arbitrary ticket id would fail the mandatory
		// project-bootstrap preflight's ticket_structure check instead.
		ID:            "fixture-ticket",
		Workspace:     workspace,
		SpecPath:      specPath,
		VerifyCommand: "true",
	}
	cfg := requestdriver.WorkerConfig{
		BuildAppScript:           script,
		SandboxImage:             fakeSandboxImage,
		ConformityPolicy:         conformityPolicy,
		ConformityPolicyExplicit: true,
		Settings:                 settings,
		TemporalAddress:          sharedTemporalAddress(t),
	}

	before, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read runs dir before invocation: %v", err)
	}
	existed := make(map[string]bool, len(before))
	for _, e := range before {
		existed[e.Name()] = true
	}

	// runQueueEntry's own error (a non-zero factoryd exit, e.g. for a
	// quarantined/halted outcome) is not asserted here -- exactly like
	// runFactoryd's own doc comment: a halted or quarantined run is a
	// valid, expected outcome for the -required case.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if runErr := runMainWithReady(dp, ctx, requestdriver.BuildTicketRunArgs(dataDir, entry, cfg), func(*run.Run) {}); runErr != nil {
		t.Logf("runMainWithReady error (may be expected for a blocked/halted outcome): %v", runErr)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	var newEntries []os.DirEntry
	for _, e := range entries {
		if !existed[e.Name()] {
			newEntries = append(newEntries, e)
		}
	}
	if len(newEntries) != 1 {
		t.Fatalf("expected exactly 1 new run directory, got %d: %v", len(newEntries), entries)
	}
	r, err := run.Load(dataDir, newEntries[0].Name())
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	return r.State
}

// TestTicketRunPolicyGating is acceptance criteria 1 and 2 from issue
// #164: a worker-drained entry with -conformity-policy required (the
// default) must still block on an unparseable/unreachable conformity
// response -- proving the flag reaches the drained child at all, not
// just that build_app.py's own fail-closed default fires with no way to
// override it -- while advisory, issue #164's own documented escape
// valve, must let the same scenario through to acceptance instead of
// quarantining objectively-correct work.
func TestTicketRunPolicyGating(t *testing.T) {
	dp := newTestDeps(t)
	tests := []struct {
		name             string
		conformityPolicy string
		want             run.State
	}{
		{"conformity required blocks", "required", run.StateQuarantined},
		{"conformity advisory does not block", "advisory", run.StateAccepted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runQueueEntryWithPolicyGateFixture(dp, t, test.conformityPolicy)
			if got != test.want {
				t.Fatalf("state = %q, want %q", got, test.want)
			}
		})
	}
}
