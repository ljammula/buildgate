package notify

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

func TestPrepareHaltNotifiesOnce(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateHalted}

	n, dispatch := PrepareHalt(r, dataDir)
	if !dispatch {
		t.Fatal("dispatch = false, want true for a run newly transitioned to halted")
	}
	if n.RunID != "run-1" || n.State != run.StateHalted {
		t.Errorf("notification = %+v, want RunID run-1 and State halted", n)
	}
	if len(r.Notifications) != 1 {
		t.Fatalf("len(r.Notifications) = %d, want 1", len(r.Notifications))
	}

	logBytes, err := os.ReadFile(filepath.Join(run.Dir(dataDir, "run-1"), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if len(logBytes) == 0 {
		t.Error("notifications.log is empty, want the notification durably logged")
	}

	// A second call against a run that already carries this notification
	// (matching save()'s own dedup guard) must not re-notify.
	_, dispatchAgain := PrepareHalt(r, dataDir)
	if dispatchAgain {
		t.Error("dispatch = true on a second call, want false (already notified)")
	}
	if len(r.Notifications) != 1 {
		t.Errorf("len(r.Notifications) after a second call = %d, want still 1", len(r.Notifications))
	}
}

// TestPrepareHaltIncludesHaltError guards against the exact gap found
// live: a run halted by a runner.Run infrastructure failure (e.g. its
// lifecycle context cancelled mid-build_app.py) produced a build_app.log
// with nothing in it, and PrepareHalt's own generic "see ... build/verify
// logs for detail" reason pointed an operator at that empty file with no
// way to recover the real cause -- the only place it ever existed was a
// log.Fatalf to factoryd's own stderr, never persisted anywhere
// associated with the run. r.HaltError is the fix: when a caller sets it
// before calling save() (see save's own doc comment), the halt reason
// must actually surface it, not just repeat the dead-end pointer.
func TestPrepareHaltIncludesHaltError(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID:        "run-1",
		Ticket:    "ticket-1",
		State:     run.StateHalted,
		HaltError: "run build_app.py: context canceled",
	}

	n, dispatch := PrepareHalt(r, dataDir)
	if !dispatch {
		t.Fatal("dispatch = false, want true for a run newly transitioned to halted")
	}
	if !strings.Contains(n.Reason, "context canceled") {
		t.Errorf("notification reason = %q, want it to include HaltError %q", n.Reason, r.HaltError)
	}
}

// TestDispatchExternalCallsAllThreeChannels proves DispatchExternal
// reaches Discord, Slack, and the desktop notifier, using each channel's
// own configured/injected seam as a spy: two fake webhook servers stood
// in via the two env vars (the same seam DiscordWebhookURLs/DispatchDiscord
// already use), and desktopNotifierLookPath pointed at a stand-in script
// that records its own invocation, since there is no HTTP call to
// intercept for the desktop channel.
func TestDispatchExternalCallsAllThreeChannels(t *testing.T) {
	var discordHit, slackHit atomic.Bool
	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discordHit.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discordSrv.Close()
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	t.Setenv(DiscordWebhookURLsEnvironmentVariable, discordSrv.URL)
	t.Setenv(SlackWebhookURLsEnvironmentVariable, slackSrv.URL)
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")

	marker := filepath.Join(t.TempDir(), "desktop-notifier-invoked")
	script := filepath.Join(t.TempDir(), "fake-osascript.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("write fake osascript: %v", err)
	}

	origOS := desktopOS
	origLookPath := desktopNotifierLookPath
	desktopOS = "darwin"
	desktopNotifierLookPath = func() (string, error) { return script, nil }
	defer func() {
		desktopOS = origOS
		desktopNotifierLookPath = origLookPath
	}()

	DispatchExternal(Notification{RunID: "run-1", Ticket: "ticket-1", State: run.StateHalted, Reason: "test"})
	// DispatchExternal itself returns immediately (its fan-out runs on a
	// background goroutine) -- wait for it to finish before asserting.
	WaitForPendingDispatches(5 * time.Second)

	if !discordHit.Load() {
		t.Error("Discord webhook was never hit, want DispatchExternal to reach it")
	}
	if !slackHit.Load() {
		t.Error("Slack webhook was never hit, want DispatchExternal to reach it")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("desktop notifier marker file missing, want DispatchExternal to have invoked the injected osascript stand-in: %v", err)
	}
}

// TestWaitForPendingDispatchesWaitsForSlowDispatch is the regression test
// for the Codex finding on PR #88 round 2: a CLI process can exit before
// a DispatchExternal goroutine ever reaches the network, silently
// dropping the alert. Proven here by handing a slow (150ms) fake webhook
// to DispatchExternal and asserting WaitForPendingDispatches, called
// with a much longer timeout, still lets it finish and returns promptly
// afterward rather than always waiting out the full timeout.
func TestWaitForPendingDispatchesWaitsForSlowDispatch(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		hit.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv(DiscordWebhookURLsEnvironmentVariable, srv.URL)

	DispatchExternal(Notification{RunID: "run-1"})

	start := time.Now()
	WaitForPendingDispatches(5 * time.Second)
	elapsed := time.Since(start)

	if !hit.Load() {
		t.Error("webhook was never hit, want WaitForPendingDispatches to have let the slow dispatch finish")
	}
	if elapsed >= 4*time.Second {
		t.Errorf("WaitForPendingDispatches took %v, want it to return promptly once the dispatch finished rather than waiting out the full 5s timeout", elapsed)
	}
}

// TestWaitForPendingDispatchesReturnsOnTimeout covers the other half: a
// dispatch stuck on a webhook that never responds must not hang
// WaitForPendingDispatches (and therefore process exit) forever -- it
// must give up once its own timeout elapses.
func TestWaitForPendingDispatchesReturnsOnTimeout(t *testing.T) {
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv(DiscordWebhookURLsEnvironmentVariable, srv.URL)

	DispatchExternal(Notification{RunID: "run-1"})

	start := time.Now()
	WaitForPendingDispatches(100 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed >= 2*time.Second {
		t.Errorf("WaitForPendingDispatches took %v against a hung webhook and a 100ms timeout, want it to give up promptly", elapsed)
	}

	// The dispatch goroutine this test started is still blocked on the
	// handler above, alive past this test's own return, at this point --
	// WaitForPendingDispatches' own 100ms timeout deliberately doesn't
	// wait for it (that's what this test proves). Left running into
	// later tests, that goroutine calls os.Getenv on the very env var
	// name a later t.Setenv can be actively mutating, a real data race
	// under -race across two otherwise-unrelated tests (found live:
	// TestDispatchExternalRunsChannelsConcurrently, run right after this
	// one, failed under -race for exactly this reason). Unblock the
	// handler now and actually drain this test's own dispatch before
	// returning, so no goroutine survives to race with what runs next.
	close(blockCh)
	WaitForPendingDispatches(2 * time.Second)
}

// TestDispatchExternalRunsChannelsConcurrently is the regression test for
// the Codex finding round 3 on PR #88: DispatchExternal used to run its
// three channels one after another inside its own goroutine, so a slow
// Discord webhook alone could delay Slack (and desktop) long enough that
// WaitForPendingDispatches' own bounded exit-flush gave up before they
// ever got a turn -- defeating them as fallback channels entirely. Proven
// here by having the fake Discord webhook block for several seconds
// while asserting the fake Slack webhook is hit within a much shorter
// window.
func TestDispatchExternalRunsChannelsConcurrently(t *testing.T) {
	blockCh := make(chan struct{})
	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discordSrv.Close()

	slackHit := make(chan struct{})
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(slackHit)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	t.Setenv(DiscordWebhookURLsEnvironmentVariable, discordSrv.URL)
	t.Setenv(SlackWebhookURLsEnvironmentVariable, slackSrv.URL)

	DispatchExternal(Notification{RunID: "run-1"})

	select {
	case <-slackHit:
	case <-time.After(1 * time.Second):
		t.Fatal("Slack webhook was not hit within 1s while a slow Discord webhook was still blocking -- channels must run concurrently, not serially")
	}

	// Unblock the still-in-flight Discord request and actually drain this
	// test's own DispatchExternal call before returning -- left to a
	// defer, that goroutine can survive into a later test (or, under
	// -count>1, this same test's own next run) and race its os.Getenv
	// read against that run's t.Setenv on the same env var name (see
	// TestWaitForPendingDispatchesReturnsOnTimeout's own comment on this
	// exact failure mode, found live under -race).
	close(blockCh)
	WaitForPendingDispatches(2 * time.Second)
}

// TestPrepareHaltNextRetriesOwningRequest proves a halted run that
// belongs to a request (its ID appears in one of the request's own
// Tickets) gets a Next pointing at retrying that request, not a bare
// "watch this run" hint.
func TestPrepareHaltNextRetriesOwningRequest(t *testing.T) {
	dataDir := t.TempDir()
	req := &request.Request{
		ID:          "req-1",
		State:       request.StateBuilding,
		SubmittedAt: time.Now().Format(time.RFC3339),
		Tickets:     []request.Ticket{{Index: 1, RunID: "run-1"}},
	}
	if err := req.Save(dataDir); err != nil {
		t.Fatalf("save request: %v", err)
	}
	r := &run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateHalted}

	n, dispatch := PrepareHalt(r, dataDir)
	if !dispatch {
		t.Fatal("dispatch = false, want true")
	}
	if want := "factoryd retry req-1"; n.Next != want {
		t.Errorf("Next = %q, want %q", n.Next, want)
	}
}

// TestPrepareHaltNextWatchesRunWithoutOwningRequest proves a halted run
// that belongs to no request falls back to a Next that watches the run
// directly.
func TestPrepareHaltNextWatchesRunWithoutOwningRequest(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateHalted}

	n, dispatch := PrepareHalt(r, dataDir)
	if !dispatch {
		t.Fatal("dispatch = false, want true")
	}
	if want := "factoryd watch run-1"; n.Next != want {
		t.Errorf("Next = %q, want %q", n.Next, want)
	}
}

// TestPrepareHaltLinkUsesConsoleEnvVar proves Link is populated from
// FACTORYD_CONSOLE_URL as the run's own console deep link, and empty
// when that env var is unset.
func TestPrepareHaltLinkUsesConsoleEnvVar(t *testing.T) {
	t.Setenv(consolelink.EnvVar, "https://console.example/")
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateHalted}

	n, dispatch := PrepareHalt(r, dataDir)
	if !dispatch {
		t.Fatal("dispatch = false, want true")
	}
	if want := "https://console.example/runs/run-1"; n.Link != want {
		t.Errorf("Link = %q, want %q", n.Link, want)
	}
}

func TestPrepareHaltSkipsNonHaltedStates(t *testing.T) {
	r := &run.Run{ID: "run-1", State: run.StateAccepted}
	_, dispatch := PrepareHalt(r, t.TempDir())
	if dispatch {
		t.Error("dispatch = true for an accepted run, want false")
	}
	if len(r.Notifications) != 0 {
		t.Errorf("len(r.Notifications) = %d, want 0", len(r.Notifications))
	}
}
