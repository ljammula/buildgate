package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	temporalclient "go.temporal.io/sdk/client"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

func TestIntegrationFactoryDoesNotCommitVerificationOutputWhenVerificationFails(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	preVerifyHEAD, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD before run: %v", err)
	}
	// Writes dirt (like a formatter would) and then fails, the same way a
	// real "fmt + test" verify command can rewrite files before its test
	// step fails.
	r := runFactorydWithSpecAndFlags(t, ws, "commit", afterBaseline(t, "echo formatted >> content.txt; exit 1"), "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil)

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if r.CommittedByFactoryd {
		t.Error("CommittedByFactoryd = true, want false: verification failed, so its dirt must not have been committed as a safety net")
	}
	out, err := exec.Command("git", "-C", r.WorkspacePath, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(out) == 0 {
		t.Error("workspace is clean after a failed verification that wrote dirt; expected that dirt to remain uncommitted")
	}
	postRunHEAD, err := exec.Command("git", "-C", r.WorkspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD after run: %v", err)
	}
	// mode "commit" itself commits once (build-time), so HEAD does move —
	// the point is it must not move *again* for verification's own dirt.
	if string(postRunHEAD) == string(preVerifyHEAD) {
		t.Fatal("HEAD never advanced at all; the build-time commit itself is missing")
	}
	if r.ResultSHA != strings.TrimSpace(string(postRunHEAD)) {
		t.Fatalf("ResultSHA = %q, want it to match actual HEAD %q (no extra commit beyond the build's own)", r.ResultSHA, strings.TrimSpace(string(postRunHEAD)))
	}
}

func TestIntegrationBuildFailureQuarantines(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	// fake_build_app.sh exits 1 without touching the workspace: a normal
	// non-zero exit, not an infrastructure failure, so factoryd still
	// runs canonical verification (it has nothing new to verify, but the
	// gate itself is what must fail here — the build's own exit code
	// alone must never be trusted as the pass/fail signal).
	r := runFactoryd(t, ws, "fail", "true")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if attempts := afterBaselineAttempt(t, r.Attempts); len(attempts) != 2 || attempts[0].Kind != "build" || attempts[0].ExitCode != 1 || attempts[1].Kind != "verify" {
		t.Errorf("expected the baseline, one failed build attempt, then one verify attempt, got %v", r.Attempts)
	}
	// canonical_verify (failing) plus tests_added (passing via the opt-out
	// runFactoryd's spec content carries) -- see that helper's own comment.
	if len(r.GateResults) != 2 || r.GateResults[0].Passed {
		t.Errorf("expected one failing gate result (gated on build exit code, not just verify), got %v", r.GateResults)
	}
}

func TestIntegrationVerifyFailureQuarantinesAndRecordsNotification(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit", afterBaseline(t, "false"))

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if len(r.GateResults) != 2 || r.GateResults[0].Passed {
		t.Errorf("expected one failing gate result, got %v", r.GateResults)
	}
	if len(r.Notifications) != 1 {
		t.Fatalf("Notifications = %v, want exactly one quarantine notification", r.Notifications)
	}
	n := r.Notifications[0]
	if n.RunID != r.ID || n.Ticket != r.Ticket || n.State != run.StateQuarantined || n.SentAt == "" {
		t.Errorf("notification = %+v, want run/ticket IDs, quarantined state, and sent timestamp", n)
	}
	// The reason leads with the gate and, since the triage sentence exists
	// (run.Run.Triage), continues with a factory-authored "why".
	if !strings.HasPrefix(n.Reason, "policy gate did not pass: canonical_verify") {
		t.Errorf("notification reason = %q, want canonical policy-gate failure", n.Reason)
	}
	if n.Delivered == nil || !*n.Delivered || n.DeliveryError != "" {
		t.Errorf("notification = %+v, want Delivered=true and no DeliveryError for a successful local LogNotifier append", n)
	}
	// This run belongs to no request, so Next must point the operator at
	// watching the run directly, not a nonexistent owning request.
	if want := "factoryd watch " + r.ID; n.Next != want {
		t.Errorf("notification Next = %q, want %q", n.Next, want)
	}
}

// TestIntegrationQuarantineNotifiesDiscordWebhooks confirms
// FACTORYD_DISCORD_WEBHOOK_URLS is read as a comma-separated list and
// every entry receives the quarantine notification, alongside (not
// instead of) the durable LogNotifier record.
func TestIntegrationQuarantineNotifiesDiscordWebhooks(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndEnv(t, ws, "commit", afterBaseline(t, "false"), "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s",
		[]string{"FACTORYD_DISCORD_WEBHOOK_URLS=" + srv.URL + "/hook-a, " + srv.URL + "/hook-b"})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if len(r.Notifications) != 1 || r.Notifications[0].Delivered == nil || !*r.Notifications[0].Delivered {
		t.Fatalf("Notifications = %v, want the durable LogNotifier record unaffected by Discord dispatch", r.Notifications)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"/hook-a", "/hook-b"}
	slices.Sort(received)
	if !slices.Equal(received, want) {
		t.Errorf("webhook paths received = %v, want %v", received, want)
	}
}

// TestIntegrationQuarantineNotifiesSlackWebhook is the regression test
// for the Codex finding round 2 on PR #88: the quarantine
// notification called notify.DiscordNotifier directly rather than
// notify.DispatchExternal, so an operator configuring only
// FACTORYD_SLACK_WEBHOOK_URLS (no Discord) never got paged on quarantine
// even though DispatchExternal's own documentation says all three
// channels fire. Deliberately sets only the Slack env var, not Discord.
func TestIntegrationQuarantineNotifiesSlackWebhook(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndEnv(t, ws, "commit", afterBaseline(t, "false"), "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s",
		[]string{"FACTORYD_SLACK_WEBHOOK_URLS=" + srv.URL})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if !hit.Load() {
		t.Error("Slack webhook was never hit for a quarantined run with only FACTORYD_SLACK_WEBHOOK_URLS configured")
	}
}

// TestIntegrationQuarantinePersistsBeforeDiscordCompletes is the
// regression test for a real finding from review: run.json used to be
// saved only after the Discord dispatch loop, so a crash or forced
// shutdown during a slow/hung webhook's wait would leave the durable
// record stuck at its previously saved "verifying" state instead of the
// "quarantined" the run actually reached. The fixture Discord endpoint
// here blocks until explicitly released; while it's still blocked (proven
// by hitting the endpoint at all), run.json must already show
// state=quarantined.
func TestIntegrationQuarantinePersistsBeforeDiscordCompletes(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	reached := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer closeRelease()

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", afterBaseline(t, "false"),
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"FACTORYD_DISCORD_WEBHOOK_URLS="+srv.URL,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output synchronizedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd: %v", err)
	}
	processWaited := false
	t.Cleanup(func() {
		if !processWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		processWaited = true
		t.Fatalf("discord endpoint was never reached; output:\n%s", output.String())
	}

	// The Discord handler is still blocked on <-release right now. If
	// run.json already shows quarantined, the save happened before this
	// wait, not after it.
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("read runs dir: %v, %v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "runs", entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if r.State != run.StateQuarantined {
		t.Errorf("state = %q while Discord is still in flight, want %q already persisted", r.State, run.StateQuarantined)
	}

	closeRelease()
	if err := cmd.Wait(); err != nil {
		// A non-zero exit for a quarantined run is expected; see runFactoryd's doc comment.
		t.Logf("factoryd exited: %v", err)
	}
	processWaited = true
}

// TestIntegrationOverrideDuringDiscordWaitIsNotLost is the regression test
// for a real P1 finding from codex review of the API server's new
// POST /runs/{id}/override endpoint: applyRunWorkflowResult/realMain used
// to save run.json once to persist "quarantined" before their Discord
// webhook wait, then save it a *second* time afterward (to attach
// AgentEvidence) from the same stale in-memory copy — so an override
// applied by a separate process while the first factoryd was still
// blocked on Discord would be silently reverted by that second save. This
// starts a real factoryd blocked on a slow Discord webhook (same fixture
// as TestIntegrationQuarantinePersistsBeforeDiscordCompletes), applies a
// real `factoryd override` while it's still blocked, then releases it and
// proves the override survived — not reverted back to quarantined, and
// not missing its audit entry.
func TestIntegrationOverrideDuringDiscordWaitIsNotLost(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	reached := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer closeRelease()

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", afterBaseline(t, "false"),
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"FACTORYD_DISCORD_WEBHOOK_URLS="+srv.URL,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output synchronizedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd: %v", err)
	}
	processWaited := false
	t.Cleanup(func() {
		if !processWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		processWaited = true
		t.Fatalf("discord endpoint was never reached; output:\n%s", output.String())
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("read runs dir: %v, %v", entries, err)
	}
	runID := entries[0].Name()

	// The originating factoryd is still blocked on <-release right now —
	// its own single save already landed (proven by
	// TestIntegrationQuarantinePersistsBeforeDiscordCompletes), so nothing
	// it does from here on should ever touch run.json again. Applying a
	// real override here, from a wholly separate process, while it's
	// still blocked is exactly the race window the fix closes.
	overrideCmd := factorydCommand(t, "override",
		"-run", runID,
		"-data-dir", dataDir,
		"-by", "operator",
		"-reason", "reviewed manually during Discord wait",
		"-state", "accepted",
	)
	if out, err := overrideCmd.CombinedOutput(); err != nil {
		t.Fatalf("factoryd override: %v: %s", err, out)
	}

	closeRelease()
	if err := cmd.Wait(); err != nil {
		t.Logf("factoryd exited: %v", err)
	}
	processWaited = true

	b, err := os.ReadFile(filepath.Join(dataDir, "runs", runID, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q after the Discord wait completed, want %q — the override must not have been reverted", r.State, run.StateAccepted)
	}
	if len(r.Overrides) != 1 || r.Overrides[0].By != "operator" {
		t.Fatalf("Overrides = %+v, want exactly one entry recording the override applied during the Discord wait", r.Overrides)
	}
}

// TestIntegrationOverrideRaceAcrossCLIAndAPIServer is the regression test
// for a real P2 finding from a second codex review round: `run.WithLock`
// closes the lost-update window between the API server's own
// POST /runs/{id}/override handler and `cmd/factoryd`'s `override` CLI
// subcommand — two genuinely separate processes, so an earlier in-process
// mutex (internal/api's own overrideMu, since removed) could never have
// excluded them from each other. Fires a real `factoryd override` process
// and a real HTTP request at `factoryd serve`'s own override endpoint at
// the same instant against the same quarantined run; exactly one must
// succeed, and the run must end up with exactly one Overrides entry, not
// two and not zero.
func TestIntegrationOverrideRaceAcrossCLIAndAPIServer(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seeded := run.Run{
		ID:        "run-race",
		Ticket:    "ticket-race",
		State:     run.StateQuarantined,
		CreatedAt: "2026-08-26T10:00:00Z",
	}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	const token = "test-override-token"
	serveCmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr)
	serveCmd.Env = append(os.Environ(), "FACTORYD_API_OVERRIDE_TOKEN="+token)
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	defer func() {
		_ = serveCmd.Process.Kill()
		_ = serveCmd.Wait()
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://" + addr + "/runs"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var wg sync.WaitGroup
	var cliErr error
	var httpStatus int
	var httpErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		cmd := factorydCommand(t, "override",
			"-run", "run-race",
			"-data-dir", dataDir,
			"-by", "cli-operator",
			"-reason", "reviewed via CLI",
			"-state", "accepted",
		)
		cliErr = cmd.Run()
	}()
	go func() {
		defer wg.Done()
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/runs/run-race/override",
			strings.NewReader(`{"by":"api-operator","reason":"reviewed via API","state":"halted"}`))
		if err != nil {
			httpErr = err
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			httpErr = err
			return
		}
		defer resp.Body.Close()
		httpStatus = resp.StatusCode
	}()
	wg.Wait()

	if httpErr != nil {
		t.Fatalf("POST override: %v", httpErr)
	}
	cliSucceeded := cliErr == nil
	apiSucceeded := httpStatus == http.StatusOK
	if cliSucceeded == apiSucceeded {
		t.Fatalf("cliSucceeded=%v (err=%v) apiSucceeded=%v (status=%d) — want exactly one to succeed", cliSucceeded, cliErr, apiSucceeded, httpStatus)
	}

	b, err := os.ReadFile(filepath.Join(dataDir, "runs", "run-race", "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if len(r.Overrides) != 1 {
		t.Fatalf("Overrides = %+v, want exactly 1 entry — the loser must not have clobbered the winner's", r.Overrides)
	}
	wantBy := "cli-operator"
	wantState := run.StateAccepted
	if apiSucceeded {
		wantBy, wantState = "api-operator", run.StateHalted
	}
	if r.Overrides[0].By != wantBy || r.State != wantState {
		t.Fatalf("run = {By: %q, State: %q}, want {%q, %q} (the actual winner's own override)", r.Overrides[0].By, r.State, wantBy, wantState)
	}
}

// TestIntegrationAPIOverrideRecordsReleaseDecision is
// TestIntegrationOverrideRecordsReleaseDecision's counterpart for
// internal/api's own POST /runs/{id}/override -- the same Codex review
// finding on PR #45 applied to both override paths independently, since
// they're two separate code paths (cmd/factoryd's overrideMain and
// internal/api.Server.overrideRun) that happen to share run.WithLock, not
// one shared implementation.
func TestIntegrationAPIOverrideRecordsReleaseDecision(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	dataDir := t.TempDir()
	ws := newFixtureRepo(t)
	seeded := run.Run{
		ID:          "run-api-override-release",
		Ticket:      "ticket-api-override-release",
		ProjectPath: ws,
		State:       run.StateQuarantined,
		CreatedAt:   "2026-08-26T10:00:00Z",
	}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	const token = "test-override-token"
	serveCmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr, "-release-allow-overrides")
	serveCmd.Env = append(os.Environ(), "FACTORYD_API_OVERRIDE_TOKEN="+token)
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	defer func() {
		_ = serveCmd.Process.Kill()
		_ = serveCmd.Wait()
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://" + addr + "/runs"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/runs/"+seeded.ID+"/override",
		strings.NewReader(`{"by":"api-operator","reason":"reviewed via API","state":"accepted"}`))
	if err != nil {
		t.Fatalf("build override request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST override: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST override status = %d, want 200: %s", resp.StatusCode, body)
	}

	project := release.ProjectFromWorkspace(ws)
	decisionPath := filepath.Join(dataDir, "projects", project, "release-decisions", seeded.ID+".json")
	b, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatalf("read release decision %s after API override: %v", decisionPath, err)
	}
	var decision release.Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("unmarshal release decision: %v", err)
	}
	if decision.RunID != seeded.ID {
		t.Errorf("decision.RunID = %q, want %q", decision.RunID, seeded.ID)
	}
}

// TestIntegrationTicketVerifyCommandOverridesFlagDefault is the spec
// cross-reference test: it proves factoryd actually runs the verify
// command the *ticket* declares, not whatever -verify-command happens to
// default to. The ticket declares "true" (always passes); the flag is
// deliberately set to "false" (always fails). If factoryd used the flag
// instead of the ticket, this run would halt; if it correctly reads the
// ticket's Verify-Command key, it accepts.
func TestIntegrationTicketVerifyCommandOverridesFlagDefault(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Acceptance / verification\n\nVerify-Command: true\n"

	r := runFactorydWithSpec(t, ws, "commit", "false", specContent, "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — factoryd should have used the ticket's declared verify command (true), not the flag default (false)", r.State, run.StateAccepted)
	}
	// canonical_verify plus tests_added (which always runs -- see
	// runFactorydWithSpecFlagsAndDataDir's own opt-out injection, which
	// this specContent doesn't override) plus full_suite_verify: no
	// -full-suite-command/.factory.yml full_suite_command is configured
	// here, so it's now substituted with this ticket's own effective
	// verify command -- see TestIntegrationFullSuiteDefaultsToVerifyCommand
	// for that substitution's own dedicated test.
	if len(r.GateResults) != 3 {
		t.Fatalf("expected exactly three gate results (canonical_verify, tests_added, full_suite_verify), got %v", r.GateResults)
	}
	want := []string{"sh", "-c", "true"}
	// On the Temporal path the recorded argv is the sandbox's docker run
	// wrapper (RunVerifyActivity records sandbox.Run's command), whose tail is
	// the verify command itself: "... <image> sh -c <command>".
	endsWithWant := func(argv []string) bool {
		return len(argv) >= len(want) && slices.Equal(argv[len(argv)-len(want):], want)
	}
	if !endsWithWant(r.GateResults[0].Command) {
		t.Errorf("gate command = %v, want %v (the ticket's declared command)", r.GateResults[0].Command, want)
	}
	fullSuiteGate, ok := gateResultByCheck(r.GateResults, "full_suite_verify")
	if !ok {
		t.Fatalf("GateResults = %v, want a full_suite_verify gate", r.GateResults)
	}
	// The substituted full_suite_verify command must be the TICKET's own
	// effective verify command ("true"), not the bare -verify-command
	// flag default ("false") this run was started with -- confirming the
	// substitution happens after the ticket override resolves, not
	// before it.
	if !endsWithWant(fullSuiteGate.Command) {
		t.Errorf("full_suite_verify gate command = %v, want %v (the ticket's declared command, substituted -- not the flag default \"false\")", fullSuiteGate.Command, want)
	}
	if r.FullSuiteSource != fullSuiteSourceVerifyCommand {
		t.Errorf("FullSuiteSource = %q, want %q", r.FullSuiteSource, fullSuiteSourceVerifyCommand)
	}
}

// gateResultByCheck finds the first run.GateResult in results whose Check
// matches name, for tests that need to inspect one named gate's own
// fields (command, pass/fail) rather than just its presence/absence.
func gateResultByCheck(results []run.GateResult, name string) (run.GateResult, bool) {
	for _, g := range results {
		if g.Check == name {
			return g, true
		}
	}
	return run.GateResult{}, false
}

func assertClean(t *testing.T, workspace string) {
	t.Helper()
	out, err := exec.Command("git", "-C", workspace, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected clean workspace after run, got:\n%s", out)
	}
}

// TestIntegrationRejectsTicketWithPathTraversal pins a guard found by a
// golangci-lint (gosec G703 taint analysis) pass: -ticket becomes part of
// the durable run ID, which is joined onto -data-dir to build every path
// under that run's own directory. An unvalidated -ticket containing ".."
// could otherwise write outside data/runs/<id>/.
func TestIntegrationRejectsTicketWithPathTraversal(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "../../escape-attempt",
		"-sandbox-image", fakeSandboxImage,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-workspace", ws,
		"-spec", specPath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd exited 0 for a path-traversal -ticket, want a rejecting error; output:\n%s", out)
	}
	if !strings.Contains(string(out), "single path component") {
		t.Errorf("factoryd output = %q, want it to name the -ticket validation error", out)
	}

	entries, statErr := os.ReadDir(filepath.Join(dataDir, "runs"))
	if statErr == nil && len(entries) != 0 {
		t.Errorf("expected no run directory created, got %v", entries)
	}
}

// TestIntegrationRequireDeclaredScopeFlagAbsentProceedsNormally verifies
// that when -require-declared-scope is not passed (the default), a ticket
// that declares neither Allowed-Files: nor Required-Changed-Files: runs
// normally and reaches state=accepted, proving zero behavior change when
// the flag is off.
func TestIntegrationRequireDeclaredScopeFlagAbsentProceedsNormally(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	// Ticket with neither Allowed-Files: nor Required-Changed-Files:
	specContent := "# Ticket: fixture\n\n## Acceptance\n\nno scope declared\n"
	r := runFactorydWithSpec(t, ws, "commit", "true", specContent, "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — ticket without scope declarations must proceed when -require-declared-scope is not set", r.State, run.StateAccepted)
	}
}

// TestIntegrationRequireDeclaredScopeHaltsWhenKeysMissing verifies that
// when -require-declared-scope is passed, a ticket declaring neither
// Allowed-Files: nor Required-Changed-Files: causes factoryd to halt
// before build_app.py is invoked at all. The run is halted (not quarantined
// from a gate failure), and no attempt is recorded, proving the halt
// happened early — before build_app.py ran.
func TestIntegrationRequireDeclaredScopeHaltsWhenKeysMissing(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	// Ticket with neither Allowed-Files: nor Required-Changed-Files:
	specContent := "# Ticket: fixture\n\n## Acceptance\n\nno scope declared\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	ticket := "fixture-ticket"
	cmd := factorydCommand(t,
		"-ticket", ticket,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-require-declared-scope", // Flag is set
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // exit code intentionally unchecked
	outStr := output.String()
	t.Logf("factoryd output:\n%s", outStr)

	// Verify the run halted and no gate results exist (early halt, not gate failure)
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory, got %d: %v", len(entries), entries)
	}

	b, err := os.ReadFile(filepath.Join(dataDir, "runs", entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q — missing scope declarations with -require-declared-scope must halt early", r.State, run.StateHalted)
	}
	if len(r.GateResults) != 0 {
		t.Errorf("GateResults = %v, want none — the run must halt before any gate evaluates", r.GateResults)
	}
	if strings.Contains(outStr, "fake_build_app: mode=") {
		t.Error("factoryd output contains fake_build_app marker, but build_app.py should never have been invoked with -require-declared-scope and missing declarations")
	}
}

// TestIntegrationRequireDeclaredScopeProceedsWhenBothKeysPresent verifies
// that when -require-declared-scope is passed and the ticket declares both
// Allowed-Files: and Required-Changed-Files:, the run proceeds normally
// and reaches state=accepted.
func TestIntegrationRequireDeclaredScopeProceedsWhenBothKeysPresent(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	// Ticket with both Allowed-Files: and Required-Changed-Files:
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\nRequired-Changed-Files: content.txt\n"
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", specContent, "30s", nil, []string{"-require-declared-scope"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — ticket with both scope declarations must proceed even with -require-declared-scope", r.State, run.StateAccepted)
	}
}

// TestIntegrationIsolatedTemporalUnreachableHaltsInsteadOfFallingBack: a dial
// failure must halt rather than silently execute anywhere else, since a run
// never quietly loses the containment/rollback guarantees isolation gives.
func TestIntegrationIsolatedTemporalUnreachableHaltsInsteadOfFallingBack(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel.
	ws := newFixtureRepo(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	unreachable := ln.Addr().String()
	ln.Close()

	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-temporal-address", unreachable})

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q — an unreachable -temporal-address combined with the now-default isolation must halt, not fall back", r.State, run.StateHalted)
	}
}

// TestIntegrationTemporalRelativeDataDirResolvesCheckpointDir is
// TestIntegrationRelativeDataDirResolvesSnapshotPath's counterpart for the
// Temporal path — the regression test for a real P2 finding from review:
// runViaTemporal built LogDir/CheckpointDir directly from -data-dir
// without resolving it to absolute first. -data-dir defaults to the
// relative "data", and these directories are now carried in
// RunWorkflowInput specifically so a Worker other than this process's own
// could execute RunBuildActivity/RunVerifyActivity correctly (see
// RunWorkflowInput.LogDir's doc comment) — a relative path would resolve
// under whichever Worker's own working directory happens to execute the
// Activity, not the submitting invocation's, defeating that guarantee.
// This test runs factoryd from a harness directory distinct from the
// workspace (the same cwd/workspace split
// TestIntegrationRelativeDataDirResolvesSnapshotPath reproduces) and
// confirms both that the run still completes and that its checkpoint
// files actually landed under the harness directory's own relative
// -data-dir, not somewhere else silently.
func TestIntegrationTemporalRelativeDataDirResolvesCheckpointDir(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	harnessDir := t.TempDir() // deliberately not ws — reproduces the cwd/workspace split

	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", "data", // relative, and harnessDir != ws — the exact mismatch
		"-temporal-address", address,
	)
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, _ := cmd.CombinedOutput()
	t.Logf("factoryd output:\n%s", out)

	entries, err := os.ReadDir(filepath.Join(harnessDir, "data", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory under the harness's relative -data-dir: entries=%v err=%v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(harnessDir, "data", "runs", entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (relative -data-dir with a different workspace cwd should still resolve through the Temporal path)", r.State, run.StateAccepted)
	}
	if len(r.Attempts) == 0 {
		t.Fatal("Attempts is empty — checkpoints must have been read back from the correctly-resolved CheckpointDir for this to be populated at all")
	}

	checkpointEntries, err := os.ReadDir(filepath.Join(harnessDir, "data", "temporal-checkpoints", r.ID, "activity-checkpoints"))
	if err != nil || len(checkpointEntries) == 0 {
		t.Fatalf("expected checkpoint files under the harness's relative -data-dir's temporal-checkpoints/%s, got entries=%v err=%v", r.ID, checkpointEntries, err)
	}
}

// TestIntegrationTemporalRoutesThroughRealServer is real-server proof that
// -temporal-address actually routes execution through
// internal/workflow.RunWorkflow, not just that the flag is accepted. It
// skips when no Temporal server is reachable at the default address (or
// $TEMPORAL_ADDRESS), matching internal/workflow's own live-test skip
// convention, so ordinary verification does not require a running server.
// TestIntegrationTemporalRepositoryOwnerRoutesRun is a smoke test proving
// -repository actually routes a run through
// internal/workflow.RepositoryOwnerWorkflow (runViaRepositoryOwner), not
// just that the flag is accepted — a real out-of-scope file must still be
// caught by diff_scope through this path too, exactly like
// TestIntegrationTemporalRoutesThroughRealServer proves for the direct
// RunWorkflow path.
func TestIntegrationTemporalRepositoryOwnerRoutesRun(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())
	r := runFactorydWithSpecAndFlags(t, ws, "commit_extra", "true", specContent, "30s", nil,
		[]string{"-temporal-address", address, "-repository", repository})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — diff_scope must still catch an out-of-scope file when routed through RepositoryOwnerWorkflow", r.State, run.StateQuarantined)
	}
	var scopeGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "diff_scope" {
			scopeGate = &r.GateResults[i]
		}
	}
	if scopeGate == nil || scopeGate.Passed {
		t.Fatalf("expected a failing diff_scope gate result from the real RepositoryOwnerWorkflow path, got %+v", r.GateResults)
	}
	if len(r.Attempts) == 0 {
		t.Error("Attempts is empty, want per-attempt evidence from the RepositoryOwnerWorkflow path too")
	}
}

// TestIntegrationDaemonServicesRepositoryWithNoRunInvocation is the
// regression test for the plan's own "no genuine multi-run daemon" gap
// (see CLAIMS.md's Phase 6 entry): every prior -repository path only ever
// had a Worker polling a repository's shared task queue for as long as
// some `factoryd -repository ...` run invocation was itself in flight —
// a repository with none running had nothing servicing it at all. This
// starts a real `factoryd daemon` process for a repository, then submits
// a run against that repository's RepositoryOwnerWorkflow directly via a
// raw Temporal client with NO worker of its own anywhere in this test
// process — the only thing that can possibly execute the resulting
// RunWorkflow/RunBuildActivity/etc. is the daemon subprocess's own
// long-lived Worker, proving it services the queue independent of any
// run invocation ever existing.
// TestIntegrationDaemonWritesHeartbeat proves a real `factoryd daemon`
// subprocess writes a durable liveness heartbeat (internal/daemonheartbeat)
// an external supervisor could poll — closing part of the plan's own named
// Phase 6 gap ("no supervisor, health-check endpoint, or internal/api-driven
// lifecycle management yet"). No run is ever submitted; this only proves
// the daemon's own background heartbeat loop runs and is immediately fresh.
func TestIntegrationDaemonWritesHeartbeat(t *testing.T) {
	t.Parallel()
	address := sharedTemporalAddress(t)

	daemonDataDir := t.TempDir()
	repository := fmt.Sprintf("fixture/repo-heartbeat-%d", time.Now().UnixNano())
	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	heartbeatPath := daemonheartbeat.Path(daemonDataDir, workflow.RepositoryOwnerWorkflowID(repository))
	deadline := time.Now().Add(5 * time.Second)
	var hb daemonheartbeat.Heartbeat
	var readErr error
	for {
		hb, readErr = daemonheartbeat.Read(heartbeatPath)
		if readErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat file never appeared at %s: %v (daemon output:\n%s)", heartbeatPath, readErr, daemonOutput.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	if hb.Repository != repository {
		t.Errorf("hb.Repository = %q, want %q", hb.Repository, repository)
	}
	if hb.PID != daemonCmd.Process.Pid {
		t.Errorf("hb.PID = %d, want %d", hb.PID, daemonCmd.Process.Pid)
	}
	if hb.StartedAt == "" || hb.UpdatedAt == "" {
		t.Errorf("hb = %+v, want non-empty StartedAt/UpdatedAt", hb)
	}
	if daemonheartbeat.Stale(hb, time.Now(), 10*time.Second) {
		t.Errorf("heartbeat reported stale immediately after being written: %+v", hb)
	}
}

// TestIntegrationDaemonHeartbeatsAreIsolatedPerRepository is the
// regression test for a real P1 finding from codex review: an earlier
// version wrote one fixed heartbeat filename per data directory, so the
// supported one-daemon-per-repository deployment — multiple `factoryd
// daemon` processes sharing the same `-data-dir` — had every daemon
// overwrite the same file. Starts two real daemon subprocesses for two
// different repositories against one shared data directory and proves
// each produces its own distinct, independently fresh heartbeat.
func TestIntegrationDaemonHeartbeatsAreIsolatedPerRepository(t *testing.T) {
	t.Parallel()
	address := sharedTemporalAddress(t)

	sharedDataDir := t.TempDir()
	unique := time.Now().UnixNano()
	repoA := fmt.Sprintf("fixture/repo-heartbeat-a-%d", unique)
	repoB := fmt.Sprintf("fixture/repo-heartbeat-b-%d", unique)

	startDaemon := func(repository string) *exec.Cmd {
		cmd := factorydCommand(t, "daemon",
			"-temporal-address", address,
			"-repository", repository,
			"-data-dir", sharedDataDir,
		)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatalf("start factoryd daemon for %q: %v", repository, err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
			t.Logf("daemon output (%s):\n%s", repository, output.String())
		})
		return cmd
	}

	daemonA := startDaemon(repoA)
	daemonB := startDaemon(repoB)

	readHeartbeat := func(repository string, cmd *exec.Cmd) daemonheartbeat.Heartbeat {
		t.Helper()
		path := daemonheartbeat.Path(sharedDataDir, workflow.RepositoryOwnerWorkflowID(repository))
		deadline := time.Now().Add(5 * time.Second)
		for {
			hb, err := daemonheartbeat.Read(path)
			if err == nil {
				return hb
			}
			if time.Now().After(deadline) {
				t.Fatalf("heartbeat file for %q never appeared at %s: %v", repository, path, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	hbA := readHeartbeat(repoA, daemonA)
	hbB := readHeartbeat(repoB, daemonB)

	if hbA.Repository != repoA || hbA.PID != daemonA.Process.Pid {
		t.Errorf("hbA = %+v, want Repository=%q PID=%d", hbA, repoA, daemonA.Process.Pid)
	}
	if hbB.Repository != repoB || hbB.PID != daemonB.Process.Pid {
		t.Errorf("hbB = %+v, want Repository=%q PID=%d", hbB, repoB, daemonB.Process.Pid)
	}
	if daemonheartbeat.Stale(hbA, time.Now(), 10*time.Second) || daemonheartbeat.Stale(hbB, time.Now(), 10*time.Second) {
		t.Errorf("one or both heartbeats reported stale immediately after being written: hbA=%+v hbB=%+v", hbA, hbB)
	}
}

func TestIntegrationDaemonServicesRepositoryWithNoRunInvocation(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	daemonDataDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	requestID := fmt.Sprintf("fixture-daemon-run-%d", time.Now().UnixNano())
	logDir := filepath.Join(t.TempDir(), "logs")
	checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		t.Fatalf("create checkpoint dir: %v", err)
	}
	runInput := workflow.RunWorkflowInput{
		Ticket:              "fixture-ticket",
		WorkspacePath:       ws,
		IsolateWorkspace:    true,
		IsolatedRepoDir:     ws,
		IsolatedParentDir:   t.TempDir(),
		SpecPath:            specPath,
		BaseSHA:             baseSHA,
		LogDir:              logDir,
		CheckpointDir:       checkpointDir,
		SandboxImage:        fakeSandboxImage,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildMaxAttempts:    1,
		VerifyCommand:       "true",
		VerifyMaxAttempts:   1,
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	}
	// This test process registers no Worker at all — SignalWithStartWorkflow
	// only ever creates/signals the Workflow Execution server-side; the
	// only Worker that can ever pick up and execute it is the daemon
	// subprocess's own, on the task queue it's servicing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestID, Input: runInput},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel()
		t.Fatalf("signal repository owner workflow: %v", err)
	}
	cancel()

	pollCtx, cancelPoll := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelPoll()
	pollRun := &run.Run{ID: requestID}
	result, _, err := pollRepositoryOwnerResult(pollCtx, temporalClient, ownerID, requestID, "", pollRun, daemonDataDir, taskQueue+"-run-"+requestID, address, repository)
	if err != nil {
		t.Fatalf("poll repository owner result: %v (daemon output so far:\n%s)", err, daemonOutput.String())
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — the daemon's own Worker must have executed this run with no run invocation of its own ever existing", result.State, run.StateAccepted)
	}
}

// TestIntegrationReconcileReclaimedRunRecoversCrashedSubmitter is the
// regression test for a real P2 finding from codex review: when the
// submitting process crashes outright, nothing else ever calls
// applyRunWorkflowResult on its behalf, so run.json stays stuck at its
// last pre-crash nonterminal state forever even after the repository owner
// and a recovery Worker finish the run. This simulates exactly that crash —
// a run.json is seeded at StateSliceRunning (its "initial durable record
// exists" point) and then never touched again by anything this test
// controls — and proves reconcileReclaimedRun alone, querying only the
// real repository owner (serviced by a real `factoryd daemon` subprocess,
// with no submitting invocation of its own ever running), brings it to a
// terminal, correct state.
func TestIntegrationReconcileReclaimedRunRecoversCrashedSubmitter(t *testing.T) {
	// not parallel-safe: newFixtureRepo (below) calls t.Setenv, which
	// panics if the test also calls t.Parallel.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	daemonDataDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-reconcile-%d", time.Now().UnixNano())

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	requestID := fmt.Sprintf("fixture-crashed-run-%d", time.Now().UnixNano())
	logDir := filepath.Join(t.TempDir(), "logs")
	checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		t.Fatalf("create checkpoint dir: %v", err)
	}
	runInput := workflow.RunWorkflowInput{
		Ticket:              "fixture-crashed-ticket",
		WorkspacePath:       ws,
		IsolateWorkspace:    true,
		IsolatedRepoDir:     ws,
		IsolatedParentDir:   t.TempDir(),
		SpecPath:            specPath,
		BaseSHA:             baseSHA,
		LogDir:              logDir,
		CheckpointDir:       checkpointDir,
		SandboxImage:        fakeSandboxImage,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildMaxAttempts:    1,
		VerifyCommand:       "true",
		VerifyMaxAttempts:   1,
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	}

	// The submitter's own dataDir — deliberately separate from the
	// daemon's — with the same "initial durable record exists" run.json a
	// real submitter writes right before it starts waiting, and then
	// (simulating the crash) never touches again.
	submitterDataDir := t.TempDir()
	seeded := &run.Run{
		ID:            requestID,
		Ticket:        runInput.Ticket,
		WorkspacePath: ws,
		SpecPath:      specPath,
		Repository:    repository,
		State:         run.StateSliceRunning,
		BaseSHA:       baseSHA,
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(submitterDataDir); err != nil {
		t.Fatalf("seed crashed-submitter run.json: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestID, Input: runInput},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel()
		t.Fatalf("signal repository owner workflow: %v", err)
	}
	cancel()

	// No submitter of this test's own ever polls or writes run.json again
	// from here — only reconcileReclaimedRun, exactly as daemonMain's own
	// periodic reclaim scan would call it, standing in for the crashed
	// submitter's process.
	deadline := time.Now().Add(30 * time.Second)
	var terminal bool
	for time.Now().Before(deadline) {
		terminal, err = reconcileReclaimedRun(context.Background(), temporalClient, ownerID, requestID, submitterDataDir)
		if err != nil {
			t.Fatalf("reconcileReclaimedRun: %v (daemon output so far:\n%s)", err, daemonOutput.String())
		}
		if terminal {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !terminal {
		t.Fatalf("reconcileReclaimedRun never reported this request terminal within the deadline (daemon output:\n%s)", daemonOutput.String())
	}

	reconciled, err := run.Load(submitterDataDir, requestID)
	if err != nil {
		t.Fatalf("load reconciled run.json: %v", err)
	}
	if reconciled.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — reconcileReclaimedRun must bring a crashed submitter's run.json to the owner's own real result, not leave it at %q", reconciled.State, run.StateAccepted, run.StateSliceRunning)
	}
}

// TestIntegrationDaemonReclaimHoldsRepositoryOwnershipAfterSubmitterCrash
// proves the daemon's recovery Worker cannot execute a crashed repository run
// without the same Git-common-dir ownership a live submission would hold. A
// durable running record and delivered child stand in for a submitter killed
// immediately afterward; the daemon adopts that request, and a concurrent
// direct invocation is rejected until the recovered child finishes and its
// lock is released.
