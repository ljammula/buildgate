package requestdriver_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/unresolved"
)

// The provider's retry time every quota wait below was given, and the
// moments around it.
var (
	quotaRefusedAt = time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)
	quotaRetryAt   = time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	quotaDue       = quotaRetryAt.Add(request.AutoResumeMargin)
)

// autoResumeConfig is a worker config whose resume checks find no live
// container of a drafting job.
func autoResumeConfig() requestdriver.WorkerConfig {
	return requestdriver.WorkerConfig{Resume: requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{}}}
}

// autoResume runs the worker's check for id at now and returns what it said
// and the request after it.
func autoResume(t *testing.T, dataDir, id string, cfg requestdriver.WorkerConfig, now time.Time) (bool, *request.Request) {
	t.Helper()
	resumed, err := requestdriver.AutoResumeIfDue(context.Background(), dataDir, id, cfg, now)
	if err != nil {
		t.Fatalf("AutoResumeIfDue: %v", err)
	}
	return resumed, requestdrivertest.LoadRequest(t, dataDir, id)
}

// meetQuotaAgain moves id, back in its step, to resume_review with another
// provider-quota refusal, as the driver does when the step's job is refused.
func meetQuotaAgain(t *testing.T, dataDir, id string, retryAt, at time.Time) {
	t.Helper()
	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	step := request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: retryAt.Format(time.RFC3339), Routes: []string{"codex"}}
	if err := requestdriver.EnterResumeReviewHoldingLock(dataDir, r, step, at); err != nil {
		t.Fatal(err)
	}
}

func TestAutoResumeResumesAProviderQuotaWaitOnceItsRetryTimeHasPassed(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	waiting := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if due, ok := waiting.AutoResumeDue(); !ok || !due.Equal(quotaDue) {
		t.Fatalf("AutoResumeDue = %v, %t, want %v", due, ok, quotaDue)
	}
	if !strings.Contains(waiting.NextAction(), "the factory resumes it by itself, once, at "+quotaDue.Format(time.RFC3339)) {
		t.Errorf("NextAction = %q, want it to say when the factory resumes it", waiting.NextAction())
	}

	resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Second))
	if !resumed || r.State != request.StateSpecDrafting || r.PendingResumeVerb() != request.ResumeRound {
		t.Fatalf("resumed = %t, state %s, verb %q; want the spec_drafting step resumed with %q", resumed, r.State, r.PendingResumeVerb(), request.ResumeRound)
	}
	auto := r.Resume.AutoResumed
	if auto == nil || auto.RetryAt != quotaRetryAt.Format(time.RFC3339) || auto.At != r.ResumeDecision.At || auto.FromState != request.StateSpecDrafting {
		t.Fatalf("AutoResumed = %+v, want the resume recorded with the retry time it waited for", auto)
	}
	last := r.History[len(r.History)-1]
	if last.From != request.StateResumeReview || last.By != "factory" || !strings.Contains(last.Reason, "automatic, after the provider's limit reset at "+auto.RetryAt) {
		t.Errorf("History ends with %+v, want the factory's automatic resume", last)
	}
	if r.EnteredByAutoResume() == nil {
		t.Error("EnteredByAutoResume is nil in the step the factory resumed")
	}
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil || !strings.Contains(string(log), "Resumed automatically after the provider") {
		t.Errorf("notifications.log = %q, %v; want the automatic resume announced", log, err)
	}
}

func TestAutoResumeWaitsForTheRetryTimeAndItsMargin(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	for _, now := range []time.Time{quotaRefusedAt, quotaRetryAt, quotaDue.Add(-time.Second)} {
		if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), now); resumed || r.State != request.StateResumeReview || r.Resume.AutoResumed != nil {
			t.Fatalf("at %v: resumed = %t, state %s, AutoResumed %+v; want it still waiting, nothing spent", now, resumed, r.State, r.Resume.AutoResumed)
		}
	}
}

func TestAutoResumeNeedsTheMetersRetryTime(t *testing.T) {
	for _, retryAt := range []string{"", "3:11 AM by the worker container's clock"} {
		dataDir := t.TempDir()
		requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, retryAt, quotaRefusedAt)
		resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(24*time.Hour))
		if resumed || r.State != request.StateResumeReview {
			t.Errorf("retry time %q: resumed = %t, state %s; want the wait left to the operator", retryAt, resumed, r.State)
		}
		if strings.Contains(r.NextAction(), "the factory resumes it") {
			t.Errorf("retry time %q: NextAction = %q promises an automatic resume", retryAt, r.NextAction())
		}
	}
}

func TestAutoResumeLeavesEveryOtherCauseToTheOperator(t *testing.T) {
	for _, cause := range []unresolved.Cause{unresolved.ModelUnavailable, unresolved.MeterDenied, unresolved.RouteCredential, unresolved.NoVerdict, unresolved.WorkerLost, unresolved.SandboxMemory, unresolved.SandboxDisk} {
		dataDir := t.TempDir()
		requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecDrafting)
		r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
		if err := r.EnterResumeReview(request.StateSpecDrafting, request.LostStep{Cause: cause, RetryAt: quotaRetryAt.Format(time.RFC3339)}, quotaRefusedAt); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if resumed, after := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Hour)); resumed || after.State != request.StateResumeReview {
			t.Errorf("cause %s: resumed = %t, state %s; want it left to the operator", cause, resumed, after.State)
		}
	}
}

func TestAutoResumeLeavesAParkOrARefusedResumeToTheOperator(t *testing.T) {
	cases := map[string]func(r *request.Request){
		"parked":  func(r *request.Request) { r.Resume.Reason = "ticket 1's pull request was closed without merging" },
		"refused": func(r *request.Request) { r.Resume.Refused = []string{"a container is still alive"} },
	}
	for name, mutate := range cases {
		dataDir := t.TempDir()
		r := requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
		mutate(r)
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if resumed, after := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Hour)); resumed || after.State != request.StateResumeReview {
			t.Errorf("%s: resumed = %t, state %s; want it left to the operator", name, resumed, after.State)
		}
	}
}

// An operator who reopened the wait after its gate expired has acted on it
// since the refusal: the factory leaves it to them.
func TestAutoResumeLeavesAWaitTheOperatorActedOn(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	if _, expired, err := request.ExpireIfDue(dataDir, "req-1", time.Minute, quotaRefusedAt.Add(time.Hour)); err != nil || !expired {
		t.Fatalf("ExpireIfDue = %t, %v", expired, err)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if err := r.ReopenReview("alice", "", quotaRefusedAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if resumed, after := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Hour)); resumed || after.State != request.StateResumeReview {
		t.Fatalf("resumed = %t, state %s; want the reopened wait left to the operator", resumed, after.State)
	}
}

// One automatic resume per refusal episode: a step the factory resumed that
// the provider refuses again waits for the operator, and only their resume
// starts a new episode.
func TestAutoResumeIsSpentUntilTheOperatorResumes(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	if resumed, _ := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue); !resumed {
		t.Fatal("the first wait was not resumed")
	}
	if resumed, _ := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Minute)); resumed {
		t.Fatal("a request already resumed was resumed again")
	}

	secondRetry := quotaDue.Add(time.Hour)
	meetQuotaAgain(t, dataDir, "req-1", secondRetry, quotaDue.Add(time.Minute))
	again := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if again.Resume.AutoResumed == nil {
		t.Fatal("the second refusal of the resumed step lost the record of the spent automatic resume")
	}
	if _, ok := again.AutoResumeDue(); ok {
		t.Fatal("the second refusal of the resumed step is due an automatic resume")
	}
	if !strings.Contains(again.Error, "already resumed it once by itself") || !strings.Contains(again.NextAction(), "so it waits for you") {
		t.Errorf("Error = %q, NextAction = %q; want both to say the automatic resume is spent", again.Error, again.NextAction())
	}
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), secondRetry.Add(time.Hour)); resumed || r.State != request.StateResumeReview {
		t.Fatalf("resumed = %t, state %s; want the second refusal to wait for the operator", resumed, r.State)
	}

	if _, err := request.ResumeRequest(dataDir, "req-1", request.ResumeRound, "alice", secondRetry.Add(2*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	thirdRetry := secondRetry.Add(3 * time.Hour)
	meetQuotaAgain(t, dataDir, "req-1", thirdRetry, secondRetry.Add(2*time.Hour+time.Minute))
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), thirdRetry.Add(request.AutoResumeMargin)); !resumed || r.State != request.StateSpecDrafting {
		t.Fatalf("after the operator's resume: resumed = %t, state %s; want a new episode resumed by the factory", resumed, r.State)
	}
}

// A refusal of the resume's own checks spends it: the request waits for the
// operator, is told why, and is not tried again.
func TestAutoResumeRefusedByItsChecksIsSpent(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	cfg := requestdriver.WorkerConfig{Resume: requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{IDs: []string{"abc123"}}}}
	resumed, r := autoResume(t, dataDir, "req-1", cfg, quotaDue)
	if resumed || r.State != request.StateResumeReview {
		t.Fatalf("resumed = %t, state %s; want the refused resume to leave it waiting", resumed, r.State)
	}
	if r.Resume.AutoResumed == nil || len(r.Resume.AutoResumed.Refused) == 0 || !strings.Contains(r.NextAction(), "abc123") {
		t.Fatalf("AutoResumed = %+v, NextAction = %q; want the refusal recorded and named", r.Resume.AutoResumed, r.NextAction())
	}
	if strings.Contains(r.Error, "the factory resumes it by itself") {
		t.Errorf("Error = %q still promises an automatic resume", r.Error)
	}
	if resumed, _ := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Hour)); resumed {
		t.Fatal("a spent automatic resume was made again")
	}
}

// A lost build that kept nothing is offered only a rebuild, so that is the
// factory's resume of it.
func TestAutoResumeRebuildsALostBuildThatKeptNothing(t *testing.T) {
	dataDir, id := requestdrivertest.BuildingFixture(newFakeDeps(t), t, 2)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: quotaRetryAt.Format(time.RFC3339)}, quotaRefusedAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	resumed, after := autoResume(t, dataDir, id, autoResumeConfig(), quotaDue)
	if !resumed || after.State != request.StateBuilding || after.PendingResumeVerb() != request.ResumeScratch {
		t.Fatalf("resumed = %t, state %s, verb %q; want the ticket rebuilt (%q)", resumed, after.State, after.PendingResumeVerb(), request.ResumeScratch)
	}
}
