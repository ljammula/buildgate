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
	step := request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: retryAt.Format(time.RFC3339), ResetAt: retryAt.Format(time.RFC3339), Routes: []string{"codex"}}
	if err := requestdriver.EnterResumeReviewHoldingLock(dataDir, r, step, at); err != nil {
		t.Fatal(err)
	}
}

func TestAutoResumeResumesAProviderQuotaWaitOnceItsRetryTimeHasPassed(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	waiting := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if due, ok := waiting.AutoResumeDue(quotaRefusedAt); !ok || !due.Equal(quotaDue) {
		t.Fatalf("AutoResumeDue = %v, %t, want %v", due, ok, quotaDue)
	}
	if !strings.Contains(waiting.NextAction(), "the factory resumes it by itself, once, at "+quotaDue.Format(time.RFC3339)) {
		t.Errorf("NextAction = %q, want it to say when the factory resumes it", waiting.NextAction())
	}

	resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue.Add(time.Second))
	if !resumed || r.State != request.StateSpecDrafting || r.PendingResumeVerb() != request.ResumeRound {
		t.Fatalf("resumed = %t, state %s, verb %q; want the spec_drafting step resumed with %q", resumed, r.State, r.PendingResumeVerb(), request.ResumeRound)
	}
	auto := r.AutoResumed
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
		if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), now); resumed || r.State != request.StateResumeReview || r.AutoResumed != nil {
			t.Fatalf("at %v: resumed = %t, state %s, AutoResumed %+v; want it still waiting, nothing spent", now, resumed, r.State, r.AutoResumed)
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

// seedQuotaWaitWith saves a provider-quota wait entered at quotaRefusedAt
// with retryAt (shown, and read by the route hold) and resetAt (the reset of
// the refusal that ended the step).
func seedQuotaWaitWith(t *testing.T, dataDir, retryAt, resetAt string) {
	t.Helper()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecDrafting)
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if err := r.EnterResumeReview(request.StateSpecDrafting, request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: retryAt, ResetAt: resetAt}, quotaRefusedAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// A transient 429 with a short Retry-After, then the hard refusal with no
// time: the step's retry time is the transient's, already past, and the
// refusal that ended the step has no reset of its own. Manual.
func TestAutoResumeIgnoresTheTimeOfAnEarlierTransientRefusal(t *testing.T) {
	dataDir := t.TempDir()
	transient := quotaRefusedAt.Add(-time.Minute).Add(20 * time.Second).Format(time.RFC3339)
	seedQuotaWaitWith(t, dataDir, transient, "")
	resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaRefusedAt.Add(time.Hour))
	if resumed || r.State != request.StateResumeReview || r.AutoResumed != nil {
		t.Fatalf("resumed = %t, state %s, AutoResumed %+v; want the wait left to the operator", resumed, r.State, r.AutoResumed)
	}
	if !strings.Contains(r.Error, "try again after "+transient) {
		t.Errorf("Error = %q, want the transient's time still shown", r.Error)
	}
}

// A reset at or before the moment the request entered the wait is not the
// reset of the refusal that put it there. Manual.
func TestAutoResumeIgnoresAResetBeforeTheRefusal(t *testing.T) {
	for _, resetAt := range []time.Time{quotaRefusedAt.Add(-time.Minute), quotaRefusedAt} {
		dataDir := t.TempDir()
		seedQuotaWaitWith(t, dataDir, resetAt.Format(time.RFC3339), resetAt.Format(time.RFC3339))
		if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaRefusedAt.Add(time.Hour)); resumed || r.State != request.StateResumeReview {
			t.Errorf("reset %v: resumed = %t, state %s; want the wait left to the operator", resetAt, resumed, r.State)
		}
	}
}

func TestAutoResumeLeavesEveryOtherCauseToTheOperator(t *testing.T) {
	for _, cause := range []unresolved.Cause{unresolved.MeterDenied, unresolved.RouteCredential, unresolved.NoVerdict, unresolved.WorkerLost, unresolved.SandboxMemory, unresolved.SandboxDisk, unresolved.DraftingJob, unresolved.TemporalUnreachable, unresolved.ReproductionUnheld} {
		dataDir := t.TempDir()
		requestdrivertest.NewApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecDrafting)
		r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
		if err := r.EnterResumeReview(request.StateSpecDrafting, request.LostStep{Cause: cause, RetryAt: quotaRetryAt.Format(time.RFC3339), ResetAt: quotaRetryAt.Format(time.RFC3339)}, quotaRefusedAt); err != nil {
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

// One automatic resume per request, ever: a resumed step the provider
// refuses again waits for the operator, and so does every later
// provider-quota wait, after the operator's own resume too.
func TestAutoResumeIsOncePerRequestAndAnOperatorResumeDoesNotReArmIt(t *testing.T) {
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
	if _, ok := again.AutoResumeDue(quotaDue); ok || again.AutoResumed == nil {
		t.Fatalf("AutoResumed = %+v; want the second refusal not due, the record kept", again.AutoResumed)
	}
	if !strings.Contains(again.Error, "already made its one automatic resume of this request") || !strings.Contains(again.NextAction(), "so it waits for you") {
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
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), thirdRetry.Add(time.Hour)); resumed || r.State != request.StateResumeReview {
		t.Fatalf("after the operator's resume: resumed = %t, state %s; want it still waiting for the operator", resumed, r.State)
	}
}

// A provider-quota wait on a later ticket, after the automatic resume of an
// earlier one, waits for the operator.
func TestAutoResumeIsNotReArmedOnAnotherTicket(t *testing.T) {
	dataDir, id := requestdrivertest.BuildingFixture(newFakeDeps(t), t, 2)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.TicketIndex = 1
	reset := quotaRetryAt.Format(time.RFC3339)
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: reset, ResetAt: reset}, quotaRefusedAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if resumed, _ := autoResume(t, dataDir, id, autoResumeConfig(), quotaDue); !resumed {
		t.Fatal("ticket 1's wait was not resumed")
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	r.TicketIndex = 2
	later := quotaDue.Add(5 * time.Hour)
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: later.Format(time.RFC3339), ResetAt: later.Format(time.RFC3339)}, quotaDue.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if resumed, after := autoResume(t, dataDir, id, autoResumeConfig(), later.Add(time.Hour)); resumed || after.State != request.StateResumeReview {
		t.Fatalf("ticket 2: resumed = %t, state %s; want it left to the operator", resumed, after.State)
	}
}

// A lost build kept for a round resume is resumed with "round" through the
// same checks; when the build step then refuses that resume
// (RefuseResume), the request waits for the operator.
func TestAutoResumeOfAKeptBuildThenARefusedResumeWaitsForTheOperator(t *testing.T) {
	dataDir, id, _ := requestdrivertest.LostBuildFixture(newFakeDeps(t), t, "")
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	entered, err := time.Parse(time.RFC3339Nano, r.Resume.At)
	if err != nil {
		t.Fatal(err)
	}
	reset := entered.Add(time.Hour).UTC().Truncate(time.Second)
	r.Resume.Cause, r.Resume.RetryAt, r.Resume.ResetAt = unresolved.ProviderQuota, reset.Format(time.RFC3339), reset.Format(time.RFC3339)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	checks := &requestdrivertest.FakeResumePreconditions{T: t, OK: true}
	cfg := requestdriver.WorkerConfig{Resume: requestdriver.ResumeGate{Preconditions: checks}}
	resumed, after := autoResume(t, dataDir, id, cfg, reset.Add(request.AutoResumeMargin))
	if !resumed || after.State != request.StateBuilding || after.PendingResumeVerb() != request.ResumeRound || len(checks.Asked) != 1 || checks.Asked[0] != "lost-run" {
		t.Fatalf("resumed = %t, state %s, verb %q, checked %v; want the kept build resumed with round after its checks", resumed, after.State, after.PendingResumeVerb(), checks.Asked)
	}
	if err := after.RefuseResume([]string{"a container of run lost-run is alive"}, reset.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := after.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if resumed, r := autoResume(t, dataDir, id, cfg, reset.Add(time.Hour)); resumed || r.State != request.StateResumeReview {
		t.Fatalf("after the refused resume: resumed = %t, state %s; want it waiting for the operator", resumed, r.State)
	} else if !strings.Contains(r.NextAction(), "already made its one automatic resume") {
		t.Errorf("NextAction = %q, want it to say the automatic resume is spent", r.NextAction())
	}
}

// Two polls of one request at once make one resume.
func TestAutoResumeIsMadeOnceByConcurrentPolls(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	const callers = 4
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			resumed, err := requestdriver.AutoResumeIfDue(context.Background(), dataDir, "req-1", autoResumeConfig(), quotaDue)
			results <- resumed
			errs <- err
		}()
	}
	made := 0
	for range callers {
		if <-results {
			made++
		}
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	resumes := 0
	for _, h := range r.History {
		if h.From == request.StateResumeReview {
			resumes++
		}
	}
	if made != 1 || resumes != 1 {
		t.Fatalf("%d callers reported a resume and the History holds %d; want one of each", made, resumes)
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
	if r.AutoResumed == nil || len(r.AutoResumed.Refused) == 0 || !strings.Contains(r.NextAction(), "abc123") {
		t.Fatalf("AutoResumed = %+v, NextAction = %q; want the refusal recorded and named", r.AutoResumed, r.NextAction())
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
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: quotaRetryAt.Format(time.RFC3339), ResetAt: quotaRetryAt.Format(time.RFC3339)}, quotaRefusedAt); err != nil {
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

// A park of a request the factory once resumed by itself says nothing of
// the automatic resume: it is no provider-quota wait.
func TestAParkAfterAnAutomaticResumeSaysNothingOfIt(t *testing.T) {
	dataDir, id := requestdrivertest.BuildingFixture(newFakeDeps(t), t, 2)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	reset := quotaRetryAt.Format(time.RFC3339)
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: reset, ResetAt: reset}, quotaRefusedAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if resumed, _ := autoResume(t, dataDir, id, autoResumeConfig(), quotaDue); !resumed {
		t.Fatal("the wait was not resumed")
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Reason: "ticket 1's pull request was closed without merging"}, quotaDue.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Error, "automatic resume") || strings.Contains(r.NextAction(), "automatic resume") {
		t.Errorf("Error = %q, NextAction = %q; want the park's prompt alone", r.Error, r.NextAction())
	}
}

// seedTransientWait puts id, a spec_drafting request, in resume_review on
// cause at quotaRefusedAt, as the driver does when the step's job never got
// to the work.
func seedTransientWait(t *testing.T, dataDir, id string, cause unresolved.Cause) {
	t.Helper()
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, id, request.StateSpecDrafting)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := r.EnterResumeReview(request.StateSpecDrafting, request.LostStep{Cause: cause}, quotaRefusedAt); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// A step that never got to the work (its sandbox never started, the route
// served nothing, a registry was unreachable) is resumed once by the factory
// AutoRetryDelay after the wait began, as `factoryd resume` would.
func TestAutoResumeRetriesATransientCauseOnceAfterItsDelay(t *testing.T) {
	due := quotaRefusedAt.Add(request.AutoRetryDelay)
	for _, cause := range []unresolved.Cause{unresolved.SandboxLaunch, unresolved.ModelUnavailable, unresolved.DependencyFetch} {
		dataDir := t.TempDir()
		seedTransientWait(t, dataDir, "req-1", cause)
		waiting := requestdrivertest.LoadRequest(t, dataDir, "req-1")
		if got, ok := waiting.AutoResumeDue(quotaRefusedAt); !ok || !got.Equal(due) {
			t.Fatalf("cause %s: AutoResumeDue = %v, %t, want %v", cause, got, ok, due)
		}
		if !strings.Contains(waiting.NextAction(), "the factory resumes it by itself, once, at "+due.Format(time.RFC3339)) {
			t.Errorf("cause %s: NextAction = %q, want it to say when the factory resumes it", cause, waiting.NextAction())
		}
		if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), due.Add(-time.Second)); resumed || r.AutoRetried != nil {
			t.Fatalf("cause %s: resumed before its delay (state %s)", cause, r.State)
		}
		resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), due)
		if !resumed || r.State != request.StateSpecDrafting {
			t.Fatalf("cause %s: resumed = %t, state %s; want the spec_drafting step resumed", cause, resumed, r.State)
		}
		if auto := r.AutoRetried; auto == nil || auto.Cause != cause || auto.RetryAt != "" || r.AutoResumed != nil {
			t.Fatalf("cause %s: AutoRetried = %+v, AutoResumed = %+v; want the transient record with its cause, the quota record untouched", cause, auto, r.AutoResumed)
		}
		last := r.History[len(r.History)-1]
		if last.By != "factory" || !strings.Contains(last.Reason, "after the step ended with cause "+string(cause)) {
			t.Errorf("cause %s: History ends with %+v, want the factory's automatic resume naming the cause", cause, last)
		}
		log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
		if err != nil || !strings.Contains(string(log), "Resumed automatically after a step that never got to the work") {
			t.Errorf("cause %s: notifications.log = %q, %v; want the automatic resume announced", cause, log, err)
		}
	}
}

// enterWaitAgain moves id, back in its step, to resume_review on step at at,
// as the driver does when the resumed step ends with no verdict again.
func enterWaitAgain(t *testing.T, dataDir, id string, step request.LostStep, at time.Time) {
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
	if err := requestdriver.EnterResumeReviewHoldingLock(dataDir, r, step, at); err != nil {
		t.Fatal(err)
	}
}

// Two budgets, each spent once per request: a transient retry does not spend
// the provider-quota resume, and a second wait of either kind waits for the
// operator.
func TestAutoResumeKeepsSeparateBudgetsForTheTwoKinds(t *testing.T) {
	dataDir := t.TempDir()
	seedTransientWait(t, dataDir, "req-1", unresolved.SandboxLaunch)
	if resumed, _ := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaRefusedAt.Add(request.AutoRetryDelay)); !resumed {
		t.Fatal("the transient wait was not retried")
	}

	quotaAt := quotaRefusedAt.Add(time.Hour)
	reset := quotaAt.Add(time.Hour)
	enterWaitAgain(t, dataDir, "req-1", request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: reset.Format(time.RFC3339), ResetAt: reset.Format(time.RFC3339)}, quotaAt)
	resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), reset.Add(request.AutoResumeMargin))
	if !resumed || r.AutoResumed == nil || r.AutoRetried == nil {
		t.Fatalf("quota wait after a transient retry: resumed = %t, AutoResumed %+v, AutoRetried %+v; want it resumed, both records kept", resumed, r.AutoResumed, r.AutoRetried)
	}
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil || !strings.Contains(string(log), "is spent too") {
		t.Errorf("notifications.log = %q, %v; want the notice to say both automatic resumes are spent", log, err)
	}

	secondQuota := reset.Add(time.Hour)
	secondReset := secondQuota.Add(time.Hour)
	enterWaitAgain(t, dataDir, "req-1", request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: secondReset.Format(time.RFC3339), ResetAt: secondReset.Format(time.RFC3339)}, secondQuota)
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), secondReset.Add(time.Hour)); resumed || r.State != request.StateResumeReview {
		t.Fatalf("second quota wait: resumed = %t, state %s; want it left to the operator", resumed, r.State)
	} else if !strings.Contains(r.NextAction(), "already made its one automatic resume of this request after a provider's limit reset") {
		t.Errorf("NextAction = %q, want it to say the quota resume is spent", r.NextAction())
	}
	if _, err := request.ResumeRequest(dataDir, "req-1", request.ResumeRound, "alice", secondReset.Add(2*time.Hour), nil); err != nil {
		t.Fatal(err)
	}

	secondTransient := secondReset.Add(3 * time.Hour)
	enterWaitAgain(t, dataDir, "req-1", request.LostStep{Cause: unresolved.DependencyFetch}, secondTransient)
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), secondTransient.Add(request.AutoRetryDelay)); resumed || r.State != request.StateResumeReview {
		t.Fatalf("second transient wait: resumed = %t, state %s; want it left to the operator", resumed, r.State)
	} else if !strings.Contains(r.NextAction(), "already made its one automatic retry of this request") {
		t.Errorf("NextAction = %q, want it to say the transient retry is spent", r.NextAction())
	}
}

// The other order: a provider-quota resume leaves the transient retry, and a
// second transient wait waits for the operator.
func TestAutoResumeRetriesATransientWaitAfterAQuotaResume(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.SeedQuotaWait(t, dataDir, "req-1", request.StateSpecDrafting, quotaRetryAt.Format(time.RFC3339), quotaRefusedAt)
	if resumed, _ := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaDue); !resumed {
		t.Fatal("the quota wait was not resumed")
	}
	first := quotaDue.Add(time.Minute)
	enterWaitAgain(t, dataDir, "req-1", request.LostStep{Cause: unresolved.ModelUnavailable}, first)
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), first.Add(request.AutoRetryDelay)); !resumed || r.AutoRetried == nil || r.AutoRetried.Cause != unresolved.ModelUnavailable {
		t.Fatalf("transient wait after a quota resume: resumed = %t, AutoRetried %+v; want it retried", resumed, r.AutoRetried)
	}
	second := first.Add(time.Hour)
	enterWaitAgain(t, dataDir, "req-1", request.LostStep{Cause: unresolved.SandboxLaunch}, second)
	if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), second.Add(request.AutoRetryDelay)); resumed || r.State != request.StateResumeReview {
		t.Fatalf("second transient wait: resumed = %t, state %s; want it left to the operator", resumed, r.State)
	}
}

// A record an earlier release wrote (auto_resumed with no cause) is the
// provider-quota resume spent; the transient retry is still there.
func TestAnEarlierAutoResumeRecordIsTheQuotaBudget(t *testing.T) {
	dataDir := t.TempDir()
	seedTransientWait(t, dataDir, "req-1", unresolved.SandboxLaunch)
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	r.AutoResumed = &request.AutoResume{At: quotaRefusedAt.Add(-time.Hour).Format(time.RFC3339Nano), RetryAt: quotaRefusedAt.Add(-2 * time.Hour).Format(time.RFC3339), FromState: request.StateSpecDrafting}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if resumed, after := autoResume(t, dataDir, "req-1", autoResumeConfig(), quotaRefusedAt.Add(request.AutoRetryDelay)); !resumed || after.AutoRetried == nil {
		t.Fatalf("resumed = %t, AutoRetried %+v; want the transient wait retried", resumed, after.AutoRetried)
	}
	again := quotaRefusedAt.Add(time.Hour)
	reset := again.Add(time.Hour)
	enterWaitAgain(t, dataDir, "req-1", request.LostStep{Cause: unresolved.ProviderQuota, RetryAt: reset.Format(time.RFC3339), ResetAt: reset.Format(time.RFC3339)}, again)
	if resumed, after := autoResume(t, dataDir, "req-1", autoResumeConfig(), reset.Add(time.Hour)); resumed || after.State != request.StateResumeReview {
		t.Fatalf("quota wait: resumed = %t, state %s; want the earlier record to count as the quota resume spent", resumed, after.State)
	}
}

// A transient wait is resumed by the factory only while it is younger than
// AutoRetryWindow: an older one (a worker that was down, an install over a
// wait the operator left) waits for the operator, and nothing is spent.
func TestAutoResumeLeavesAStaleTransientWaitToTheOperator(t *testing.T) {
	until := quotaRefusedAt.Add(request.AutoRetryWindow)
	for _, now := range []time.Time{until, until.Add(72 * time.Hour)} {
		dataDir := t.TempDir()
		seedTransientWait(t, dataDir, "req-1", unresolved.SandboxLaunch)
		waiting := requestdrivertest.LoadRequest(t, dataDir, "req-1")
		if _, ok := waiting.AutoResumeDue(now); ok {
			t.Fatalf("at %v: AutoResumeDue is ok; want a wait past its window not due", now)
		}
		if !strings.Contains(waiting.NextAction(), "before "+until.Format(time.RFC3339)) {
			t.Errorf("NextAction = %q, want it to name the end of the window", waiting.NextAction())
		}
		if resumed, r := autoResume(t, dataDir, "req-1", autoResumeConfig(), now); resumed || r.State != request.StateResumeReview || r.AutoRetried != nil {
			t.Fatalf("at %v: resumed = %t, state %s, AutoRetried %+v; want it left to the operator, nothing spent", now, resumed, r.State, r.AutoRetried)
		}
	}
	dataDir := t.TempDir()
	seedTransientWait(t, dataDir, "req-1", unresolved.SandboxLaunch)
	if resumed, _ := autoResume(t, dataDir, "req-1", autoResumeConfig(), until.Add(-time.Second)); !resumed {
		t.Fatal("a transient wait inside its window was not resumed")
	}
}
