package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// restackTicketSpec is a request ticket's spec in the restack integration
// test, in the shape a request's ticket build checks: ticket 1 changes
// content.txt, ticket 2 other.txt, each covering criterion 1 of the
// request's spec (restackRequestSpec).
func restackTicketSpec(file string) string {
	return "Verify-Command: true\nAllowed-Files: " + file + "\nRequired-Changed-Files: " + file + "\nTests-Required: no -- integration fixture doesn't exercise tests_red_on_base\n\n## Goal\n\nChange " + file + ".\n\n## Plan\n\n### Files to touch\n\n- " + file + "\n\n### Steps\n\n1. Change it.\n\n### Tests to add\n\n- none\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nNothing else.\n"
}

// restackRequestSpec is the request's approved spec.
const restackRequestSpec = "# Spec\n\n## Acceptance criteria\n\n1. AC-1: the files change.\n"

// restackDeps is the test's requestdriver.Deps: the real git calls, a fake
// GitHub, and builds run through the integration binary (whose fake docker
// stands in for the sandbox), since this test process has no sandbox runtime.
type restackDeps struct {
	*deps
	runTicket func(ctx context.Context, args []string, onReady func(*run.Run)) error
}

func (d restackDeps) RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error {
	return d.runTicket(ctx, args, onReady)
}

// runReplayThroughTheBinary runs the request driver's replay run argv with
// the integration binary and reports the run it made to onReady once it
// ended: the binary is another process, so the run cannot be reported when
// it starts.
func runReplayThroughTheBinary(t *testing.T, dataDir string, env []string) func(context.Context, []string, func(*run.Run)) error {
	return func(_ context.Context, args []string, onReady func(*run.Run)) error {
		script, err := filepath.Abs("testdata/fake_build_app.sh")
		if err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadDir(filepath.Join(dataDir, "runs"))
		// The spec-conformity review needs a model route, which this test
		// has none of: its criteria file is left out, as for every
		// integration run here.
		if i := slices.Index(args, "-spec-acceptance-criteria"); i >= 0 {
			args = slices.Delete(slices.Clone(args), i, i+2)
		}
		cmd := factorydCommand(t, append(slices.Clone(args), "-sandbox-image", fakeSandboxImage, "-build-app-interpreter", "/bin/sh", "-build-app-script", script, "-timeout", "30s")...)
		cmd.Env = append(append(os.Environ(), "FAKE_BUILD_APP_MODE=fail", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), env...)
		out, _ := cmd.CombinedOutput()
		t.Logf("replay run output:\n%s", out)
		after, err := os.ReadDir(filepath.Join(dataDir, "runs"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range after {
			if !slices.ContainsFunc(before, func(b os.DirEntry) bool { return b.Name() == e.Name() }) {
				rec, err := run.Load(dataDir, e.Name())
				if err != nil {
					t.Fatal(err)
				}
				onReady(rec)
				return nil
			}
		}
		t.Fatal("the replay run left no record")
		return nil
	}
}

// restackIntegration is the restack integration test's world: a fixture
// repository with a bare origin, two stacked tickets built by the
// integration binary and pushed, and a pushed review round that moved ticket
// 1's branch to newTip.
type restackIntegration struct {
	t              *testing.T
	ws, dataDir    string
	env            []string
	mainBranch     string
	t1, t2, review *run.Run
	newTip         string
}

func newRestackIntegration(t *testing.T) *restackIntegration {
	t.Helper()
	x := &restackIntegration{t: t, ws: newFixtureRepo(t), dataDir: t.TempDir(), env: replayEnv(t)}
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitOut(t, x.ws, "init", "-q", "--bare", origin)
	gitOut(t, x.ws, "remote", "add", "origin", origin)
	x.mainBranch = gitOut(t, x.ws, "rev-parse", "--abbrev-ref", "HEAD")
	gitOut(t, x.ws, "push", "-q", "origin", "HEAD:refs/heads/"+x.mainBranch)
	x.t1 = runFactorydWithSpecFlagsAndDataDir(t, x.ws, "commit", "true", restackTicketSpec("content.txt"), "30s", x.env, nil, x.dataDir)
	x.t2 = runFactorydWithSpecFlagsAndDataDir(t, x.ws, "commit_other", "true", restackTicketSpec("other.txt"), "30s", x.env, []string{"-prior-run", x.t1.ID}, x.dataDir)
	if x.t1.State != run.StateAccepted || x.t2.State != run.StateAccepted || x.t2.StackBaseSHA != x.t1.ResultSHA {
		t.Fatalf("ticket runs: %s %s, ticket 2 stack base %s; want both accepted, ticket 2 on %s", x.t1.State, x.t2.State, x.t2.StackBaseSHA, x.t1.ResultSHA)
	}
	gitOut(t, x.ws, "push", "-q", "origin", x.t1.ResultSHA+":refs/heads/"+x.t1.Branch, x.t2.ResultSHA+":refs/heads/"+x.t2.Branch)
	x.reviewRound()
	// The ticket runs as the request driver records them; ticket 2's
	// ticket index is left for the restack to name from the request.
	x.t1.RequestID, x.t1.TicketIndex, x.t2.RequestID = "req-1", 1, "req-1"
	for _, rec := range []*run.Run{x.t1, x.t2} {
		if err := rec.Save(x.dataDir); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

// reviewRound commits a review fix on ticket 1's branch and pushes it, and
// records its run: ticket 1's PR head.
func (x *restackIntegration) reviewRound() {
	t := x.t
	round := filepath.Join(t.TempDir(), "round")
	gitOut(t, x.ws, "worktree", "add", "-q", "--force", round, x.t1.Branch)
	if err := os.WriteFile(filepath.Join(round, "fix.txt"), []byte("review fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, round, "add", "-A")
	gitOut(t, round, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "ticket 1: review fix")
	x.newTip = gitOut(t, round, "rev-parse", "HEAD")
	gitOut(t, x.ws, "worktree", "remove", "--force", round)
	gitOut(t, x.ws, "push", "-q", "origin", x.newTip+":refs/heads/"+x.t1.Branch)
	x.review = &run.Run{ID: "req-1-001-review1", Project: x.t1.Project, ProjectPath: x.ws, State: run.StateAccepted, RequestID: "req-1", TicketIndex: 1, Branch: x.t1.Branch, BaseSHA: x.t1.ResultSHA, DiffBaseSHA: x.t1.BaseSHA, ResultSHA: x.newTip}
	if err := x.review.Save(x.dataDir); err != nil {
		t.Fatal(err)
	}
}

// request saves the two-ticket request in pr_review, ticket 2's pull
// request stacked on ticket 1's branch, and its approved spec.
func (x *restackIntegration) request() *request.Request {
	t := x.t
	specs := t.TempDir()
	var tickets []request.Ticket
	for i, rec := range []*run.Run{x.t1, x.t2} {
		spec := filepath.Join(specs, fmt.Sprintf("%03d.spec.md", i+1))
		if err := os.WriteFile(spec, []byte(restackTicketSpec([]string{"content.txt", "other.txt"}[i])), 0o600); err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, request.Ticket{Index: i + 1, SpecPath: spec, RunID: rec.ID, Branch: rec.Branch, PRURL: fmt.Sprintf("https://github.com/acme/app/pull/%d", i+1), PRState: "draft"})
	}
	tickets[0].PRBase = x.mainBranch
	tickets[0].Rounds = []request.Round{{Index: 1, RunID: x.review.ID, Outcome: request.RoundAccepted, Pushed: true}}
	tickets[1].PRBase, tickets[1].PRState = x.t1.Branch, "stacked"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := &request.Request{ID: "req-1", Workspace: x.ws, Project: x.t1.Project, State: request.StatePRReview, TicketIndex: 2, TicketCount: 2, Tickets: tickets, VerifyCommand: "true", SubmittedAt: now, UpdatedAt: now, EnteredAt: now}
	if err := r.Save(x.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(x.dataDir, r.ID), []byte(restackRequestSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// deps is the driver's deps: the real git calls of a restack and the host's
// git version, a GitHub whose two pull requests are open drafts at their
// heads, and builds run through the integration binary.
func (x *restackIntegration) deps(r *request.Request) requestdriver.Deps {
	dp := newTestDeps(x.t)
	f, real := fakeForgeOf(dp), realForge{dp: dp}
	f.readReviewStateFn = func(_ context.Context, prURL string, _ forge.AuthorPolicy) (forge.ReviewState, error) {
		if prURL == r.Tickets[0].PRURL {
			return forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, BaseRefName: x.mainBranch, HeadSHA: x.newTip}, nil
		}
		return forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, BaseRefName: x.t1.Branch, HeadSHA: x.t2.ResultSHA}, nil
	}
	f.markPullRequestReadyFn = func(context.Context, string) error { return nil }
	f.fetchBranchCommitFn, f.revListRangeFn, f.patchIDFn = real.fetchBranchCommit, real.revListRange, real.patchID
	f.replayCommitsFn, f.pushWithLeaseFn, f.moveBranchFn = real.replayCommits, real.pushWithLease, real.moveBranch
	f.hostGitVersionFn = real.hostGitVersion
	return restackDeps{deps: dp, runTicket: runReplayThroughTheBinary(x.t, x.dataDir, x.env)}
}

// TestIntegrationRestackReplaysAStackedTicketOntoItsEarlierTicketsNewHead:
// one full restack with real git. Ticket 2 was built on ticket 1's first
// head and both branches are on origin; a review round then moved ticket 1's
// branch. The PR-review pass replays ticket 2's commit onto ticket 1's new
// head, a replay run judges it (accepted, every gate recorded, nothing
// built), and the result replaces ticket 2's branch on origin through the
// leased push; the local branch follows and the restack keeps its round ref.
func TestIntegrationRestackReplaysAStackedTicketOntoItsEarlierTicketsNewHead(t *testing.T) {
	x := newRestackIntegration(t)
	r := x.request()
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3, TemporalAddress: sharedTemporalAddress(t)}
	if err := requestdriver.AdvancePRReview(x.deps(r), context.Background(), x.dataDir, r, cfg, time.Now()); err != nil {
		t.Fatalf("AdvancePRReview: %v", err)
	}
	if r.State != request.StatePRReview {
		t.Fatalf("request state %s (%s), want pr_review", r.State, r.Error)
	}
	ticket := r.Tickets[1]
	if len(ticket.Rounds) != 1 || ticket.Rounds[0].Kind != request.RestackRoundKind || !ticket.Rounds[0].Pushed {
		t.Fatalf("ticket 2 rounds = %+v, want one pushed restack", ticket.Rounds)
	}
	replayRun, err := run.Load(x.dataDir, ticket.PRHeadRunID())
	if err != nil {
		t.Fatal(err)
	}
	x.checkReplayRun(replayRun)
	x.checkOrigin(replayRun)
}

// checkReplayRun checks the restack's replay run: accepted onto ticket 1's
// new head, its gates recorded, its build carried from ticket 2's run, and
// its one commit pair's patch ids equal.
func (x *restackIntegration) checkReplayRun(rec *run.Run) {
	t := x.t
	if rec.State != run.StateAccepted || rec.Replay == nil || rec.Replay.OntoSHA != x.newTip || rec.StackBaseSHA != x.newTip {
		t.Fatalf("replay run %s: state %s (%s), replay %+v, stack base %s; want accepted onto %s", rec.ID, rec.State, rec.HaltError, rec.Replay, rec.StackBaseSHA, x.newTip)
	}
	if len(rec.GateResults) == 0 || !slices.ContainsFunc(rec.Attempts, func(a run.Attempt) bool { return a.ReplayOf == x.t2.ID }) {
		t.Errorf("replay run gates %+v, attempts %+v: want every gate recorded and the build carried from %s", rec.GateResults, rec.Attempts, x.t2.ID)
	}
	if pairs := rec.Replay.Commits; len(pairs) != 1 || pairs[0].OriginalPatchID == "" || pairs[0].OriginalPatchID != pairs[0].ReplayedPatchID {
		data, _ := json.Marshal(rec.Replay)
		t.Errorf("replay record %s: want one pair with equal patch ids", data)
	}
}

// checkOrigin checks that ticket 2's branch, on origin and locally, is at
// the replay run's result, on ticket 1's new head, and that the restack
// keeps its round ref.
func (x *restackIntegration) checkOrigin(rec *run.Run) {
	t := x.t
	remote := strings.Fields(gitOut(t, x.ws, "ls-remote", "origin", "refs/heads/"+x.t2.Branch))[0]
	if local := gitOut(t, x.ws, "rev-parse", "refs/heads/"+x.t2.Branch); remote != rec.ResultSHA || local != remote {
		t.Fatalf("origin's %s is %s, local %s; want both at the replay run's %s", x.t2.Branch, remote, local, rec.ResultSHA)
	}
	if gitOut(t, x.ws, "merge-base", x.newTip, remote) != x.newTip {
		t.Errorf("ticket 2's new head %s does not contain ticket 1's new head %s", remote, x.newTip)
	}
	if got := gitOut(t, x.ws, "rev-parse", "refs/buildgate/rounds/req-1/t2/r1"); got != remote {
		t.Errorf("round ref = %s, want the restacked head", got)
	}
}
