package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// A session config that cannot be read refuses the decision: the expiry is
// not guessed, and the request is left as it was.
func TestDecisionsRefusedWhenTheSessionConfigCannotBeRead(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	config := filepath.Join(t.TempDir(), "config.yml")
	writeSessionConfig(t, config, "review_gate_expiry: [not, a, duration\n")
	reviewWaitingSince(t, dataDir, request.StateSpecReview, time.Minute)

	err := approveMain(dp, []string{"-config", config, "-data-dir", dataDir, "req-1"})
	if err == nil || !strings.Contains(err.Error(), "review_gate_expiry is unknown") {
		t.Fatalf("approve = %v, want a refusal naming the unreadable config", err)
	}
	err = rejectMain(dp, []string{"-config", config, "-data-dir", dataDir, "-reason", "x", "req-1"})
	if err == nil || !strings.Contains(err.Error(), "review_gate_expiry is unknown") {
		t.Fatalf("reject = %v, want a refusal naming the unreadable config", err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.State != request.StateSpecReview {
		t.Errorf("state = %q, want the request untouched", got.State)
	}
}

// resume decides a gate too (resume_review), and a refused decision tells the
// operator once, whoever found the gate expired.
func TestResumeRefusedAtALapsedGateAndTheOperatorIsToldOnce(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	config := filepath.Join(t.TempDir(), "config.yml")
	writeSessionConfig(t, config, "review_gate_expiry: 2h\n")
	reviewWaitingSince(t, dataDir, request.StateResumeReview, 3*time.Hour)

	err := resumeMainWith(dp, []string{"-config", config, "-data-dir", dataDir, "-from", "scratch", "req-1"}, requestdriver.ResumeGate{})
	if err == nil || !strings.Contains(err.Error(), "resume_review expired") {
		t.Fatalf("resume = %v, want a refusal naming the expiry", err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if got.State != request.StateHalted || got.HaltKind != request.HaltGateExpired {
		t.Fatalf("state %q kind %q, want halted gate_expired", got.State, got.HaltKind)
	}
	logPath := requestdriver.RequestNotificationLogPath(dataDir, "req-1")
	first, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(first), "expired") {
		t.Fatalf("notifications.log = %q (%v), want the expiry told by the refused decision", first, err)
	}
	// Another finder (the worker's pass, the API) adds nothing.
	if err := requestdriver.NotifyGateExpired(dataDir, "req-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if expired, err := requestdriver.ExpireIfDue(dataDir, "req-1", 2*time.Hour, time.Now()); err != nil || expired {
		t.Errorf("ExpireIfDue on a halted request = %v, %v", expired, err)
	}
	if again, _ := os.ReadFile(logPath); string(again) != string(first) {
		t.Errorf("the operator was told twice:\n%s", again)
	}
}
