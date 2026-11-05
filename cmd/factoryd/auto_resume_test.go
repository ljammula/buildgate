package main

import (
	"context"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/unresolved"
)

// A provider-quota wait wakes its workflow when the factory resumes it, not
// a reminder interval later.
func TestReviewWaitWakesAProviderQuotaWaitWhenItsAutomaticResumeIsDue(t *testing.T) {
	now := time.Now()
	dataDir := t.TempDir()
	retryAt := now.Add(10 * time.Minute).UTC().Truncate(time.Second)
	r := requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, retryAt.Format(time.RFC3339), now.Add(-time.Hour))
	want := retryAt.Add(request.AutoResumeMargin).Sub(now)
	if got := reviewWait(r, time.Hour, 0, now); got-want > time.Second || want-got > time.Second {
		t.Errorf("reviewWait = %v, want %v: the time left before the automatic resume", got, want)
	}
	if got := reviewWait(r, time.Hour, 0, retryAt.Add(time.Hour)); got != minReviewWait {
		t.Errorf("past the resume time: reviewWait = %v, want %v", got, minReviewWait)
	}
}

// The worker's poll of a waiting request makes the automatic resume.
func TestRemindRequestResumesADueProviderQuotaWait(t *testing.T) {
	now := time.Now()
	dataDir := t.TempDir()
	retryAt := now.Add(-2 * request.AutoResumeMargin).UTC().Truncate(time.Second)
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, retryAt.Format(time.RFC3339), now.Add(-time.Hour))
	cfg := requestdriver.WorkerConfig{HitlReminderInterval: time.Hour, Resume: requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{}}}
	acts := &requestActivities{dp: newTestDeps(t), dataDir: dataDir, cfg: cfg}
	if err := acts.RemindRequest(context.Background(), "req-1"); err != nil {
		t.Fatal(err)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if r.State != request.StateSpecDrafting || r.AutoResumed == nil {
		t.Fatalf("state %s, AutoResumed %+v; want the step resumed by the factory", r.State, r.AutoResumed)
	}
	entry := buildRequestStatusEntry(r, nil, time.Now())
	if want := "  [resumed automatically after the provider's limit reset at " + retryAt.Format(time.RFC3339) + "]"; requestRowSuffix(entry) != want {
		t.Errorf("status row suffix = %q, want %q", requestRowSuffix(entry), want)
	}
}

// The worker's poll of a request whose step never got to the work (here,
// its sandbox never started) resumes it once AutoRetryDelay has passed, and
// `factoryd status` names that resume.
func TestRemindRequestResumesADueTransientWait(t *testing.T) {
	now := time.Now()
	dataDir := t.TempDir()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecDrafting)
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if err := r.EnterResumeReview(request.StateSpecDrafting, request.LostStep{Cause: unresolved.SandboxLaunch}, now.Add(-2*request.AutoRetryDelay)); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if got := reviewWait(r, time.Hour, 0, now); got != minReviewWait {
		t.Errorf("past the retry delay: reviewWait = %v, want %v", got, minReviewWait)
	}
	cfg := requestdriver.WorkerConfig{HitlReminderInterval: time.Hour, Resume: requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{}}}
	acts := &requestActivities{dp: newTestDeps(t), dataDir: dataDir, cfg: cfg}
	if err := acts.RemindRequest(context.Background(), "req-1"); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if r.State != request.StateSpecDrafting || r.AutoResumed == nil || r.AutoResumed.Cause != unresolved.SandboxLaunch {
		t.Fatalf("state %s, AutoResumed %+v; want the step resumed by the factory", r.State, r.AutoResumed)
	}
	entry := buildRequestStatusEntry(r, nil, time.Now())
	if want := "  [resumed automatically 5m0s after the step ended with cause sandbox_launch]"; requestRowSuffix(entry) != want {
		t.Errorf("status row suffix = %q, want %q", requestRowSuffix(entry), want)
	}
}
