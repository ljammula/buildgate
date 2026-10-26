package workflow

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/meter"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

// replayActivityFixture is a real replay: in one repository, the ticket's
// accepted run ("source") committed c1 and c2 on the earlier ticket's tip U;
// the base then moved to O; and the replay run's worktree is a restack branch
// at O with c1 and c2 cherry-picked onto it (r1, r2). The replay run's own
// record holds the replay, written from git as the host writes it.
type replayActivityFixture struct {
	t       *testing.T
	acts    *Activities
	input   RunWorkflowInput
	dir     string
	creates int

	upstream, c1, c2, onto, r1, r2 string
	// replay is the record apply writes on the replay run; selfRequest and
	// selfTicket the request and ticket it names.
	replay      *run.Replay
	selfRequest string
	selfTicket  int
	// inputReplay, when set, is the workflow input's replay instead of the
	// record's.
	inputReplay *ReplayInput
}

func newReplayActivityFixture(t *testing.T) *replayActivityFixture {
	t.Helper()
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"built"}}}
	acts, input, _ := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	f := &replayActivityFixture{t: t, acts: acts, input: input, dir: input.WorkspacePath, selfRequest: "req-1", selfTicket: 2}
	rt.onCreate = func(sandbox.SandboxRequest) { f.creates++ }
	f.upstream = f.git("rev-parse", "HEAD")
	f.git("checkout", "-q", "-b", "factoryd/ticket-2")
	f.c1 = f.commit("a.txt", "one\n", "ticket 2: a")
	f.c2 = f.commit("b.txt", "two\n", "ticket 2: b")
	f.git("checkout", "-q", "-b", "earlier-ticket-new-tip", f.upstream)
	f.onto = f.commit("earlier.txt", "fixed\n", "ticket 1: review fix")
	f.git("checkout", "-q", "-b", "buildgate/req-1/t2-restack1")
	f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "cherry-pick", f.c1, f.c2)
	f.r2 = f.git("rev-parse", "HEAD")
	f.r1 = f.git("rev-parse", "HEAD~1")
	f.input.IsolatedRepoDir = f.dir
	f.input.RunID = "replay-run"
	f.input.OnBranch = "buildgate/req-1/t2-restack1"
	f.input.DiffBaseSHA = f.onto
	f.replay = f.replayOf("source", []string{"source"}, f.upstream, f.c2, f.onto, f.r2)
	f.input.Replay = &ReplayInput{OfRunID: "source", SourceRunIDs: []string{"source"}, OntoSHA: f.onto, HeadSHA: f.r2}
	f.saveSource(&run.Run{})
	return f
}

func (f *replayActivityFixture) git(args ...string) string {
	f.t.Helper()
	return gitIn(f.t, f.dir, args...)
}

// commit writes name and commits it, returning the commit.
func (f *replayActivityFixture) commit(name, content, msg string) string {
	f.t.Helper()
	writeFile(f.t, filepath.Join(f.dir, name), content)
	f.git("add", "-A")
	f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

// replayOf builds the record of replaying upstream..sourceHead onto onto,
// ending at head, from git: the pairs in order, each with the patch ids git
// computes for it (testPatchID, computed apart from the code under test).
func (f *replayActivityFixture) replayOf(of string, sources []string, upstream, sourceHead, onto, head string) *run.Replay {
	f.t.Helper()
	originals := strings.Fields(f.git("rev-list", "--reverse", upstream+".."+sourceHead))
	replayed := strings.Fields(f.git("rev-list", "--reverse", onto+".."+head))
	if len(originals) != len(replayed) {
		f.t.Fatalf("fixture: %d originals, %d replayed", len(originals), len(replayed))
	}
	rp := &run.Replay{RequestID: "req-1", TicketIndex: 2, OfRunID: of, SourceRunIDs: sources, UpstreamSHA: upstream, SourceHeadSHA: sourceHead, OntoSHA: onto, HeadSHA: head}
	for i := range originals {
		rp.Commits = append(rp.Commits, run.ReplayedCommit{Original: originals[i], Replayed: replayed[i], OriginalPatchID: testPatchID(f.t, f.dir, originals[i]), ReplayedPatchID: testPatchID(f.t, f.dir, replayed[i])})
	}
	return rp
}

// testPatchID is `git show <sha> | git patch-id --stable`.
func testPatchID(t *testing.T, dir, sha string) string {
	t.Helper()
	show, err := exec.Command("git", "-C", dir, "show", "--format=", sha).Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "patch-id", "--stable")
	cmd.Stdin = strings.NewReader(string(show))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// saveSource saves r as a source run and its release decision (allowed).
// Unset fields default to the ticket's accepted, sandboxed run "source" of
// request req-1 ticket 2, built on U with result c2.
func (f *replayActivityFixture) saveSource(r *run.Run) *run.Run {
	f.t.Helper()
	if r.ID == "" {
		r.ID = "source"
	}
	if r.State == "" {
		r.State = run.StateAccepted
	}
	if r.ProjectPath == "" {
		r.ProjectPath = f.dir
	}
	if r.RequestID == "" {
		r.RequestID = "req-1"
	}
	if r.TicketIndex == 0 {
		r.TicketIndex = 2
	}
	if r.BaseSHA == "" {
		r.BaseSHA = f.upstream
	}
	if r.StackBaseSHA == "" && r.Replay == nil {
		r.StackBaseSHA = f.upstream
	}
	if r.ResultSHA == "" {
		r.ResultSHA = f.c2
	}
	if r.Attempts == nil {
		r.Attempts = []run.Attempt{{Kind: "build", ExitCode: 0, ImageDigest: "sha256:worker", Harness: "codex", RelayRoute: "codex", RelayConsumedCostMicroUSD: 70, LogPath: "/source/build.log", RelayCalls: &meter.Calls{Answered: 9}}}
	}
	if err := r.Save(f.dataDir()); err != nil {
		f.t.Fatal(err)
	}
	f.saveDecision(r, release.Decision{Allowed: true})
	return r
}

// saveDecision records d as r's release decision.
func (f *replayActivityFixture) saveDecision(r *run.Run, d release.Decision) {
	f.t.Helper()
	d.RunID, d.Project, d.Evaluated = r.ID, release.ProjectOf(r), "2026-10-25T00:00:00Z"
	if err := release.SaveDecision(f.dataDir(), d); err != nil {
		f.t.Fatal(err)
	}
}

func (f *replayActivityFixture) dataDir() string { return f.acts.dataDirFor(f.input) }

// apply writes the replay run's own record (f.replay, f.selfRequest,
// f.selfTicket), sets the workflow input's replay from it unless
// f.inputReplay says otherwise, and runs ApplyReplayActivity.
func (f *replayActivityFixture) apply() (BuildActivityResult, error) {
	f.t.Helper()
	self := &run.Run{ID: f.input.RunID, State: run.StateSliceRunning, ProjectPath: f.dir, RequestID: f.selfRequest, TicketIndex: f.selfTicket, Replay: f.replay}
	if err := self.Save(f.dataDir()); err != nil {
		f.t.Fatal(err)
	}
	f.input.Replay = f.inputReplay
	if f.input.Replay == nil {
		f.input.Replay = &ReplayInput{OfRunID: f.replay.OfRunID, SourceRunIDs: f.replay.SourceRunIDs, OntoSHA: f.replay.OntoSHA, HeadSHA: f.replay.HeadSHA}
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.ApplyReplayActivity)
	val, err := env.ExecuteActivity(f.acts.ApplyReplayActivity, f.input)
	if err != nil {
		return BuildActivityResult{}, err
	}
	var res BuildActivityResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

// TestApplyReplayCarriesTheSourceBuildAndLaunchesNothing: a real replay (the
// ticket's accepted run's commits cherry-picked onto a new base) passes the
// host's checks, so the step returns one passed build attempt carried from
// the source run (its image, harness and route, none of its spend), marked
// ReplayOf, and no sandbox is created, however often it runs.
func TestApplyReplayCarriesTheSourceBuildAndLaunchesNothing(t *testing.T) {
	f := newReplayActivityFixture(t)
	for range 2 {
		res, err := f.apply()
		if err != nil {
			t.Fatalf("ApplyReplayActivity: %v", err)
		}
		if res.Result.ExitCode != 0 || len(res.Attempts) != 1 {
			t.Fatalf("result = %+v, want one passed attempt", res)
		}
		a := res.Attempts[0]
		if a.Kind != "build" || a.ReplayOf != "source" || a.ImageDigest != "sha256:worker" || a.Harness != "codex" || a.RelayRoute != "codex" {
			t.Errorf("attempt = %+v, want the source's build marked ReplayOf", a)
		}
		if a.RelayConsumedCostMicroUSD != 0 || a.RelayCalls != nil || a.LogPath != "" || a.KeptFromRun != "" {
			t.Errorf("attempt carries the source's spend, log or a kept mark: %+v", a)
		}
	}
	if f.creates != 0 {
		t.Errorf("sandboxes created = %d, want none", f.creates)
	}
}

// TestApplyReplayAcceptsSeveralSourceRuns: the ticket's first build made c1
// and a pushed review round on it made c2; both are sources, and each
// original is in the range one of them judged.
func TestApplyReplayAcceptsSeveralSourceRuns(t *testing.T) {
	f := newReplayActivityFixture(t)
	f.saveSource(&run.Run{ID: "first", ResultSHA: f.c1})
	f.saveSource(&run.Run{ID: "source", BaseSHA: f.c1, DiffBaseSHA: f.c1})
	f.replay = f.replayOf("source", []string{"first", "source"}, f.upstream, f.c2, f.onto, f.r2)
	if _, err := f.apply(); err != nil {
		t.Fatalf("ApplyReplayActivity: %v", err)
	}
}

// TestApplyReplayAcceptsEmptyPairs: a commit that changes nothing has no
// patch id, and neither has its replay; the pair passes.
func TestApplyReplayAcceptsEmptyPairs(t *testing.T) {
	f := newReplayActivityFixture(t)
	f.git("checkout", "-q", "factoryd/ticket-2")
	f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "empty")
	empty := f.git("rev-parse", "HEAD")
	f.git("checkout", "-q", "buildgate/req-1/t2-restack1")
	f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "cherry-pick", "--allow-empty", "--keep-redundant-commits", empty)
	head := f.git("rev-parse", "HEAD")
	f.saveSource(&run.Run{ResultSHA: empty})
	f.replay = f.replayOf("source", []string{"source"}, f.upstream, empty, f.onto, head)
	if got := f.replay.Commits[2]; got.OriginalPatchID != "" || got.ReplayedPatchID != "" {
		t.Fatalf("fixture: the empty pair has patch ids %+v", got)
	}
	if _, err := f.apply(); err != nil {
		t.Fatalf("ApplyReplayActivity: %v", err)
	}
}

// TestApplyReplayRefuses: each check the host makes, missed, refuses the
// replay with ReplayFailureType and launches nothing; a source run that
// lacks what the check reads never passes.
func TestApplyReplayRefuses(t *testing.T) {
	for name, arrange := range replayRefusals(t) {
		t.Run(name, func(t *testing.T) {
			f := newReplayActivityFixture(t)
			want := arrange(f)
			_, err := f.apply()
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) || appErr.Type() != ReplayFailureType || !appErr.NonRetryable() {
				t.Fatalf("error = %v, want a non-retryable %s", err, ReplayFailureType)
			}
			if !strings.Contains(err.Error(), "replay run refused") || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to say the replay was refused because %q", err, want)
			}
			if f.creates != 0 {
				t.Errorf("sandboxes created = %d, want none", f.creates)
			}
		})
	}
}

// replayRefusals is each refusal TestApplyReplayRefuses checks: what to
// arrange on a passing fixture, and the words the refusal must hold.
func replayRefusals(t *testing.T) map[string]func(f *replayActivityFixture) string {
	refusals := map[string]func(f *replayActivityFixture) string{
		"a dirty worktree": func(f *replayActivityFixture) string {
			writeFile(t, filepath.Join(f.dir, "stray.txt"), "x\n")
			return "changes the replay did not make"
		},
		"HEAD is not the replayed head": func(f *replayActivityFixture) string {
			f.git("checkout", "-q", "--detach", f.r1)
			return "not at the replayed head"
		},
		"the run's record holds no replay": func(f *replayActivityFixture) string {
			in := &ReplayInput{OfRunID: "source", SourceRunIDs: []string{"source"}, OntoSHA: f.onto, HeadSHA: f.r2}
			f.inputReplay, f.replay = in, nil
			return "record carries no replay"
		},
		"the workflow's replay is not the record's": func(f *replayActivityFixture) string {
			f.inputReplay = &ReplayInput{OfRunID: "source", SourceRunIDs: []string{"source"}, OntoSHA: f.onto, HeadSHA: f.r1}
			return "not the one the run's record holds"
		},
		"the run is of another request than its replay": func(f *replayActivityFixture) string {
			f.selfRequest = "req-other"
			return "not of the request ticket its replay names"
		},
		"the replay names another request than its sources": func(f *replayActivityFixture) string {
			f.selfRequest, f.replay.RequestID = "req-other", "req-other"
			return `not of this run's request "req-other"`
		},
		"the source is missing": func(f *replayActivityFixture) string {
			f.replay.OfRunID, f.replay.SourceRunIDs = "gone", []string{"gone"}
			return "load run gone"
		},
		"the source is not accepted": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{State: run.StateQuarantined})
			return "not accepted"
		},
		"the source was not sandboxed": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}}})
			return "in the sandbox"
		},
		"the source has no build attempt": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{Attempts: []run.Attempt{{Kind: "verify", ImageDigest: "sha256:worker"}}})
			return "in the sandbox"
		},
		"the source is another repository's": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{ProjectPath: t.TempDir()})
			return "not in this repository"
		},
		"the source is another request's": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{RequestID: "req-other"})
			return `request "req-other"`
		},
		"the source is another ticket's": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{TicketIndex: 1})
			return "ticket 1"
		},
		"the source has no release decision": func(f *replayActivityFixture) string {
			src := f.saveSource(&run.Run{})
			path := filepath.Join(f.dataDir(), "projects", release.ProjectOf(src), "release-decisions", "source.json")
			if err := os.Remove(path); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			return "no release decision"
		},
		"the source was denied": func(f *replayActivityFixture) string {
			f.saveDecision(f.saveSource(&run.Run{}), release.Decision{Allowed: false, Reasons: []string{"protected path"}})
			return "denied by release policy"
		},
		"the source's decision was invalidated": func(f *replayActivityFixture) string {
			f.saveDecision(f.saveSource(&run.Run{}), release.Decision{Allowed: true, Invalidated: true})
			return "invalidated"
		},
		"the source was overridden": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{Overrides: []run.Override{{By: "kanna", Reason: "ok", PriorState: run.StateQuarantined, NewState: run.StateAccepted}}})
			return "override"
		},
		"a second source is not accepted": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{ID: "second", State: run.StateHalted})
			f.replay.SourceRunIDs = append(f.replay.SourceRunIDs, "second")
			return "second replayed from is halted"
		},
		"the replayed commits do not end at the source's result": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{ResultSHA: f.c1})
			return "not at " + shortSHA(f.c1)
		},
		"the replayed commits do not start at the source's stack base": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{StackBaseSHA: f.c1})
			return "earlier ticket's tip"
		},
		"the record leaves out an original": func(f *replayActivityFixture) string {
			f.replay.Commits = f.replay.Commits[1:]
			return "the record pairs 1 commit(s), but git has 2 original and 2 replayed"
		},
		"the record pairs the commits out of order": func(f *replayActivityFixture) string {
			f.replay.Commits[0].Original = f.c2
			return "commit pair 1"
		},
		"the record's patch id is not git's": func(f *replayActivityFixture) string {
			f.replay.Commits[0].OriginalPatchID = strings.Repeat("ab", 20)
			f.replay.Commits[0].ReplayedPatchID = strings.Repeat("ab", 20)
			return "records patch ids other than git's"
		},
		"an original no source judged": func(f *replayActivityFixture) string {
			f.saveSource(&run.Run{DiffBaseSHA: f.c1})
			return "commit " + shortSHA(f.c1) + " is in no range"
		},
	}
	maps.Copy(refusals, replayCommitRefusals(t))
	return refusals
}

// replayCommitRefusals are the refusals whose fixture rewrites git history:
// a replayed commit that changes something else, merge commits on either
// side, and the reviewer's case, an unrelated commit claiming another run as
// its source.
func replayCommitRefusals(t *testing.T) map[string]func(f *replayActivityFixture) string {
	return map[string]func(f *replayActivityFixture) string{
		"a replayed commit changes something else": func(f *replayActivityFixture) string {
			f.git("reset", "-q", "--hard", f.r1)
			head := f.commit("b.txt", "not two\n", "ticket 2: b")
			f.replay = f.replayOf("source", []string{"source"}, f.upstream, f.c2, f.onto, head)
			return "changes something else"
		},
		"a merge commit among the originals": func(f *replayActivityFixture) string {
			f.git("checkout", "-q", "-b", "side", f.upstream)
			f.commit("side.txt", "side\n", "side")
			f.git("checkout", "-q", "factoryd/ticket-2")
			f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-ff", "-m", "merge side", "side")
			merge := f.git("rev-parse", "HEAD")
			f.git("checkout", "-q", "buildgate/req-1/t2-restack1")
			f.saveSource(&run.Run{ResultSHA: merge})
			f.replay.SourceHeadSHA, f.replay.Commits[1].Original = merge, merge
			return "original commits include merge commit"
		},
		"a merge commit among the replayed": func(f *replayActivityFixture) string {
			f.git("checkout", "-q", "-b", "side", f.onto)
			f.commit("side.txt", "side\n", "side")
			f.git("checkout", "-q", "buildgate/req-1/t2-restack1")
			f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-ff", "-m", "merge side", "side")
			merge := f.git("rev-parse", "HEAD")
			f.replay.HeadSHA, f.replay.Commits[1].Replayed = merge, merge
			return "replayed commits include merge commit"
		},
		"an unrelated commit claiming another request's run as its source": func(f *replayActivityFixture) string {
			// A commit no run of this ticket made, on the earlier ticket's
			// tip, from an accepted run of another request, replayed onto
			// the new base as if it were this ticket's.
			f.git("checkout", "-q", "-b", "unrelated", f.upstream)
			unrelated := f.commit("unrelated.txt", "x\n", "unrelated")
			f.git("checkout", "-q", "-B", "buildgate/req-1/t2-restack1", f.onto)
			f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "cherry-pick", unrelated)
			head := f.git("rev-parse", "HEAD")
			f.saveSource(&run.Run{ID: "other", RequestID: "req-other", ResultSHA: unrelated})
			f.replay = f.replayOf("other", []string{"other"}, f.upstream, unrelated, f.onto, head)
			return `run other replayed from is of request "req-other"`
		},
		"an unrelated commit claiming this ticket's run as its source": func(f *replayActivityFixture) string {
			// The same commit, claimed as the result of the ticket's own run:
			// that run's result is c2, not it.
			f.git("checkout", "-q", "-b", "unrelated", f.upstream)
			unrelated := f.commit("unrelated.txt", "x\n", "unrelated")
			f.git("checkout", "-q", "-B", "buildgate/req-1/t2-restack1", f.onto)
			f.git("-c", "user.name=t", "-c", "user.email=t@example.com", "cherry-pick", unrelated)
			head := f.git("rev-parse", "HEAD")
			f.replay = f.replayOf("source", []string{"source"}, f.upstream, unrelated, f.onto, head)
			return "the result of run source"
		},
	}
}

// TestBaselineVerifyOfAReplayRunLaunchesNothing: a replay run takes no
// baseline on the new base: with no record of the source's to carry, the
// step records none and creates no sandbox.
func TestBaselineVerifyOfAReplayRunLaunchesNothing(t *testing.T) {
	f := newReplayActivityFixture(t)
	f.input.VerifyCommand = "make verify"
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunBaselineVerifyActivity)
	val, err := env.ExecuteActivity(f.acts.RunBaselineVerifyActivity, f.input)
	if err != nil {
		t.Fatalf("RunBaselineVerifyActivity: %v", err)
	}
	var res BaselineVerifyResult
	if err := val.Get(&res); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || len(res.Attempts) != 0 || res.BuildNotePath != "" {
		t.Errorf("baseline of a replay run: %d sandboxes, result %+v; want nothing launched or recorded", f.creates, res)
	}
}
