package requestdriver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/forge"
	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/workspace"
)

// hexSHA is a full commit id of one repeated hex digit.
func hexSHA(c byte) string { return strings.Repeat(string(c), 40) }

// The commits of restackFixture: ticket 1 was built to oldTip and a pushed
// review round moved it to newTip; ticket 2's run built c1 and head on
// oldTip; the replay makes r1 and r2.
var (
	oldTip, newTip = hexSHA('1'), hexSHA('5')
	c1, head       = hexSHA('2'), hexSHA('3')
	r1, r2         = hexSHA('a'), hexSHA('b')
	mainTip        = hexSHA('7')
)

// restackFixture is a two-ticket request in pr_review whose ticket 2 needs a
// restack: ticket 1's branch moved from oldTip to newTip after ticket 2 was
// built on it. Its fake deps answer from the maps below, and record every
// side effect in events, in order.
type restackFixture struct {
	t       *testing.T
	dp      *fakeDeps
	r       *request.Request
	dataDir string
	cfg     requestdriver.WorkerConfig
	now     time.Time

	remote     map[string]string
	under      map[[2]string]bool
	ranges     map[[2]string][]string
	merges     []string
	prs        map[string]forge.ReviewState
	events     []string
	runArgs    [][]string
	replayEnd  run.State
	replayErr  error
	gitVersion string
}

func newRestackFixture(t *testing.T, tickets int) *restackFixture {
	t.Helper()
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, tickets)
	f := &restackFixture{t: t, dp: dp, r: r, dataDir: dataDir, now: time.Now(),
		cfg:    requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3},
		remote: map[string]string{"factoryd/run-1": newTip, "factoryd/run-2": head},
		// The replayed head sits on the tip it was replayed onto.
		under:      map[[2]string]bool{{newTip, r2}: true, {mainTip, r2}: true},
		ranges:     map[[2]string][]string{{oldTip, head}: {c1, head}},
		prs:        map[string]forge.ReviewState{},
		replayEnd:  run.StateAccepted,
		gitVersion: "git version 2.50.1 (Apple Git-155)",
	}
	// Ticket 1: built to oldTip, a pushed review round moved it to newTip.
	f.saveRun(&run.Run{ID: "run-1", BaseSHA: hexSHA('0'), ResultSHA: oldTip, Branch: "factoryd/run-1"})
	f.saveRun(&run.Run{ID: "run-1-review1", BaseSHA: oldTip, DiffBaseSHA: hexSHA('0'), ResultSHA: newTip, Branch: "factoryd/run-1"})
	t1 := &r.Tickets[0]
	t1.Branch, t1.PRState, t1.PRBase = "factoryd/run-1", "draft", "main"
	t1.Rounds = []request.Round{{Index: 1, RunID: "run-1-review1", Outcome: request.RoundAccepted, Pushed: true}}
	// Ticket 2: built on oldTip.
	f.saveRun(&run.Run{ID: "run-2", BaseSHA: oldTip, StackBaseSHA: oldTip, ResultSHA: head, Branch: "factoryd/run-2"})
	t2 := &r.Tickets[1]
	t2.Branch, t2.PRState, t2.PRBase = "factoryd/run-2", "stacked", "factoryd/run-1"
	f.prs[t1.PRURL] = forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, BaseRefName: "main", HeadSHA: newTip}
	f.prs[t2.PRURL] = forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, BaseRefName: "factoryd/run-1", HeadSHA: head}
	f.save()
	f.wire()
	return f
}

// saveRun saves rec as an accepted, allowed run of ticket (the index its id
// names) of req-1.
func (f *restackFixture) saveRun(rec *run.Run) {
	f.t.Helper()
	rec.Project, rec.State, rec.RequestID = "widget", run.StateAccepted, "req-1"
	if rec.TicketIndex == 0 {
		fmt.Sscanf(strings.TrimPrefix(rec.ID, "run-"), "%d", &rec.TicketIndex)
	}
	if err := saveAcceptedRoundRun(f.t, f.dataDir, rec); err != nil {
		f.t.Fatal(err)
	}
}

func (f *restackFixture) save() {
	f.t.Helper()
	if err := f.r.Save(f.dataDir); err != nil {
		f.t.Fatal(err)
	}
}

func (f *restackFixture) event(format string, args ...any) {
	f.events = append(f.events, fmt.Sprintf(format, args...))
}

func (f *restackFixture) wire() {
	dp := f.dp
	dp.readReviewStateFn = func(_ context.Context, prURL string, _ forge.AuthorPolicy) (forge.ReviewState, error) {
		return f.prs[prURL], nil
	}
	dp.remoteBranchHeadSHAFn = func(_ context.Context, _, branch string) (string, error) {
		if sha, ok := f.remote[branch]; ok {
			return sha, nil
		}
		return "", fmt.Errorf("%w: %s", requestdriver.ErrRemoteBranchNotFound, branch)
	}
	dp.fetchBranchCommitFn = func(context.Context, string, string, string) error { return nil }
	dp.roundResultDescendsFromHeadFn = func(_, ancestor, descendant string) (bool, error) {
		return ancestor == descendant || f.under[[2]string{ancestor, descendant}], nil
	}
	dp.revListRangeFn = func(_, from, to string) ([]string, []string, error) {
		return f.ranges[[2]string{from, to}], f.merges, nil
	}
	dp.patchIDFn = func(_, sha string) (string, error) {
		return map[string]string{c1: hexSHA('d'), r1: hexSHA('d'), head: hexSHA('e'), r2: hexSHA('e')}[sha], nil
	}
	dp.replayCommitsFn = func(_ context.Context, _, _, branch, onto string, commits []string) ([]string, error) {
		f.event("replay %s onto %s on %s", strings.Join(commits, ","), onto, branch)
		if f.replayErr != nil {
			return nil, f.replayErr
		}
		return []string{r1, r2}[:len(commits)], nil
	}
	dp.runTicketFn = func(_ context.Context, args []string, onReady func(*run.Run)) error {
		f.runArgs = append(f.runArgs, args)
		id := requestdrivertest.ArgValue(args, "-ticket") + "-x"
		f.event("run %s", id)
		onReady(&run.Run{ID: id})
		replay, err := run.ReadReplayFile(requestdrivertest.ArgValue(args, "-replay"))
		if err != nil {
			f.t.Fatalf("the replay run's record: %v", err)
		}
		rec := &run.Run{ID: id, Project: "widget", State: f.replayEnd, RequestID: replay.RequestID, TicketIndex: replay.TicketIndex, Branch: requestdrivertest.ArgValue(args, "-on-branch"), BaseSHA: replay.HeadSHA, DiffBaseSHA: replay.OntoSHA, ResultSHA: replay.HeadSHA, StackBaseSHA: replay.OntoSHA, Replay: replay, Attempts: []run.Attempt{{Kind: "build", ReplayOf: replay.OfRunID}}}
		if f.replayEnd != run.StateAccepted {
			rec.HaltError = "verify failed on the new base"
		}
		return saveAcceptedRoundRun(f.t, f.dataDir, rec)
	}
	dp.undoMarkPullRequestReadyFn = func(_ context.Context, prURL string) error {
		f.event("draft %s", prURL)
		st := f.prs[prURL]
		st.IsDraft = true
		f.prs[prURL] = st
		return nil
	}
	dp.markPullRequestReadyFn = func(_ context.Context, prURL string) error {
		f.event("ready %s", prURL)
		return nil
	}
	dp.retargetPullRequestBaseFn = func(_ context.Context, prURL, base string) error {
		f.event("retarget %s to %s", prURL, base)
		return nil
	}
	dp.pushWithLeaseFn = func(_ context.Context, _, sha, branch, expected string) error {
		f.event("push %s to %s leased on %s", sha, branch, expected)
		if f.remote[branch] != expected {
			return errors.New("stale info")
		}
		f.remote[branch] = sha
		return nil
	}
	dp.moveBranchFn = func(_, branch, sha string) error {
		f.event("move %s to %s", branch, sha)
		return nil
	}
	dp.createRefFn = func(string, string, string) error { return nil }
	dp.hostGitVersionFn = func(context.Context) (string, error) { return f.gitVersion, nil }
}

// pass runs one PR-review pass, every ticket's poll due.
func (f *restackFixture) pass() {
	f.t.Helper()
	f.now = f.now.Add(2 * time.Minute)
	if err := requestdriver.AdvancePRReview(f.dp, context.Background(), f.dataDir, f.r, f.cfg, f.now); err != nil {
		f.t.Fatalf("AdvancePRReview: %v", err)
	}
}

func (f *restackFixture) did(prefix string) bool {
	return slices.ContainsFunc(f.events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// TestRestackReplaysTheTicketOntoTheEarlierTicketsNewTip: ticket 1's branch
// moved, so ticket 2's commits are replayed onto its new tip on a local
// restack branch, judged by a replay run (the restack branch, the new base as
// diff base, the replay record, no pull request), and pushed with a lease on
// the old head; the run becomes the ticket's PR-head run, the local branch
// follows, and the round is a restack round with its ref.
func TestRestackReplaysTheTicketOntoTheEarlierTicketsNewTip(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.pass()

	want := []string{
		"replay " + c1 + "," + head + " onto " + newTip + " on buildgate/req-1/t2-restack1",
		"run req-1-002-restack1-x",
		"push " + r2 + " to factoryd/run-2 leased on " + head,
		"move factoryd/run-2 to " + r2,
	}
	var got []string
	for _, e := range f.events {
		if !strings.HasPrefix(e, "ready") {
			got = append(got, e)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkReplayRunArgs(t, f.runArgs[0])
	t2 := f.r.Tickets[1]
	if t2.PRHeadRunID() != "req-1-002-restack1-x" || t2.Restack != nil || len(t2.Rounds) != 1 || t2.Rounds[0].Kind != request.RestackRoundKind || !t2.Rounds[0].Pushed {
		t.Errorf("ticket 2 = head %s, restack %+v, rounds %+v", t2.PRHeadRunID(), t2.Restack, t2.Rounds)
	}
	if tag, ok := t2.RoundTagN(1); !ok || tag.Kind != request.RoundTagRestack || tag.SHA != r2 || tag.BaseSHA != newTip {
		t.Errorf("round tag = %+v (%v), want the restack's own ref from the new base", tag, ok)
	}
	if f.r.State != request.StatePRReview {
		t.Errorf("state = %s, want pr_review", f.r.State)
	}
}

// checkReplayRunArgs checks the replay run of restackFixture's restack: on
// the restack branch, from the new base, with no pull request, prior run or
// earlier attempt, and a replay record of ticket 2's commits onto newTip.
func checkReplayRunArgs(t *testing.T, args []string) {
	t.Helper()
	if requestdrivertest.ArgValue(args, "-on-branch") != "buildgate/req-1/t2-restack1" || requestdrivertest.ArgValue(args, "-diff-base") != newTip || slices.Contains(args, "-open-pull-request") || slices.Contains(args, "-prior-run") || slices.Contains(args, "-earlier-attempt") {
		t.Errorf("replay run args = %v", args)
	}
	data, err := os.ReadFile(requestdrivertest.ArgValue(args, "-replay"))
	if err != nil {
		t.Fatal(err)
	}
	var replay run.Replay
	if err := json.Unmarshal(data, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.OfRunID != "run-2" || !slices.Equal(replay.SourceRunIDs, []string{"run-2"}) || replay.UpstreamSHA != oldTip || replay.OntoSHA != newTip || replay.HeadSHA != r2 || len(replay.Commits) != 2 || replay.Commits[1].OriginalPatchID != hexSHA('e') {
		t.Errorf("replay record = %+v", replay)
	}
}

// TestNoRestackWhenTheEarlierTipIsUnderTheHead: ticket 1's tip is an
// ancestor of ticket 2's head: nothing is replayed or pushed.
func TestNoRestackWhenTheEarlierTipIsUnderTheHead(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.under[[2]string{newTip, head}] = true
	f.pass()
	if f.did("replay") || f.did("push") || len(f.r.Tickets[1].Rounds) != 0 {
		t.Fatalf("events %v, rounds %+v: want no restack", f.events, f.r.Tickets[1].Rounds)
	}
}

// mergedTicket1 marks ticket 1 merged into main, at mainTip on origin.
func (f *restackFixture) mergedTicket1() {
	f.r.Tickets[0].PRState = "merged"
	f.remote["main"] = mainTip
	f.save()
}

// TestRestackAfterASquashMergeThenRetargets: ticket 1 was squash-merged into
// main, so ticket 2's stack base is not under main's tip: it is restacked
// onto main, and only once that push is confirmed is its pull request
// retargeted onto main (the next pass), never before.
func TestRestackAfterASquashMergeThenRetargets(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.mergedTicket1()
	f.pass()
	if f.did("retarget") || !f.did("push "+r2+" to factoryd/run-2") {
		t.Fatalf("events %v: want the restack pushed and no retarget yet", f.events)
	}
	if !strings.Contains(f.events[0], "onto "+mainTip) {
		t.Errorf("replayed %q, want onto main's tip", f.events[0])
	}
	st := f.prs[f.r.Tickets[1].PRURL]
	st.HeadSHA = r2
	f.prs[f.r.Tickets[1].PRURL] = st
	f.pass()
	if !f.did("retarget " + f.r.Tickets[1].PRURL + " to main") {
		t.Fatalf("events %v: want the retarget once the restack is confirmed", f.events)
	}
	if strings.Count(strings.Join(f.events, "\n"), "replay ") != 1 {
		t.Errorf("events %v: main's tip, now under the stack base, restacked again", f.events)
	}
}

// TestNoRestackAfterAMergeCommit: ticket 1 merged with a merge commit, so
// ticket 2's stack base is under main's tip: no restack; the retarget runs.
func TestNoRestackAfterAMergeCommit(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.mergedTicket1()
	f.under[[2]string{oldTip, mainTip}] = true
	f.pass()
	if f.did("replay") || !f.did("retarget") {
		t.Fatalf("events %v: want no restack and the retarget", f.events)
	}
}

// TestRestackCascadesInTicketOrder: ticket 1 moved, so ticket 2 needs a
// restack and, once it is pushed, so does ticket 3, onto ticket 2's new
// head. While ticket 2's restack has not run (its poll is not due), ticket 3
// is held: not replayed, and not marked ready.
func TestRestackCascadesInTicketOrder(t *testing.T) {
	f := newRestackFixture(t, 3)
	h3 := hexSHA('4')
	f.saveRun(&run.Run{ID: "run-3", BaseSHA: head, StackBaseSHA: head, ResultSHA: h3, Branch: "factoryd/run-3"})
	t3 := &f.r.Tickets[2]
	t3.Branch, t3.PRState = "factoryd/run-3", "stacked"
	f.remote["factoryd/run-3"] = h3
	f.prs[t3.PRURL] = forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, BaseRefName: "factoryd/run-2", HeadSHA: h3}
	f.ranges[[2]string{head, h3}] = []string{h3}
	f.r.Tickets[1].LastPolledAt = f.now.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	f.save()

	f.pass()
	if f.did("replay") || f.did("ready "+t3.PRURL) {
		t.Fatalf("events %v: ticket 3 moved before ticket 2's restack", f.events)
	}

	f.r.Tickets[1].LastPolledAt = ""
	f.save()
	f.pass()
	var replays []string
	for _, e := range f.events {
		if strings.HasPrefix(e, "replay ") {
			replays = append(replays, e)
		}
	}
	if len(replays) != 2 || !strings.HasSuffix(replays[0], "t2-restack1") || !strings.Contains(replays[1], "onto "+r2+" on buildgate/req-1/t3-restack1") {
		t.Fatalf("replays %v: want ticket 2, then ticket 3 onto ticket 2's restacked head", replays)
	}
}

// TestRestackParksBeforeReplaying: a commit the factory did not make on the
// ticket's branch, a merge commit in its range, or a commit none of its own
// runs judged parks the request with the reason, before anything is
// replayed.
func TestRestackParksBeforeReplaying(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(f *restackFixture)
		reason  string
	}{
		"a human commit on the branch": {func(f *restackFixture) { f.remote["factoryd/run-2"] = hexSHA('9') }, "a commit the factory did not make is on it"},
		"a merge commit":               {func(f *restackFixture) { f.merges = []string{hexSHA('8')} }, "include merge commit"},
		"a commit no run judged": {func(f *restackFixture) {
			f.saveRun(&run.Run{ID: "run-2", BaseSHA: c1, DiffBaseSHA: c1, StackBaseSHA: oldTip, ResultSHA: head, Branch: "factoryd/run-2"})
			f.ranges[[2]string{c1, head}] = []string{head}
		}, "is not the work of any of its own runs"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRestackFixture(t, 2)
			tc.arrange(f)
			f.pass()
			if f.r.State != request.StateResumeReview || !f.r.Resume.Parked() || !strings.Contains(f.r.Resume.Reason, tc.reason) {
				t.Fatalf("state %s, resume %+v: want parked because %q", f.r.State, f.r.Resume, tc.reason)
			}
			if f.did("replay") || f.did("push") {
				t.Errorf("events %v: want nothing replayed or pushed", f.events)
			}
		})
	}
}

// TestRestackConflictParksAndResumeReplaysAgain: a replay that conflicts
// parks the request in resume_review with a reason that says what resolves
// it (the branch under the ticket moving, or cancel) and that a fix pushed
// by hand is refused, nothing built; `factoryd resume` returns it to
// pr_review, and the next pass replays again.
func TestRestackConflictParksAndResumeReplaysAgain(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.replayErr = fmt.Errorf("%w: CONFLICT (content): b.txt", workspace.ErrReplayConflict)
	f.pass()
	if f.r.State != request.StateResumeReview || !f.r.Resume.Parked() || f.r.Resume.Cause != "" || f.r.Tickets[1].Restack != nil || len(f.runArgs) != 0 {
		t.Fatalf("state %s, resume %+v, restack %+v, runs %d: want parked with no cause and no run", f.r.State, f.r.Resume, f.r.Tickets[1].Restack, len(f.runArgs))
	}
	for _, want := range []string{"do not apply cleanly", "once factoryd/run-1 has moved", "refused as a commit the factory did not make", "factoryd cancel req-1"} {
		if !strings.Contains(f.r.Resume.Reason, want) {
			t.Errorf("reason %q does not say %q", f.r.Resume.Reason, want)
		}
	}
	if !strings.Contains(f.r.NextAction(), "factoryd resume req-1") {
		t.Errorf("next action %q does not offer the resume", f.r.NextAction())
	}
	resumed, err := request.ResumeRequest(f.dataDir, "req-1", request.ResumeRound, "alice", f.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.r, f.replayErr = resumed, nil
	f.pass()
	if !f.did("push "+r2) || f.r.Tickets[1].PRHeadRunID() == "run-2" {
		t.Fatalf("events %v: want the replay again and its push", f.events)
	}
}

// TestRestackNotAcceptedHaltsAndPushesNothing: a replay run quarantined on
// the new base halts the request with the reason; nothing is pushed and the
// ticket's PR-head run is unchanged.
func TestRestackNotAcceptedHaltsAndPushesNothing(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.replayEnd = run.StateQuarantined
	f.pass()
	if f.r.State != request.StateHalted || !strings.Contains(f.r.Error, "was not accepted") || !strings.Contains(f.r.Error, "nothing was pushed") {
		t.Fatalf("state %s, error %q: want halted naming the refused restack", f.r.State, f.r.Error)
	}
	if f.did("push") || f.r.Tickets[1].PRHeadRunID() != "run-2" || f.r.Tickets[1].Rounds[0].Outcome != request.RoundQuarantined {
		t.Errorf("events %v, ticket 2 %+v: want nothing pushed, a quarantined restack round", f.events, f.r.Tickets[1])
	}
}

// TestRestackLeaseFailureParks: someone pushed to the ticket's branch after
// the check: the leased push fails, nothing is pushed, and the request parks
// as for a human commit.
func TestRestackLeaseFailureParks(t *testing.T) {
	f := newRestackFixture(t, 2)
	push := f.dp.pushWithLeaseFn
	f.dp.pushWithLeaseFn = func(ctx context.Context, dir, sha, branch, expected string) error {
		f.remote[branch] = hexSHA('9')
		return push(ctx, dir, sha, branch, expected)
	}
	f.pass()
	if f.r.State != request.StateResumeReview || !strings.Contains(f.r.Resume.Reason, "a commit the factory did not make") || f.remote["factoryd/run-2"] != hexSHA('9') {
		t.Fatalf("state %s, resume %+v, branch %s: want parked and the branch untouched", f.r.State, f.r.Resume, f.remote["factoryd/run-2"])
	}
	if f.r.Tickets[1].PRHeadRunID() != "run-2" || f.did("move") {
		t.Errorf("head %s, events %v: want the old PR-head run and the local branch unmoved", f.r.Tickets[1].PRHeadRunID(), f.events)
	}
}

// TestRestackReadBackMismatchKeepsTheOldHead: the push reports success but
// origin does not read the new head back: the request halts, the restack
// run is not the PR-head run, and the local branch is not moved.
func TestRestackReadBackMismatchKeepsTheOldHead(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.dp.pushWithLeaseFn = func(_ context.Context, _, sha, branch, _ string) error {
		f.event("push %s to %s", sha, branch)
		return nil
	}
	f.pass()
	if f.r.State != request.StateHalted || !strings.Contains(f.r.Error, "origin reads") {
		t.Fatalf("state %s, error %q: want halted on the read-back", f.r.State, f.r.Error)
	}
	if t2 := f.r.Tickets[1]; t2.PRHeadRunID() != "run-2" || t2.Rounds[0].Pushed || f.did("move") {
		t.Errorf("ticket 2 head %s, rounds %+v, events %v: want the old head kept", t2.PRHeadRunID(), t2.Rounds, f.events)
	}
}

// TestRestackTurnsAReadyPullRequestToDraftBeforeThePush: a pull request
// ready for review is turned back to draft before the restack's push.
func TestRestackTurnsAReadyPullRequestToDraftBeforeThePush(t *testing.T) {
	f := newRestackFixture(t, 2)
	st := f.prs[f.r.Tickets[1].PRURL]
	st.IsDraft = false
	f.prs[f.r.Tickets[1].PRURL] = st
	f.r.Tickets[1].PRState = "ready"
	f.save()
	f.pass()
	draft := slices.Index(f.events, "draft "+f.r.Tickets[1].PRURL)
	push := slices.IndexFunc(f.events, func(e string) bool { return strings.HasPrefix(e, "push ") })
	if draft < 0 || push < 0 || draft > push || f.r.Tickets[1].PRState != "draft" {
		t.Fatalf("events %v, PR state %q: want the draft before the push", f.events, f.r.Tickets[1].PRState)
	}
}

// TestNoRetargetOrReadyFlipWhileARestackCannotBeDecided: whether ticket 2
// needs a restack cannot be told (main's tip cannot be fetched): its pull
// request is neither retargeted off the merged ticket 1 nor marked ready.
func TestNoRetargetOrReadyFlipWhileARestackCannotBeDecided(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.mergedTicket1()
	f.dp.fetchBranchCommitFn = func(context.Context, string, string, string) error { return errors.New("network down") }
	f.pass()
	if f.did("retarget") || f.did("ready "+f.r.Tickets[1].PRURL) || f.did("replay") {
		t.Fatalf("events %v: want no retarget, ready flip or replay", f.events)
	}
}

// TestInterruptedRestackWhosePushLandedIsRecorded: the worker stopped after
// the restack's push and before its round was recorded: the next pass finds
// the branch at the accepted replay run's result and records it as pushed.
// An interrupted restack whose run never finished is dropped.
func TestInterruptedRestackWhosePushLandedIsRecorded(t *testing.T) {
	f := newRestackFixture(t, 2)
	replay := &run.Replay{RequestID: "req-1", TicketIndex: 2, OfRunID: "run-2", OntoSHA: newTip, HeadSHA: r2}
	f.saveRun(&run.Run{ID: "req-1-002-restack1-x", TicketIndex: 2, BaseSHA: r2, ResultSHA: r2, Replay: replay})
	f.remote["factoryd/run-2"] = r2
	f.r.Tickets[1].Restack = &request.TicketRestack{N: 1, Branch: "buildgate/req-1/t2-restack1", OntoSHA: newTip, RunID: "req-1-002-restack1-x"}
	f.save()
	f.pass()
	t2 := f.r.Tickets[1]
	if t2.Restack != nil || t2.PRHeadRunID() != "req-1-002-restack1-x" || f.did("replay") || !f.did("move factoryd/run-2 to "+r2) {
		t.Fatalf("ticket 2 %+v, events %v: want the landed push recorded", t2, f.events)
	}

	g := newRestackFixture(t, 2)
	g.r.Tickets[1].Restack = &request.TicketRestack{N: 1, Branch: "buildgate/req-1/t2-restack1", OntoSHA: newTip}
	g.save()
	g.pass()
	if g.r.Tickets[1].Restack != nil || g.did("replay") || len(g.r.Tickets[1].Rounds) != 0 {
		t.Fatalf("ticket 2 %+v, events %v: want the unfinished restack dropped, nothing else this pass", g.r.Tickets[1], g.events)
	}
	g.pass()
	if !g.did("push " + r2) {
		t.Fatalf("events %v: want the next pass to restack", g.events)
	}
}

// TestAStaleKeptBuildOfAStackedTicketIsNotResumed: ticket 2's lost build was
// built on ticket 1's old head; ticket 1's head moved since, so a resume of
// it is refused, pointing at a rebuild from ticket 1's current head; and a
// retry of an attempt built on the old head does not continue on its
// branch, but chains from ticket 1's current PR-head run.
func TestAStaleKeptBuildOfAStackedTicketIsNotResumed(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.saveRun(&run.Run{ID: "lost-2", TicketIndex: 2, BaseSHA: oldTip, StackBaseSHA: oldTip, ResultSHA: head, Branch: "factoryd/lost-2"})
	f.r.State, f.r.TicketIndex = request.StateBuilding, 2
	f.r.Resume = &request.ResumeInfo{FromState: request.StateBuilding, LostRunID: "lost-2", Generation: 1}
	f.save()
	gate := requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}
	reasons, err := gate.RequestResumeRefusals(context.Background(), f.dataDir, "", f.r)
	if err != nil || len(reasons) != 1 || !strings.Contains(reasons[0], "ticket 1's old head") || !strings.Contains(reasons[0], "factoryd resume -from scratch req-1") {
		t.Fatalf("refusals = %v, %v; want the stale build refused, pointing at a rebuild", reasons, err)
	}

}

// TestARetryOfAStackedTicketOnTheOldHeadChainsFromTheCurrentHead: ticket 2
// was quarantined on a build stacked on ticket 1's first head; a review
// round of ticket 1 was pushed since. A retry does not continue on the
// quarantined attempt's branch (which sits on the old head) but chains from
// ticket 1's current PR-head run; with ticket 1 unmoved it would continue.
func TestARetryOfAStackedTicketOnTheOldHeadChainsFromTheCurrentHead(t *testing.T) {
	for _, moved := range []bool{true, false} {
		t.Run(fmt.Sprintf("ticket 1 moved %v", moved), func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
			var built [][]string
			r := buildFirstTicket(t, dp, dataDir, id, acceptingStackedBuildRunner(t, dataDir, &built))
			first := "result-" + r.Tickets[0].RunID
			earlierPROpenAt(dp, first)
			quarantining := func(_ context.Context, args []string, onReady func(*run.Run)) error {
				built = append(built, args)
				ticket := requestdrivertest.ArgValue(args, "-ticket")
				onReady(&run.Run{ID: ticket})
				rec := requestdrivertest.QuarantinedOn(t, dataDir, ticket, "factoryd/"+ticket, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "lint")
				sum, err := evidence.SHA256File(requestdrivertest.ArgValue(args, "-spec"))
				if err != nil {
					t.Fatal(err)
				}
				rec.RequestID, rec.TicketIndex, rec.SpecSHA256, rec.StackBaseSHA = id, 2, sum, first
				if err := handoff.Sync(rec, dataDir); err != nil {
					t.Fatal(err)
				}
				return rec.Save(dataDir)
			}
			if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, quarantining, time.Now()); err != nil {
				t.Fatal(err)
			}
			if r.State != request.StateQuarantined {
				t.Fatalf("state %s, want ticket 2 quarantined", r.State)
			}
			current := first
			if moved {
				current = "result-ticket-1-round"
				addPushedRound(t, dataDir, &r.Tickets[0], "req-1-001-review1", "factoryd/req-1-001", current, "")
				if err := r.Save(dataDir); err != nil {
					t.Fatal(err)
				}
			}
			retried, err := request.Retry(dataDir, id, "alice", "try again", time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			earlierPROpenAt(dp, current)
			if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, retried, stackedCfg, quarantining, time.Now()); err != nil {
				t.Fatal(err)
			}
			last := built[len(built)-1]
			onBranch, prior := requestdrivertest.ArgValue(last, "-on-branch"), requestdrivertest.ArgValue(last, "-prior-run")
			if moved && (onBranch != "" || prior != "req-1-001-review1") {
				t.Fatalf("retry -on-branch %q -prior-run %q, want no branch and ticket 1's current PR-head run", onBranch, prior)
			}
			if !moved && onBranch == "" {
				t.Fatalf("retry args %v: with ticket 1 unmoved the retry continues on the attempt's branch", last)
			}
		})
	}
}

// TestRestackRefusesAReplayThatIsNotTheSameChange: a replayed commit whose
// patch id differs from its original's (a whitespace change, say), or an
// original that changed something replayed as a commit that changes
// nothing, parks the request with the reason before any replay run;
// nothing is pushed.
func TestRestackRefusesAReplayThatIsNotTheSameChange(t *testing.T) {
	for name, tc := range map[string]struct {
		ids    map[string]string
		reason string
	}{
		"a changed patch":   {map[string]string{c1: hexSHA('d'), r1: hexSHA('f'), head: hexSHA('e'), r2: hexSHA('e')}, "are not the same change"},
		"an emptied commit": {map[string]string{c1: hexSHA('d'), r1: "", head: hexSHA('e'), r2: hexSHA('e')}, "the new base already holds the change"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRestackFixture(t, 2)
			f.dp.patchIDFn = func(_, sha string) (string, error) { return tc.ids[sha], nil }
			f.pass()
			if f.r.State != request.StateResumeReview || !f.r.Resume.Parked() || !strings.Contains(f.r.Resume.Reason, tc.reason) || !strings.Contains(f.r.Resume.Reason, shortOf(c1)) {
				t.Fatalf("state %s, resume %+v: want parked because %q", f.r.State, f.r.Resume, tc.reason)
			}
			if len(f.runArgs) != 0 || f.did("push") || f.r.Tickets[1].Restack != nil {
				t.Errorf("runs %d, events %v, restack %+v: want no replay run and nothing pushed", len(f.runArgs), f.events, f.r.Tickets[1].Restack)
			}
		})
	}
}

// shortOf is a commit id as the operator's sentences shorten it.
func shortOf(sha string) string { return sha[:12] }

// TestRestackPushThatLandedButReportedAnErrorIsConfirmed: the leased push
// landed and then reported an error (a timeout): origin reads the replay
// run's result back, so the restack is confirmed, not taken for a human
// commit.
func TestRestackPushThatLandedButReportedAnErrorIsConfirmed(t *testing.T) {
	f := newRestackFixture(t, 2)
	push := f.dp.pushWithLeaseFn
	f.dp.pushWithLeaseFn = func(ctx context.Context, dir, sha, branch, expected string) error {
		if err := push(ctx, dir, sha, branch, expected); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	f.pass()
	t2 := f.r.Tickets[1]
	if f.r.State != request.StatePRReview || t2.PRHeadRunID() != "req-1-002-restack1-x" || !t2.Rounds[0].Pushed || !f.did("move factoryd/run-2 to "+r2) {
		t.Fatalf("state %s (%s), ticket 2 head %s, rounds %+v, events %v: want the landed push confirmed", f.r.State, f.r.Error, t2.PRHeadRunID(), t2.Rounds, f.events)
	}
}

// TestRestackReadBackFailureIsConfirmedAfterTheRetry: origin could not be
// read back after the push: the request halts with the restack unconfirmed
// and the old head kept; after `factoryd retry` the next pass reads origin
// at the replay run's result and records the push, without replaying again.
func TestRestackReadBackFailureIsConfirmedAfterTheRetry(t *testing.T) {
	f := newRestackFixture(t, 2)
	push, read := f.dp.pushWithLeaseFn, f.dp.remoteBranchHeadSHAFn
	failNextRead := false
	f.dp.pushWithLeaseFn = func(ctx context.Context, dir, sha, branch, expected string) error {
		failNextRead = true
		return push(ctx, dir, sha, branch, expected)
	}
	f.dp.remoteBranchHeadSHAFn = func(ctx context.Context, dir, branch string) (string, error) {
		if failNextRead && branch == "factoryd/run-2" {
			failNextRead = false
			return "", errors.New("network down")
		}
		return read(ctx, dir, branch)
	}
	f.pass()
	if f.r.State != request.StateHalted || !strings.Contains(f.r.Error, "could not be read back") || f.r.Tickets[1].PRHeadRunID() != "run-2" || f.did("move") {
		t.Fatalf("state %s, error %q, head %s, events %v: want halted with the old head kept", f.r.State, f.r.Error, f.r.Tickets[1].PRHeadRunID(), f.events)
	}
	retried, err := request.Retry(f.dataDir, "req-1", "alice", "origin is reachable again", f.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.r = retried
	f.pass()
	t2 := f.r.Tickets[1]
	if f.r.State != request.StatePRReview || t2.PRHeadRunID() != "req-1-002-restack1-x" || !f.did("move factoryd/run-2 to "+r2) {
		t.Fatalf("state %s (%s), head %s, events %v: want the push recorded after the retry", f.r.State, f.r.Error, t2.PRHeadRunID(), f.events)
	}
	if strings.Count(strings.Join(f.events, "\n"), "replay ") != 1 || len(f.runArgs) != 1 {
		t.Errorf("events %v: the restack was replayed or judged again", f.events)
	}
}

// TestAReplayRunStillRunningIsAdoptedNotStartedAgain: a pass finds a
// restack in flight whose replay run has not ended (the worker that started
// it stopped): no other replay starts and nothing is pushed while it runs;
// once it ends accepted, with origin still at the old head, the next pass
// pushes it.
func TestAReplayRunStillRunningIsAdoptedNotStartedAgain(t *testing.T) {
	f := newRestackFixture(t, 2)
	id := "req-1-002-restack1-x"
	replay := &run.Replay{RequestID: "req-1", TicketIndex: 2, OfRunID: "run-2", SourceRunIDs: []string{"run-2"}, UpstreamSHA: oldTip, SourceHeadSHA: head, OntoSHA: newTip, HeadSHA: r2}
	f.saveRun(&run.Run{ID: id, TicketIndex: 2, BaseSHA: r2, ResultSHA: r2, DiffBaseSHA: newTip, Branch: "buildgate/req-1/t2-restack1", Replay: replay, Attempts: []run.Attempt{{Kind: "build", ReplayOf: "run-2"}}})
	rec, err := run.Load(f.dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	rec.State = run.StateVerifying
	if err := rec.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	f.r.Tickets[1].Restack = &request.TicketRestack{N: 1, Branch: "buildgate/req-1/t2-restack1", OntoSHA: newTip, RunID: id}
	f.save()
	f.pass()
	if f.did("replay") || len(f.runArgs) != 0 || f.did("push") || f.r.Tickets[1].Restack == nil {
		t.Fatalf("events %v, runs %d, restack %+v: want the running replay run adopted, nothing else", f.events, len(f.runArgs), f.r.Tickets[1].Restack)
	}
	rec.State = run.StateAccepted
	if err := rec.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	f.pass()
	t2 := f.r.Tickets[1]
	if f.did("replay") || len(f.runArgs) != 0 || !f.did("push "+r2+" to factoryd/run-2 leased on "+head) || t2.PRHeadRunID() != id || t2.Restack != nil {
		t.Fatalf("events %v, runs %d, ticket 2 %+v: want the adopted run pushed, not replayed again", f.events, len(f.runArgs), t2)
	}
}

// TestARestackThatCannotBeToldIsShownAndReminded: whether ticket 2 needs a
// restack cannot be told (ticket 1's branch cannot be fetched): the hold is
// recorded on the ticket, named in the request's next action, and reminded
// once the reminder interval has passed; it clears once a poll can tell.
func TestARestackThatCannotBeToldIsShownAndReminded(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.cfg.HitlReminderInterval = time.Hour
	f.dp.fetchBranchCommitFn = func(context.Context, string, string, string) error { return errors.New("network down") }
	f.pass()
	hold := f.r.Tickets[1].RestackHold
	if hold == nil || !strings.Contains(hold.On, "network down") || !strings.Contains(f.r.NextAction(), "whether it needs a restack cannot be told") || f.did("replay") || f.did("ready "+f.r.Tickets[1].PRURL) {
		t.Fatalf("hold %+v, next action %q, events %v: want the hold shown and nothing done", hold, f.r.NextAction(), f.events)
	}
	if hold.RemindedAt != "" {
		t.Errorf("reminded at %s, before the interval", hold.RemindedAt)
	}
	f.now = f.now.Add(2 * time.Hour)
	f.pass()
	if f.r.Tickets[1].RestackHold.RemindedAt == "" {
		t.Fatal("no reminder once the hold outlasted the interval")
	}
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(f.dataDir, f.r.ID))
	if err != nil || !strings.Contains(string(log), "whether it needs a restack cannot be told") {
		t.Errorf("notifications %s (%v): want the hold's reminder", log, err)
	}
	f.dp.fetchBranchCommitFn = func(context.Context, string, string, string) error { return nil }
	f.pass()
	if f.r.Tickets[1].RestackHold != nil || !f.did("push "+r2) {
		t.Fatalf("hold %+v, events %v: want the hold cleared and the restack run", f.r.Tickets[1].RestackHold, f.events)
	}
}

// TestAStackedPullRequestWithNoStackBaseIsHeld: ticket 2's pull request is
// on ticket 1's branch, and its PR-head run records no stack base: whether
// it needs a restack cannot be told, so it is held, never read as needing
// none; a ticket 2 not built stacked (its pull request on main) is not.
func TestAStackedPullRequestWithNoStackBaseIsHeld(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.saveRun(&run.Run{ID: "run-2", BaseSHA: oldTip, ResultSHA: head, Branch: "factoryd/run-2"})
	f.mergedTicket1()
	f.pass()
	if hold := f.r.Tickets[1].RestackHold; hold == nil || !strings.Contains(hold.On, "records no stack base") || f.did("retarget") || f.did("replay") {
		t.Fatalf("hold %+v, events %v: want ticket 2 held, not retargeted", hold, f.events)
	}

	g := newRestackFixture(t, 2)
	g.saveRun(&run.Run{ID: "run-2", BaseSHA: mainTip, ResultSHA: head, Branch: "factoryd/run-2"})
	g.r.Tickets[1].PRState, g.r.Tickets[1].PRBase = "draft", "main"
	g.prs[g.r.Tickets[1].PRURL] = forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, BaseRefName: "main", HeadSHA: head}
	g.save()
	g.pass()
	if g.r.Tickets[1].RestackHold != nil || g.did("replay") {
		t.Fatalf("hold %+v, events %v: a ticket not built stacked is neither held nor restacked", g.r.Tickets[1].RestackHold, g.events)
	}
}

// TestRestackNamesAnOldSourceRunsTicketUnderItsLock: a source run recorded
// before runs named their ticket is given the ticket's index, and a write
// made to its record after it was read (here, while its range was listed)
// is kept: the record is reloaded under its lock and only TicketIndex set.
func TestRestackNamesAnOldSourceRunsTicketUnderItsLock(t *testing.T) {
	f := newRestackFixture(t, 2)
	rec, err := run.Load(f.dataDir, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	rec.TicketIndex = 0
	if err := rec.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	list := f.dp.revListRangeFn
	f.dp.revListRangeFn = func(dir, from, to string) ([]string, []string, error) {
		if cur, err := run.Load(f.dataDir, "run-2"); err == nil && cur.TicketIndex == 0 && cur.PullRequestURL == "" {
			cur.PullRequestURL = "https://github.com/acme/widget/pull/102"
			if err := cur.Save(f.dataDir); err != nil {
				t.Fatal(err)
			}
		}
		return list(dir, from, to)
	}
	f.pass()
	got, err := run.Load(f.dataDir, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.TicketIndex != 2 || got.PullRequestURL != "https://github.com/acme/widget/pull/102" {
		t.Fatalf("run-2 ticket %d, pull request %q: want its ticket named and the later write kept", got.TicketIndex, got.PullRequestURL)
	}
	if !f.did("push " + r2) {
		t.Errorf("events %v: want the restack to go on", f.events)
	}
}

// landedReplayRun saves ticket 2's accepted replay run of restack 1, whose
// result is r2.
func (f *restackFixture) landedReplayRun() string {
	f.t.Helper()
	id := "req-1-002-restack1-x"
	replay := &run.Replay{RequestID: "req-1", TicketIndex: 2, OfRunID: "run-2", SourceRunIDs: []string{"run-2"}, UpstreamSHA: oldTip, SourceHeadSHA: head, OntoSHA: newTip, HeadSHA: r2}
	f.saveRun(&run.Run{ID: id, TicketIndex: 2, BaseSHA: r2, ResultSHA: r2, DiffBaseSHA: newTip, StackBaseSHA: newTip, Branch: "buildgate/req-1/t2-restack1", Replay: replay})
	return id
}

// TestAnInterruptedRestackIsKeptWhileOriginCannotBeRead: the worker stopped
// after the restack's push landed and before it was recorded, and the next
// pass cannot read origin: the restack is kept and nothing is decided; the
// pass after reads origin at the replay run's result and records the push,
// never taking it for a human commit.
func TestAnInterruptedRestackIsKeptWhileOriginCannotBeRead(t *testing.T) {
	f := newRestackFixture(t, 2)
	id := f.landedReplayRun()
	f.remote["factoryd/run-2"] = r2
	f.r.Tickets[1].Restack = &request.TicketRestack{N: 1, Branch: "buildgate/req-1/t2-restack1", OntoSHA: newTip, RunID: id}
	f.save()
	read := f.dp.remoteBranchHeadSHAFn
	failed := false
	f.dp.remoteBranchHeadSHAFn = func(ctx context.Context, dir, branch string) (string, error) {
		if branch == "factoryd/run-2" && !failed {
			failed = true
			return "", errors.New("network down")
		}
		return read(ctx, dir, branch)
	}
	f.pass()
	if f.r.Tickets[1].Restack == nil || f.r.State != request.StatePRReview || f.did("replay") || f.did("move") {
		t.Fatalf("state %s (%v), restack %+v, events %v: want the restack kept and nothing decided", f.r.State, f.r.Resume, f.r.Tickets[1].Restack, f.events)
	}
	f.pass()
	t2 := f.r.Tickets[1]
	if f.r.State != request.StatePRReview || t2.Restack != nil || t2.PRHeadRunID() != id || !f.did("move factoryd/run-2 to "+r2) || f.did("replay") {
		t.Fatalf("state %s (%v), ticket 2 head %s, events %v: want the landed push confirmed, not parked", f.r.State, f.r.Resume, t2.PRHeadRunID(), f.events)
	}
}

// TestRestackParksOnAGitOlderThanTheFloor: the host's git is older than the
// floor a restack's git calls need: the request parks naming the installed
// version, the floor and the remedy, before anything is replayed.
func TestRestackParksOnAGitOlderThanTheFloor(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.gitVersion = "git version 2.39.5 (Apple Git-154)"
	f.pass()
	if f.r.State != request.StateResumeReview || f.r.Resume == nil {
		t.Fatalf("state %s, error %q: want parked in resume_review", f.r.State, f.r.Error)
	}
	for _, want := range []string{"2.39", workspace.GitFloor, "upgrade git"} {
		if !strings.Contains(strings.ToLower(f.r.Resume.Reason), strings.ToLower(want)) {
			t.Errorf("park reason %q does not name %q", f.r.Resume.Reason, want)
		}
	}
	if f.did("replay") || f.did("push") || len(f.runArgs) != 0 {
		t.Errorf("events %v: want nothing replayed, run or pushed", f.events)
	}
}

// TestARestackWhoseReplayRunNeverStartsHaltsAfterThree: a replay run that
// never starts is tried again on the next poll, and the third in a row
// halts the request naming the error and the retry; after `factoryd retry`
// the count starts again, and a replay run that starts is pushed.
func TestARestackWhoseReplayRunNeverStartsHaltsAfterThree(t *testing.T) {
	f := newRestackFixture(t, 2)
	runTicket := f.dp.runTicketFn
	f.dp.runTicketFn = func(context.Context, []string, func(*run.Run)) error {
		f.event("run (never started)")
		return errors.New("temporal unreachable")
	}
	for i := 1; i <= 2; i++ {
		f.pass()
		if f.r.State != request.StatePRReview {
			t.Fatalf("pass %d: state %s (%s): want still in pr_review", i, f.r.State, f.r.Error)
		}
	}
	f.pass()
	if f.r.State != request.StateHalted || !strings.Contains(f.r.Error, "temporal unreachable") || !strings.Contains(f.r.Error, "`factoryd retry req-1` restacks again") {
		t.Fatalf("state %s, error %q: want halted naming the error and the retry", f.r.State, f.r.Error)
	}
	retried, err := request.Retry(f.dataDir, "req-1", "alice", "temporal is back", f.now.Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.r = retried
	f.pass()
	if f.r.State != request.StatePRReview {
		t.Fatalf("after the retry: state %s (%s): want one start failure counted, not halted", f.r.State, f.r.Error)
	}
	f.dp.runTicketFn = runTicket
	f.pass()
	if t2 := f.r.Tickets[1]; f.r.State != request.StatePRReview || !strings.HasPrefix(t2.PRHeadRunID(), "req-1-002-restack") || !f.did("push "+r2) {
		t.Fatalf("state %s (%s), head %s, events %v: want the restack pushed", f.r.State, f.r.Error, t2.PRHeadRunID(), f.events)
	}
}

// TestRestackRecordsADraftPullRequestAsDraft: GitHub already has the pull
// request draft while the ticket still records it ready (a crash lost the
// save after the factory's flip): the restack records it as draft before
// the push, so the merge-readiness check sees the draft, even when the push
// then fails.
func TestRestackRecordsADraftPullRequestAsDraft(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.r.Tickets[1].PRState = "ready"
	f.save()
	f.dp.pushWithLeaseFn = func(context.Context, string, string, string, string) error {
		f.remote["factoryd/run-2"] = hexSHA('9')
		return errors.New("stale info")
	}
	f.pass()
	if f.r.Tickets[1].PRState != "draft" || f.did("draft") {
		t.Fatalf("PR state %q, events %v: want it recorded draft, with no flip", f.r.Tickets[1].PRState, f.events)
	}
}

// TestRestackTurnsAPullRequestMarkedReadyDuringThePushBackToDraft: a person
// marks the pull request ready while the restack's push lands: once the
// push is confirmed the pull request is turned back to draft.
func TestRestackTurnsAPullRequestMarkedReadyDuringThePushBackToDraft(t *testing.T) {
	f := newRestackFixture(t, 2)
	pr := f.r.Tickets[1].PRURL
	push := f.dp.pushWithLeaseFn
	f.dp.pushWithLeaseFn = func(ctx context.Context, dir, sha, branch, expected string) error {
		st := f.prs[pr]
		st.IsDraft = false
		f.prs[pr] = st
		return push(ctx, dir, sha, branch, expected)
	}
	f.pass()
	pushAt := slices.IndexFunc(f.events, func(e string) bool { return strings.HasPrefix(e, "push ") })
	draftAt := slices.Index(f.events, "draft "+pr)
	if state := f.r.Tickets[1].PRState; pushAt < 0 || draftAt < pushAt || (state != "draft" && state != "stacked") || !f.prs[pr].IsDraft {
		t.Fatalf("events %v, PR state %q: want the pull request turned back to draft after the push", f.events, f.r.Tickets[1].PRState)
	}
}

// TestALandedRestackIsConfirmedWhenNoRestackIsNeeded: an accepted replay
// run's push landed and was not confirmed, and ticket 1's branch has since
// moved so that ticket 2 needs no restack: the poll reads the pull request
// at the replay run's result and records the push, and the run becomes the
// PR head.
func TestALandedRestackIsConfirmedWhenNoRestackIsNeeded(t *testing.T) {
	f := newRestackFixture(t, 2)
	id := f.landedReplayRun()
	t2 := &f.r.Tickets[1]
	t2.Rounds = []request.Round{{Index: 1, Kind: request.RestackRoundKind, RunID: id, Outcome: request.RoundAccepted}}
	f.remote["factoryd/run-2"] = r2
	f.remote["factoryd/run-1"] = oldTip
	f.under[[2]string{oldTip, head}] = true
	st := f.prs[t2.PRURL]
	st.HeadSHA = r2
	f.prs[t2.PRURL] = st
	f.save()
	f.pass()
	t2 = &f.r.Tickets[1]
	if !t2.Rounds[0].Pushed || t2.PRHeadRunID() != id || !f.did("move factoryd/run-2 to "+r2) || f.did("replay") || f.r.State != request.StatePRReview {
		t.Fatalf("state %s (%v), ticket 2 rounds %+v, events %v: want the landed push recorded", f.r.State, f.r.Resume, t2.Rounds, f.events)
	}
}

// TestRestackPushesOnlyAResultOnTheReplayedHead: a replay run whose result
// does not descend from the replayed head it judged halts the request and
// pushes nothing; one whose result is a commit on top of it (the evidence
// step's commit of what verification left) is pushed.
func TestRestackPushesOnlyAResultOnTheReplayedHead(t *testing.T) {
	onTop := hexSHA('c')
	for _, descends := range []bool{false, true} {
		f := newRestackFixture(t, 2)
		runTicket := f.dp.runTicketFn
		f.dp.runTicketFn = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
			if err := runTicket(ctx, args, onReady); err != nil {
				return err
			}
			rec, err := run.Load(f.dataDir, requestdrivertest.ArgValue(args, "-ticket")+"-x")
			if err != nil {
				return err
			}
			rec.ResultSHA = onTop
			return rec.Save(f.dataDir)
		}
		f.under[[2]string{r2, onTop}] = descends
		f.under[[2]string{newTip, onTop}] = true
		f.pass()
		if !descends {
			if f.r.State != request.StateHalted || !strings.Contains(f.r.Error, "does not descend from the replayed head") || f.did("push") {
				t.Errorf("state %s, error %q, events %v: want halted with nothing pushed", f.r.State, f.r.Error, f.events)
			}
			continue
		}
		if f.r.State != request.StatePRReview || !f.did("push "+onTop) || f.r.Tickets[1].PRHeadRunID() != "req-1-002-restack1-x" {
			t.Errorf("state %s (%s), events %v: want the result on the replayed head pushed", f.r.State, f.r.Error, f.events)
		}
	}
}
