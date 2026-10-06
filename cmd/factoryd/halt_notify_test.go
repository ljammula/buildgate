package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

// TestSaveNotifiesOnNewHalt is the regression test for the 2026-09-05
// Opus review finding S2: this file sets r.State = run.StateHalted at 60
// call sites, all of them followed by a call into save, but before this
// existed none of them notified anybody -- a halt was the dominant stop
// mode under an unattended policy and it was entirely silent. save must
// alert exactly once when a run's state transitions to halted.
func TestSaveNotifiesOnNewHalt(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}

	if err := save(r, dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}

	if len(r.Notifications) != 1 {
		t.Fatalf("len(Notifications) = %d, want 1", len(r.Notifications))
	}
	if r.Notifications[0].State != run.StateHalted {
		t.Errorf("Notifications[0].State = %q, want %q", r.Notifications[0].State, run.StateHalted)
	}

	logBytes, err := os.ReadFile(filepath.Join(run.Dir(dataDir, r.ID), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if !strings.Contains(string(logBytes), "run-1") {
		t.Errorf("notifications.log = %q, want it to mention the run id", logBytes)
	}
}

// TestSaveRecordsHaltErrorFromCause is the regression test for the halt
// haltError gap found live: a run halted by build_app.py's own
// runner.RunWithRetries returning an infrastructure error left the
// run's durable record (and its notification) pointing only at
// "its build/verify logs for detail" -- a dead end in exactly this case,
// since runner.Run's own infrastructure-failure contract means no exit
// code, and therefore nothing useful, was ever written to that log. save's
// optional cause argument must land in r.HaltError and, through
// notify.PrepareHalt, in the notification's own reason -- the only two
// places this information can still reach an operator once the process
// that generated it has exited.
func TestSaveRecordsHaltErrorFromCause(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}
	causeErr := errors.New("run python3 build_app.py: context canceled")

	if err := save(r, dataDir, causeErr); err != nil {
		t.Fatalf("save: %v", err)
	}

	if r.HaltError != causeErr.Error() {
		t.Errorf("r.HaltError = %q, want %q", r.HaltError, causeErr.Error())
	}
	if len(r.Notifications) != 1 || !strings.Contains(r.Notifications[0].Reason, "context canceled") {
		t.Errorf("Notifications = %+v, want the sole notification's reason to include the halt error", r.Notifications)
	}

	logBytes, err := os.ReadFile(filepath.Join(run.Dir(dataDir, r.ID), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if !strings.Contains(string(logBytes), "context canceled") {
		t.Errorf("notifications.log = %q, want it to include the halt error", logBytes)
	}
}

// TestSaveDoesNotRenotifyOnRepeatedHaltedSave covers the dedup guard: a
// run that stays halted across multiple save calls (e.g. a later
// reconciliation pass only flipping HaltConfirmed) must alert once, not
// once per save.
func TestSaveDoesNotRenotifyOnRepeatedHaltedSave(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}

	if err := save(r, dataDir); err != nil {
		t.Fatalf("first save: %v", err)
	}
	r.HaltConfirmed = true
	if err := save(r, dataDir); err != nil {
		t.Fatalf("second save: %v", err)
	}

	if len(r.Notifications) != 1 {
		t.Fatalf("len(Notifications) = %d after two saves while halted, want 1 (no renotify)", len(r.Notifications))
	}
}

// TestSaveDoesNotNotifyNonHaltedStates covers the common case: a run
// saved in any state other than halted must not be affected by this
// hook at all.
func TestSaveDoesNotNotifyNonHaltedStates(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateAccepted}

	if err := save(r, dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(r.Notifications) != 0 {
		t.Fatalf("len(Notifications) = %d for an accepted run, want 0", len(r.Notifications))
	}
}

// TestSavePersistsHaltBeforeDispatchingDiscord is the regression test for
// a real GitHub Codex App review finding on this PR: the first version of
// this halt-alert hook dispatched Discord webhooks (each up to a 5s
// network block) before r.Save durably persisted the halt, reopening
// exactly the crash window the direct/Temporal quarantine junctions
// elsewhere in this file deliberately avoid by saving first. Proven here
// by having the fake webhook endpoint itself read run.json directly off
// disk (bypassing this process's own in-memory r) and assert the halted
// state is already there by the time the webhook fires.
//
// save() now dispatches notify.DispatchExternal on a goroutine (PR #88
// fix, so a run's own save never blocks on network I/O), so the webhook
// can fire after save() has already returned -- the handler's own result
// is handed back over hookDone and awaited with a timeout, rather than
// read from a shared variable straight after save() returns.
func TestSavePersistsHaltBeforeDispatchingDiscord(t *testing.T) {
	dataDir := t.TempDir()
	hookDone := make(chan run.State, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		persisted, err := run.Load(dataDir, "run-1")
		if err != nil {
			t.Errorf("load run.json from inside webhook handler: %v", err)
		} else {
			hookDone <- persisted.State
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	t.Setenv("FACTORYD_DISCORD_WEBHOOK_URLS", webhook.URL)

	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}
	if err := save(r, dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}

	select {
	case sawPersistedState := <-hookDone:
		if sawPersistedState != run.StateHalted {
			t.Errorf("run.json state as observed from inside the webhook handler = %q, want %q -- the halt must be durably persisted before any remote notification is dispatched", sawPersistedState, run.StateHalted)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was never called within 2s of save() returning")
	}
}

// TestSaveReturnsErrorWithoutDispatchingDiscordOnSaveFailure covers the
// other half of the same ordering fix: if r.Save itself fails, no
// Discord dispatch should have any run to report as durably halted, so
// dispatch must not have been attempted at all. Forced by pointing
// dataDir at a path that cannot be created (a file, not a directory).
func TestSaveReturnsErrorWithoutDispatchingDiscordOnSaveFailure(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	dispatched := false
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		dispatched = true
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	t.Setenv("FACTORYD_DISCORD_WEBHOOK_URLS", webhook.URL)

	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}
	if err := save(r, notADir); err == nil {
		t.Fatal("save() error = nil, want an error since dataDir cannot hold a run directory")
	}
	if dispatched {
		t.Error("Discord webhook was dispatched despite save() failing to persist the halt")
	}
}

// TestSaveReturnsWithoutWaitingOnSlowWebhook is the regression test for
// the Codex finding on PR #88: notify.Notifier's own doc comment requires
// network-I/O dispatch to happen asynchronously from run completion, but
// save() called notify.DispatchExternal inline -- a run's own save
// blocked on up to three serial 5s-timeout webhook calls. Guarded by a
// short deadline (run on a goroutine, same pattern as this file's other
// PR #86-era regression tests) so a reintroduced inline call hangs this
// test alone, not the whole suite, since the fake webhook below blocks
// far longer than that deadline and is never told to stop.
func TestSaveReturnsWithoutWaitingOnSlowWebhook(t *testing.T) {
	// blockCh must be closed (unblocking the handler) before webhook.Close()
	// runs, or Close blocks forever waiting for that in-flight handler --
	// deferred in this order so it runs first (defers are LIFO).
	blockCh := make(chan struct{})
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-blockCh
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	defer close(blockCh)
	t.Setenv("FACTORYD_DISCORD_WEBHOOK_URLS", webhook.URL)

	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}

	done := make(chan error, 1)
	go func() { done <- save(r, dataDir) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("save() did not return within 2s while a configured Discord webhook was hanging -- notify dispatch must not block a run's own save")
	}
}

// TestSaveRollsBackStagedNotificationOnSaveFailure is the regression test
// for a real GitHub Codex App review finding on this PR: notify.PrepareHalt
// appends the halt notification to r.Notifications before save's own
// r.Save call persists r. If that save then fails (e.g. a disk or
// permission error), the notification was staged in memory but never
// actually persisted -- a caller that reused this same in-memory r for a
// later save attempt would see the dedup guard's "already notified" check
// pass and silently skip re-notifying, even though nothing was ever
// durably recorded. save must roll the staged notification back out of
// r.Notifications when this happens.
//
// Forces the save failure by pre-creating the run directory and its
// notifications.log (so notify.PrepareHalt's own MkdirAll/append -- which
// only need the log *file's* own write permission, not the directory's --
// still succeed), then making the directory itself read-only so r.Save's
// new run.json.tmp file cannot be created in it.
func TestSaveRollsBackStagedNotificationOnSaveFailure(t *testing.T) {
	dataDir := t.TempDir()
	runDir := run.Dir(dataDir, "run-1")
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "notifications.log"), nil, 0o600); err != nil {
		t.Fatalf("pre-create notifications.log: %v", err)
	}
	if err := os.Chmod(runDir, 0o500); err != nil {
		t.Fatalf("chmod run dir read-only: %v", err)
	}
	defer func() {
		if err := os.Chmod(runDir, 0o750); err != nil {
			t.Fatalf("restore run dir permissions: %v", err)
		}
	}()

	r := &run.Run{ID: "run-1", Ticket: "fixture-ticket", State: run.StateHalted}
	if err := save(r, dataDir); err == nil {
		t.Fatal("save() error = nil, want an error since run.json cannot be created in a read-only directory")
	}
	if len(r.Notifications) != 0 {
		t.Errorf("Notifications = %+v, want empty -- a notification staged before a failed save must be rolled back", r.Notifications)
	}
}
