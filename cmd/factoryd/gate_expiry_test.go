package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// reviewWaitingSince saves a request at state that entered it waited ago.
func reviewWaitingSince(t *testing.T, dataDir string, state request.State, waited time.Duration) {
	t.Helper()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", state)
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	r.EnteredAt = time.Now().Add(-waited).UTC().Format(time.RFC3339Nano)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

func TestReviewStepWaitIsCappedByExpiryAndNeverZero(t *testing.T) {
	now := time.Now()
	at := func(waited time.Duration) *request.Request {
		return &request.Request{ID: "req-1", State: request.StateSpecReview, EnteredAt: now.Add(-waited).UTC().Format(time.RFC3339Nano)}
	}
	cases := []struct {
		name             string
		r                *request.Request
		reminder, expiry time.Duration
		want             time.Duration
	}{
		{"expiry off: the reminder interval", at(time.Hour), 15 * time.Minute, 0, 15 * time.Minute},
		{"far from expiry: the reminder interval", at(time.Minute), 15 * time.Minute, 24 * time.Hour, 15 * time.Minute},
		{"expiry sooner than the reminder: the time left", at(55 * time.Minute), 15 * time.Minute, time.Hour, 5 * time.Minute},
		{"past the deadline: never 0, which would wait for a signal only", at(2 * time.Hour), 15 * time.Minute, time.Hour, time.Second},
		{"a deadline a few ms away: floored at 1s", at(time.Hour - 10*time.Millisecond), 15 * time.Minute, time.Hour, time.Second},
		{"pr_review is not a gate: the reminder interval", &request.Request{ID: "r", State: request.StatePRReview, EnteredAt: now.Add(-9 * time.Hour).UTC().Format(time.RFC3339Nano)}, 15 * time.Minute, time.Hour, 15 * time.Minute},
	}
	for _, c := range cases {
		got := reviewWait(c.r, c.reminder, c.expiry, now)
		if d := got - c.want; d < -time.Second || d > time.Second {
			t.Errorf("%s: reviewWait = %v, want %v", c.name, got, c.want)
		}
		if got < time.Second {
			t.Errorf("%s: reviewWait = %v, below the floor", c.name, got)
		}
	}

	dataDir := t.TempDir()
	reviewWaitingSince(t, dataDir, request.StatePlanReview, 50*time.Minute)
	cfg := requestdriver.WorkerConfig{HitlReminderInterval: time.Hour}
	cfg.Settings.ReviewGateExpiry = time.Hour
	acts := &requestActivities{dp: newTestDeps(t), dataDir: dataDir, cfg: cfg}
	step, err := acts.LoadRequestStep(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if !step.Remind || step.Wait < time.Second || step.Wait > 10*time.Minute+time.Second {
		t.Errorf("step = %+v, want a reminding wait cut to the 10 minutes left", step)
	}
}

func TestRemindRequestHaltsAnExpiredGateInsteadOfReminding(t *testing.T) {
	dataDir := t.TempDir()
	reviewWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
	cfg := requestdriver.WorkerConfig{HitlReminderInterval: time.Hour}
	cfg.Settings.ReviewGateExpiry = 2 * time.Hour
	acts := &requestActivities{dp: newTestDeps(t), dataDir: dataDir, cfg: cfg}
	if err := acts.RemindRequest(context.Background(), "req-1"); err != nil {
		t.Fatalf("RemindRequest: %v", err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if got.State != request.StateHalted || got.HaltKind != request.HaltGateExpired || got.NotifyCount != 0 {
		t.Errorf("after = state %q kind %q notified %d, want halted gate_expired and no reminder", got.State, got.HaltKind, got.NotifyCount)
	}
}

func TestApproveRefusedAfterExpiry(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	config := filepath.Join(t.TempDir(), "config.yml")
	writeSessionConfig(t, config, "review_gate_expiry: 2h\n")
	reviewWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)

	err := approveMain(dp, []string{"-config", config, "-data-dir", dataDir, "req-1"})
	if err == nil || !strings.Contains(err.Error(), "spec_review expired") || !strings.Contains(err.Error(), "factoryd retry req-1") {
		t.Fatalf("approve after expiry = %v, want a refusal naming the expiry and the retry", err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.State != request.StateHalted || got.HaltKind != request.HaltGateExpired {
		t.Errorf("after the refused approve: state %q kind %q, want halted gate_expired", got.State, got.HaltKind)
	}

	reviewWaitingSince(t, dataDir, request.StatePlanReview, 3*time.Hour)
	err = rejectMain(dp, []string{"-config", config, "-data-dir", dataDir, "-reason", "too late", "req-1"})
	if err == nil || !strings.Contains(err.Error(), "plan_review expired") {
		t.Errorf("reject after expiry = %v, want a refusal naming the expiry", err)
	}

	// The same request, inside the expiry, is decided as usual.
	reviewWaitingSince(t, dataDir, request.StateSpecReview, time.Hour)
	if err := approveMain(dp, []string{"-config", config, "-data-dir", dataDir, "req-1"}); err != nil {
		t.Errorf("approve inside the expiry: %v", err)
	}
}

func TestRetryReopensAnExpiredReviewFromTheCLI(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	reviewWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if err := r.HaltGateExpired(time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := retryMain(dp, []string{"-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.State != request.StateSpecReview || got.HaltKind != "" {
		t.Errorf("after retry: state %q kind %q, want spec_review", got.State, got.HaltKind)
	}
}
