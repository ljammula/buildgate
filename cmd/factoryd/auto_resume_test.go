package main

import (
	"context"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
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
	if r.State != request.StateSpecDrafting || r.Resume.AutoResumed == nil {
		t.Fatalf("state %s, AutoResumed %+v; want the step resumed by the factory", r.State, r.Resume.AutoResumed)
	}
	entry := buildRequestStatusEntry(r, nil, time.Now())
	if want := "  [resumed automatically after the provider's limit reset at " + retryAt.Format(time.RFC3339) + "]"; requestRowSuffix(entry) != want {
		t.Errorf("status row suffix = %q, want %q", requestRowSuffix(entry), want)
	}
}
