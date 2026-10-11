package requestdriver_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// escalationSettings is a session config whose execution role is luna at
// max, with escalation, nil for none.
func escalationSettings(escalation *sessionconfig.Escalation) sessionconfig.Settings {
	return sessionconfig.Settings{Roles: &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "luna", Thinking: "max", Allowed: []string{"luna", "picked", "sol"}, Escalation: escalation},
	}}
}

// requestPickingModel records `submit -model execution=picked` on the request.
func requestPickingModel(t *testing.T, dataDir, id string) {
	t.Helper()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Models = map[string]string{"execution": "picked"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// TestAutomaticCorrectiveRoundsRunOnTheEscalation: a ticket's first build
// runs on the model the request picked at the role's level, and the
// automatic round that follows a failed one, review round or check round,
// runs on roles.execution.escalation's model at its level, replacing the
// request's pick. The round's run is recorded as escalated.
func TestAutomaticCorrectiveRoundsRunOnTheEscalation(t *testing.T) {
	branchOf := func(id string) string { return "factoryd/" + id + "-001" }
	baseSHA, resultSHA := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2)
	for name, failing := range map[string]func(t *testing.T, dataDir, id string) requestdriver.TicketRunner{
		"review round": func(t *testing.T, dataDir, id string) requestdriver.TicketRunner {
			return requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branchOf(id), baseSHA, "")
		},
		"check round": func(t *testing.T, dataDir, id string) requestdriver.TicketRunner {
			return checkQuarantinedBuildRunner(t, dataDir, branchOf(id), baseSHA, resultSHA, "lint")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			requestPickingModel(t, dataDir, id)
			var firstArgs []string
			first := failing(t, dataDir, id)
			buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
				firstArgs = append([]string(nil), args...)
				return first(ctx, args, onReady)
			}
			calls, roundArgs := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
				if started, err := run.Load(dataDir, roundRunID); err != nil || !started.Escalated {
					t.Errorf("the started round's run: %v, escalated %v, want it recorded as escalated", err, started != nil && started.Escalated)
				}
				return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branchOf(id), PullRequestURL: "https://github.com/acme/app/pull/7"}
			})
			cfg := requestdriver.WorkerConfig{
				ReviewCorrectiveRounds: 1, OpenPullRequest: true,
				Settings: escalationSettings(&sessionconfig.Escalation{Model: "sol", Thinking: "high"}),
			}
			if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}
			if *calls != 1 {
				t.Fatalf("corrective runner calls = %d, want 1", *calls)
			}
			if got := requestdrivertest.ArgValue(firstArgs, "-execution-model"); got != "picked" || requestdrivertest.HasFlag(firstArgs, "-execution-thinking") {
				t.Errorf("first build: -execution-model %q, -execution-thinking present %v; want the request's pick and no level", got, requestdrivertest.HasFlag(firstArgs, "-execution-thinking"))
			}
			if model, level := requestdrivertest.ArgValue(*roundArgs, "-execution-model"), requestdrivertest.ArgValue(*roundArgs, "-execution-thinking"); model != "sol" || level != "high" {
				t.Errorf("corrective round: -execution-model %q -execution-thinking %q, want sol at high", model, level)
			}
		})
	}
}

// TestEscalationWithoutAThinkingLevelRunsAtTheRoles: an escalation that sets
// no level passes the role's own.
func TestEscalationWithoutAThinkingLevelRunsAtTheRoles(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{Settings: escalationSettings(&sessionconfig.Escalation{Model: "sol"})}
	args, err := requestdriver.BuildReviewCorrectiveArgs(dataDir, r, r.Tickets[0], cfg, r.Tickets[0].SpecPath, "round-1", "some-branch", strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	if model, level := requestdrivertest.ArgValue(args, "-execution-model"), requestdrivertest.ArgValue(args, "-execution-thinking"); model != "sol" || level != "max" {
		t.Errorf("-execution-model %q -execution-thinking %q, want sol at the role's max", model, level)
	}
}

// TestNoEscalationLeavesACorrectiveRoundsArgvUnchanged: with no escalation
// configured a corrective round's argv is what it is with no roles at all:
// the request's pick, and no -execution-thinking.
func TestNoEscalationLeavesACorrectiveRoundsArgvUnchanged(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	requestPickingModel(t, dataDir, id)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	build := func(cfg requestdriver.WorkerConfig) []string {
		args, err := requestdriver.BuildReviewCorrectiveArgs(dataDir, r, r.Tickets[0], cfg, r.Tickets[0].SpecPath, "round-1", "some-branch", strings.Repeat("0", 40))
		if err != nil {
			t.Fatal(err)
		}
		return args
	}
	bare, withRoles := build(requestdriver.WorkerConfig{}), build(requestdriver.WorkerConfig{Settings: escalationSettings(nil)})
	if !reflect.DeepEqual(bare, withRoles) {
		t.Errorf("argv with roles and no escalation:\n%v\nwant the argv with no roles:\n%v", withRoles, bare)
	}
	if requestdrivertest.HasFlag(bare, "-execution-thinking") || requestdrivertest.ArgValue(bare, "-execution-model") != "picked" {
		t.Errorf("argv = %v, want the request's pick and no -execution-thinking", bare)
	}
}

// TestPRReviewRoundStaysOnTheRolesModel: a round that answers a human
// reviewer's comments is not an automatic corrective build; with an
// escalation configured its argv names neither the escalation's model nor a
// level.
func TestPRReviewRoundStaysOnTheRolesModel(t *testing.T) {
	dp := newFakeDeps(t)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 10, Author: "alice", Body: "please rename this", CommentID: 111}}
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)

	var capturedArgs []string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		capturedArgs = args
		accepted := &run.Run{ID: requestdrivertest.ArgValue(args, "-ticket"), State: run.StateAccepted, WorkspacePath: t.TempDir(), ResultSHA: "cafef00d", Branch: requestdrivertest.ArgValue(args, "-on-branch")}
		return saveAcceptedRoundRun(t, dataDir, accepted)
	}
	dp.pushExistingBranchFn = func(ctx context.Context, workspaceDir, sha, branch string) error {
		requestdrivertest.LastPushedSHA[branch] = sha
		return nil
	}
	dp.remoteBranchHeadSHAFn = func(ctx context.Context, workspaceDir, branch string) (string, error) {
		if sha, ok := requestdrivertest.LastPushedSHA[branch]; ok {
			return sha, nil
		}
		return strings.Repeat("0", 40), nil
	}
	dp.roundResultDescendsFromHeadFn = func(dir, ancestor, descendant string) (bool, error) { return true, nil }
	dp.replyToReviewCommentFn = func(ctx context.Context, prURL string, commentID int64, body string) error { return nil }

	cfg := requestdriver.WorkerConfig{
		PrPollInterval: time.Minute, MaxReviewRounds: 3,
		Settings: escalationSettings(&sessionconfig.Escalation{Model: "sol", Thinking: "high"}),
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now()); err != nil {
		t.Fatalf("advancePRReview: %v", err)
	}
	if capturedArgs == nil {
		t.Fatal("the PR-review round's build was never started")
	}
	if requestdrivertest.HasFlag(capturedArgs, "-execution-model") || requestdrivertest.HasFlag(capturedArgs, "-execution-thinking") {
		t.Errorf("PR-review round argv = %v, want no -execution-model and no -execution-thinking", capturedArgs)
	}
}

// TestABuildThatReplacesALostEscalatedRoundRunsOnTheEscalation: an escalated
// build that never finished is followed, on `factoryd resume` in its kept
// worktree, by a build on the escalation too, itself recorded as escalated.
// A lost build that was not escalated (a ticket's first build) is followed
// by an ordinary one, and so is any lost build once no escalation is
// configured.
func TestABuildThatReplacesALostEscalatedRoundRunsOnTheEscalation(t *testing.T) {
	escalation := escalationSettings(&sessionconfig.Escalation{Model: "sol", Thinking: "high"})
	for name, tc := range map[string]struct {
		lostWasEscalated bool
		settings         sessionconfig.Settings
		wantEscalated    bool
	}{
		"lost escalated round":                  {true, escalation, true},
		"lost first build":                      {false, escalation, false},
		"lost escalated round, escalation gone": {true, escalationSettings(nil), false},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
			lost, err := run.Load(dataDir, "lost-run")
			if err != nil {
				t.Fatal(err)
			}
			lost.Escalated = tc.lostWasEscalated
			if err := lost.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			r, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			var args []string
			var started *run.Run
			accepting := requestdrivertest.AcceptingBuildRunner(t, dataDir)
			runner := func(ctx context.Context, argv []string, onReady func(*run.Run)) error {
				args = append([]string(nil), argv...)
				return accepting(ctx, argv, func(s *run.Run) {
					onReady(s)
					started = s
				})
			}
			cfg := requestdriver.WorkerConfig{
				Resume:   requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}},
				Settings: tc.settings,
			}
			if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, cfg, runner, time.Now()); err != nil {
				t.Fatalf("advanceBuilding: %v", err)
			}
			if started == nil || requestdrivertest.ArgValue(args, "-resume-worktree-of") != "lost-run" {
				t.Fatalf("argv = %v, want one build resuming the lost run", args)
			}
			model, level := requestdrivertest.ArgValue(args, "-execution-model"), requestdrivertest.ArgValue(args, "-execution-thinking")
			if tc.wantEscalated && (model != "sol" || level != "high" || !started.Escalated) {
				t.Errorf("-execution-model %q -execution-thinking %q, run escalated %v; want sol at high, recorded as escalated", model, level, started.Escalated)
			}
			if !tc.wantEscalated && (model != "" || level != "" || started.Escalated) {
				t.Errorf("-execution-model %q -execution-thinking %q, run escalated %v; want an ordinary build", model, level, started.Escalated)
			}
		})
	}
}

// TestARebuildAfterAnEscalatedRoundHaltedOrQuarantined: `factoryd retry` of
// a ticket whose last run is an escalated round. One that halted never
// finished, so the rebuild takes its place on the escalation; one that
// finished and quarantined is the operator's retry, on the role's model.
func TestARebuildAfterAnEscalatedRoundHaltedOrQuarantined(t *testing.T) {
	for name, tc := range map[string]struct {
		state         run.State
		wantEscalated bool
	}{
		"halted":      {run.StateHalted, true},
		"quarantined": {run.StateQuarantined, false},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			r, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			r.Tickets[0].RunID = "round-run"
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			if err := (&run.Run{ID: "round-run", State: tc.state, RequestID: id, Escalated: true}).Save(dataDir); err != nil {
				t.Fatal(err)
			}
			var calls [][]string
			cfg := requestdriver.WorkerConfig{Settings: escalationSettings(&sessionconfig.Escalation{Model: "sol", Thinking: "high"})}
			if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, cfg, capturingBuildRunner(t, dataDir, &calls), time.Now()); err != nil {
				t.Fatalf("advanceBuilding: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("build runner called %d times, want 1", len(calls))
			}
			model, level := requestdrivertest.ArgValue(calls[0], "-execution-model"), requestdrivertest.ArgValue(calls[0], "-execution-thinking")
			if tc.wantEscalated && (model != "sol" || level != "high") {
				t.Errorf("-execution-model %q -execution-thinking %q, want sol at high", model, level)
			}
			if !tc.wantEscalated && (model != "" || level != "") {
				t.Errorf("-execution-model %q -execution-thinking %q, want an ordinary build", model, level)
			}
		})
	}
}
