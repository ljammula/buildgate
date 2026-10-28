package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/unresolved"
)

// stubExit swaps launchOracleDraft for a script that exits code with calls on
// the meter's ledger, writing its evidence file (status "failed") only when
// evidence is set.
func stubExit(t *testing.T, code int, calls meter.Calls, evidence bool) {
	t.Helper()
	prev := launchOracleDraft
	t.Cleanup(func() { launchOracleDraft = prev })
	launchOracleDraft = func(_ context.Context, _ requestdriver.WorkerConfig, _ *request.Request, l oracleLaunch) (runner.Result, error) {
		if evidence {
			mustWrite(t, l.EvidencePath, `{"schema_version": 1, "status": "failed"}`)
		}
		return runner.Result{ExitCode: code, LogPath: l.LogPath(1), RelayCalls: calls}, nil
	}
}

// A launch that returned no exit code is a job failure whose cause is read
// from the error's chain: a sandbox that never started its command, with its
// removal confirmed, is a sandbox launch failure; anything else is the
// drafting job's.
func TestAnOracleDraftLaunchErrorIsAJobFailureWithItsCause(t *testing.T) {
	cases := map[string]struct {
		err  error
		want unresolved.Cause
	}{
		"sandbox never started":         {fmt.Errorf("sandbox attempts exhausted: %w", fmt.Errorf("%w: create refused", sandbox.ErrSandboxNotStarted)), unresolved.SandboxLaunch},
		"refused below the disk floor":  {fmt.Errorf("sandbox attempts exhausted: %w", fmt.Errorf("%w: %w", sandbox.ErrSandboxNotStarted, sandbox.ErrSandboxDiskLow)), unresolved.SandboxDisk},
		"never started, not removed":    {errors.Join(fmt.Errorf("%w: create refused", sandbox.ErrSandboxNotStarted), sandbox.ErrCleanupUnconfirmed), unresolved.DraftingJob},
		"command started twice":         {fmt.Errorf("sandbox x: %w", sandbox.ErrSandboxRerun), unresolved.DraftingJob},
		"an error that only says so":    {errors.New("the sandbox did not start the worker command"), unresolved.DraftingJob},
		"deadline while the script ran": {context.DeadlineExceeded, unresolved.DraftingJob},
		"credential refused at launch":  {fmt.Errorf("sandbox attempts exhausted: %w", sandbox.RouteCredentialError(errors.New("route credential expires before this step's budget ends"))), unresolved.RouteCredential},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDraftFixture(t)
			stubLaunch(t, fakeScript{Err: tc.err}, nil)
			got, err := f.run(t)
			var failure *requestdriver.OracleDraftJobFailure
			if !errors.As(err, &failure) {
				t.Fatalf("got %+v, %v; want an OracleDraftJobFailure", got, err)
			}
			if failure.Cause != tc.want {
				t.Errorf("Cause = %q, want %q", failure.Cause, tc.want)
			}
		})
	}
}

// A script that exited non-zero is a job failure only on a fact outside its
// output: no model request of the launch was answered, or it left no
// evidence file. With its own "failed" evidence and an answered request it
// is a failed draft, as before.
func TestAnOracleDraftScriptExitIsAJobFailureOnlyWithoutAnAnswerOrEvidence(t *testing.T) {
	cases := map[string]struct {
		code     int
		calls    meter.Calls
		evidence bool
		want     unresolved.Cause // "" for a failed draft
	}{
		"crashed before its evidence":         {1, meter.Calls{Answered: 4}, false, unresolved.DraftingJob},
		"crashed, no model route":             {1, meter.Calls{}, false, unresolved.DraftingJob},
		"its own failed verdict":              {2, meter.Calls{Answered: 2, Failed: 6, RefusedAtEnd: 6, RefusedAtEndBy: meter.RefusedByProviderError}, true, ""},
		"answered, then the provider's limit": {2, meter.Calls{Answered: 12, RateLimited: 3, RefusedAtEnd: 3, RefusedAtEndBy: meter.RefusedByProviderLimit}, true, ""},
		"its own failed verdict, no ledger":   {2, meter.Calls{}, true, ""},
		// A success head with unread usage may have delivered nothing: by
		// the rule every model step shares (run.ModelRouteCause) the route
		// did not serve the job.
		"an answer whose usage was unread": {2, meter.Calls{AnsweredUnread: 1, Failed: 2}, true, unresolved.ModelUnavailable},
		"no request answered":              {2, meter.Calls{Failed: 2, Unanswered: 1}, true, unresolved.ModelUnavailable},
		// Rejected as malformed is the job's own doing: its evidence stands.
		"every request rejected upstream":    {2, meter.Calls{Rejected: 3}, true, ""},
		"none answered, one a 429":           {2, meter.Calls{RateLimited: 1, Failed: 2}, true, unresolved.ProviderQuota},
		"every request refused by the meter": {2, meter.Calls{DeniedBudget: 2, DeniedRate: 1}, true, unresolved.MeterDenied},
		// Who refused last names the cause; with nothing served and no
		// order known, the meter's own refusals do.
		"meter refusals after a 429":           {2, meter.Calls{RateLimited: 1, DeniedCeiling: 2}, true, unresolved.MeterDenied},
		"none answered, records without facts": {2, meter.Calls{Failed: 3, Legacy: true}, true, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDraftFixture(t)
			stubExit(t, tc.code, tc.calls, tc.evidence)
			got, err := f.run(t)
			var failure *requestdriver.OracleDraftJobFailure
			if tc.want == "" {
				if err != nil || got.Status != request.OracleDraftFailed {
					t.Fatalf("got %+v, %v; want a failed draft", got, err)
				}
				return
			}
			if !errors.As(err, &failure) || failure.Cause != tc.want {
				t.Fatalf("got %+v, %v; want an OracleDraftJobFailure with cause %q", got, err, tc.want)
			}
		})
	}
}

// A run that used its whole budget ran: with no evidence file it is still
// the failed draft it was, unless the ledger shows no request answered.
func TestAnOracleDraftTimeoutIsAJobFailureOnlyWhenNoRequestWasAnswered(t *testing.T) {
	f := newDraftFixture(t)
	elapse := func(calls meter.Calls) {
		prev, prevClock := launchOracleDraft, oracleDraftClock
		t.Cleanup(func() { launchOracleDraft, oracleDraftClock = prev, prevClock })
		ticks := 0
		oracleDraftClock = func() time.Time {
			ticks++
			return time.Unix(0, 0).Add(time.Duration(ticks) * 24 * time.Hour)
		}
		launchOracleDraft = func(_ context.Context, _ requestdriver.WorkerConfig, _ *request.Request, l oracleLaunch) (runner.Result, error) {
			return runner.Result{ExitCode: oracleScriptTimeoutExit, LogPath: l.LogPath(1), RelayCalls: calls}, nil
		}
	}
	elapse(meter.Calls{Answered: 3})
	if got, err := f.run(t); err != nil || got.Status != request.OracleDraftFailed || !strings.Contains(got.Detail, "timed out; model wrote no files") {
		t.Fatalf("got %+v, %v; want the failed draft of a timeout", got, err)
	}
	elapse(meter.Calls{Unanswered: 3})
	var failure *requestdriver.OracleDraftJobFailure
	if got, err := f.run(t); !errors.As(err, &failure) || failure.Cause != unresolved.ModelUnavailable {
		t.Fatalf("got %+v, %v; want an OracleDraftJobFailure with cause model_unavailable", got, err)
	}
}

// End to end through the driver: a launch whose sandbox never started leaves
// the request in resume_review with that cause, `factoryd resume` returns it
// to oracle_drafting, and the redraft reaches oracle_review.
func TestAnOracleDraftJobFailureWaitsInResumeReviewAndResumeRedrafts(t *testing.T) {
	dp := newTestDeps(t)
	f := newDraftFixture(t)
	drive := func() {
		t.Helper()
		if err := driveRequests(dp, context.Background(), f.dataDir, f.cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runOracleDraftJob, requestdrivertest.FailingBuildRunner(t)); err != nil {
			t.Fatal(err)
		}
	}
	stubLaunch(t, fakeScript{Err: fmt.Errorf("%w: gateway refused the create", sandbox.ErrSandboxNotStarted)}, nil)
	drive()
	got := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.SandboxLaunch || got.Resume.FromState != request.StateOracleDrafting {
		t.Fatalf("State = %q, Resume = %+v; want resume_review from oracle_drafting, cause sandbox_launch", got.State, got.Resume)
	}
	if got.OracleDraft != nil {
		t.Errorf("OracleDraft = %+v, want none", got.OracleDraft)
	}
	if !strings.Contains(got.Error, "gateway refused the create") {
		t.Errorf("Error = %q, want the job's own words after the cause", got.Error)
	}

	if err := resumeMainWith(dp, []string{"-data-dir", f.dataDir, f.id}, requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := requestdrivertest.LoadRequest(t, f.dataDir, f.id); got.State != request.StateOracleDrafting {
		t.Fatalf("State after resume = %q, want oracle_drafting", got.State)
	}
	stubLaunch(t, goDrafted(), nil)
	drive()
	got = requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if got.State != request.StateOracleReview || got.OracleDraft == nil || got.OracleDraft.Status != request.OracleDrafted {
		t.Fatalf("State = %q, OracleDraft = %+v; want a drafted oracle_review", got.State, got.OracleDraft)
	}
}
