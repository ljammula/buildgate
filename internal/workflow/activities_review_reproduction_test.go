package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/codereview"
	"buildgate/internal/reproduction"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

const reproSpec = "# Spec\n\n- `Add(1, 2)` returns `3`.\n"

const reproContent = "package p\n\nimport \"testing\"\n\nfunc TestRepro(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"Add(1, 2)\")\n\t}\n}\n"

const reproFailReport = `{"Action":"run","Package":"example.com/p","Test":"TestRepro"}
{"Action":"fail","Package":"example.com/p","Test":"TestRepro","Elapsed":0}
{"Action":"fail","Package":"example.com/p","Elapsed":0.1}
`

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// reproFixture is a real Go repository the review judged, its approved spec,
// and Activities that launch through the package's fake runtime.
type reproFixture struct {
	t       *testing.T
	rt      *inspectRuntime
	acts    *Activities
	input   RunWorkflowInput
	repo    string
	commit  string
	mu      sync.Mutex
	creates []sandbox.SandboxRequest
	// mounted is what each launch mounted at the reproduction's path:
	// whether read-only, and the hash of its bytes at launch.
	mounted []string
	report  string
}

func newReproFixture(t *testing.T) *reproFixture {
	t.Helper()
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}}
	acts, input, _ := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	f := &reproFixture{t: t, rt: rt, acts: acts, repo: input.WorkspacePath, report: reproFailReport}
	writeFile(t, filepath.Join(f.repo, "go.mod"), "module example.com/p\n\ngo 1.22\n")
	writeFile(t, filepath.Join(f.repo, "p", "add.go"), "package p\n\nfunc Add(a, b int) int { return a + b }\n")
	f.commit = trustedCommit(t, f.repo)
	spec := filepath.Join(acts.LogDir, "approved-spec.snapshot.md")
	writeFile(t, spec, reproSpec)
	input.ReviewReproduction, input.ReviewSpecPath, input.ReviewSpecSHA256 = true, spec, sha256Of([]byte(reproSpec))
	input.CheckpointDir = t.TempDir()
	input.BaseSHA = f.commit
	f.input = input
	rt.onCreate = func(req sandbox.SandboxRequest) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.creates = append(f.creates, req)
		for _, m := range req.Mounts {
			switch m.Target {
			case "/workspace":
				if err := os.WriteFile(filepath.Join(m.Source, reproductionReportName), []byte(f.report), 0o644); err != nil {
					t.Errorf("write the report: %v", err)
				}
			case "/workspace/p/repro_test.go":
				body, err := os.ReadFile(m.Source)
				if err != nil {
					t.Errorf("read the mounted reproduction: %v", err)
				}
				f.mounted = append(f.mounted, map[bool]string{true: "ro ", false: "rw "}[m.ReadOnly]+sha256Of(body))
			}
		}
	}
	return f
}

// writeEvidence writes the review's evidence into the worktree.
func (f *reproFixture) writeEvidence(findings ...map[string]any) {
	f.t.Helper()
	data, err := json.Marshal(map[string]any{"schema_version": 1, "review_policy": "advisory", "available": true, "findings": findings})
	if err != nil {
		f.t.Fatal(err)
	}
	writeFile(f.t, filepath.Join(f.repo, codereview.EvidenceFile), string(data))
}

func reproFinding(path, content string, line int, text string) map[string]any {
	return map[string]any{"severity": "high", "summary": "Add is wrong", "spec_line": map[string]any{"number": line, "text": text}, "reproduction": map[string]any{"path": path, "content": content}}
}

// hold runs the review step's hold on the fixture.
func (f *reproFixture) hold() ([]run.HeldReproduction, error) {
	return f.acts.holdReproductions(context.Background(), f.input)
}

// held holds one valid reproduction and returns it.
func (f *reproFixture) held() run.HeldReproduction {
	f.t.Helper()
	f.writeEvidence(reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`."))
	held, err := f.hold()
	if err != nil || len(held) != 1 || held[0].Refused != "" {
		f.t.Fatalf("hold = %+v, %v; want one held reproduction", held, err)
	}
	return held[0]
}

// launch runs RunReviewReproductionActivity for one pass.
func (f *reproFixture) launch(h run.HeldReproduction, pass, commit string) (ReproductionRunResult, error) {
	f.t.Helper()
	in := f.input
	in.CheckpointDir = f.t.TempDir()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunReviewReproductionActivity)
	val, err := env.ExecuteActivity(f.acts.RunReviewReproductionActivity, ReproductionRunInput{RunWorkflowInput: in, Held: h, Pass: pass, Commit: commit})
	if err != nil {
		return ReproductionRunResult{}, err
	}
	var res ReproductionRunResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

func failureType(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Type()
	}
	return ""
}

// The hold copies an accepted reproduction once, read-only, into the run's
// directory and records its hash; a refused one is recorded with its rule
// and nothing is written; a finding without one is not listed. Holding again
// (a checkpoint replay that reaches it) finds the same copy.
func TestHoldReproductionsCopiesAcceptedOnesReadOnly(t *testing.T) {
	f := newReproFixture(t)
	f.writeEvidence(
		reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`."),
		reproFinding("p/add.go", reproContent, 3, "- `Add(1, 2)` returns `3`."),
		map[string]any{"severity": "low", "summary": "style"},
		reproFinding("p/other_test.go", reproContent, 3, "- Add returns something else."),
	)
	held, err := f.hold()
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if len(held) != 3 || held[0].Finding != 0 || held[1].Finding != 1 || held[2].Finding != 3 {
		t.Fatalf("held = %+v, want the first, second and fourth findings", held)
	}
	h := held[0]
	if h.Refused != "" || h.SHA256 != sha256Of([]byte(reproContent)) || h.Runner != "go-test-json" || strings.Join(h.TestIDs, ",") != "TestRepro" || h.SpecLine != 3 || !h.HoldsLiteral {
		t.Errorf("held[0] = %+v", h)
	}
	want := filepath.Join(run.ReviewReproductionDir(f.acts.LogDir), "0", "p", "repro_test.go")
	if h.HostPath != want {
		t.Errorf("host path = %s, want %s", h.HostPath, want)
	}
	info, err := os.Stat(h.HostPath)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("host copy mode = %v, %v; want 0444", info, err)
	}
	if body, _ := os.ReadFile(h.HostPath); string(body) != reproContent {
		t.Errorf("host copy = %q", body)
	}
	if held[1].Refused == "" || held[1].SHA256 != "" || held[1].HostPath != "" || held[2].Refused == "" {
		t.Errorf("refused = %+v, %+v; want each refusal recorded and nothing held", held[1], held[2])
	}
	if exists(filepath.Join(run.ReviewReproductionDir(f.acts.LogDir), "1")) {
		t.Error("a refused reproduction was written")
	}
	again, err := f.hold()
	if err != nil || len(again) != 3 || again[0].SHA256 != h.SHA256 {
		t.Errorf("hold again = %+v, %v; want the same copy", again, err)
	}
}

// The hold counts a backticked literal of the cited line only when the
// reproduction holds it and no tracked file of the base commit does
// (case-sensitive, byte-exact); a file the worktree holds untracked does not
// make it the base's, and an unknown base counts none.
func TestHoldCountsOnlyALiteralTheBaseDoesNotHold(t *testing.T) {
	holdsLiteral := func(f *reproFixture) bool {
		t.Helper()
		f.writeEvidence(reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`."))
		held, err := f.hold()
		if err != nil || len(held) != 1 || held[0].Refused != "" {
			t.Fatalf("hold = %+v, %v; want one held reproduction", held, err)
		}
		return held[0].HoldsLiteral
	}
	notInBase := newReproFixture(t)
	writeFile(t, filepath.Join(notInBase.repo, "p", "notes.txt"), "Add(1, 2) is 3\n")
	writeFile(t, filepath.Join(notInBase.repo, "p", "case.go"), "package p\n\n// add(1, 2)\n")
	if !holdsLiteral(notInBase) {
		t.Error("literals only in the reproduction and an untracked file: want the literal to count")
	}
	inBase := newReproFixture(t)
	writeFile(t, filepath.Join(inBase.repo, "p", "doc.go"), "package p\n\n// Add(1, 2) returns 3.\n")
	inBase.input.BaseSHA = trustedCommit(t, inBase.repo)
	if holdsLiteral(inBase) {
		t.Error("every literal the reproduction holds is in a tracked file of the base: want none to count")
	}
	unknown := newReproFixture(t)
	unknown.input.BaseSHA = ""
	if holdsLiteral(unknown) {
		t.Error("unknown base: want no literal to count")
	}
}

// A different file already at the host copy's path, or an approved spec
// that no longer matches the hash the run started with, is a failure to
// hold, never advice.
func TestHoldReproductionsRefusesToHoldWhatDoesNotMatch(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	if err := os.Chmod(h.HostPath, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.HostPath, "package p\n")
	if _, err := f.hold(); !errors.Is(err, errReproductionUnheld) {
		t.Errorf("hold over a different copy = %v, want unheld", err)
	}
	f = newReproFixture(t)
	writeFile(t, f.input.ReviewSpecPath, reproSpec+"- edited\n")
	f.writeEvidence(reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`."))
	if _, err := f.hold(); !errors.Is(err, errReproductionUnheld) {
		t.Errorf("hold with an edited spec = %v, want unheld", err)
	}
}

// A run without -review-reproduction holds nothing.
func TestHoldReproductionsIsOffWithoutTheRunInput(t *testing.T) {
	f := newReproFixture(t)
	f.input.ReviewReproduction = false
	f.writeEvidence(reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`."))
	if held, err := f.hold(); held != nil || err != nil {
		t.Errorf("hold = %+v, %v; want nothing", held, err)
	}
}

// SC-012: every launch checks the held copy against the hash the review
// recorded before it mounts it: a launch mounts exactly the held bytes,
// read-only, and a copy changed between two launches is refused before the
// second launch, as a failure to hold.
func TestReproductionHashCheckedBeforeEachMount(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	res, err := f.launch(h, reproductionPassHead1, f.commit)
	if err != nil {
		t.Fatalf("head1: %v", err)
	}
	if len(f.mounted) != 1 || f.mounted[0] != "ro "+h.SHA256 {
		t.Fatalf("mounted = %v, want the held bytes read-only", f.mounted)
	}
	if res.Run.ReproductionSHA256 != h.SHA256 {
		t.Errorf("run records hash %q, want %q", res.Run.ReproductionSHA256, h.SHA256)
	}
	if err := os.Chmod(h.HostPath, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.HostPath, strings.Replace(reproContent, "!= 3", "!= 4", 1))
	_, err = f.launch(h, reproductionPassHead2, f.commit)
	if failureType(err) != ReproductionUnheldFailureType {
		t.Fatalf("head2 after the copy changed = %v, want %s", err, ReproductionUnheldFailureType)
	}
	if len(f.creates) != 1 {
		t.Errorf("launches = %d, want none after the copy changed", len(f.creates))
	}
}

// The reproduction runs in a scratch worktree of the commit it judges under
// the run's directory, never in the run's own worktree, which it leaves as
// it was; the scratch is removed afterwards. The launch is a gate's: no
// route, the runner's helpers read-only at /inputs/reproduction, and the
// report classified by the runner.
func TestReproductionNeverRunsOnTheRunWorktree(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	res, err := f.launch(h, reproductionPassHead1, f.commit)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("launches = %d, want 1", len(f.creates))
	}
	req := f.creates[0]
	var workspace string
	helpers := false
	for _, m := range req.Mounts {
		switch m.Target {
		case "/workspace":
			workspace = m.Source
		case reproductionHelperDir:
			helpers = m.ReadOnly
		}
	}
	scratchRoot := filepath.Join(f.acts.LogDir, reproductionRunDirName)
	if workspace == "" || workspace == f.repo || !strings.HasPrefix(workspace, scratchRoot+string(filepath.Separator)) {
		t.Errorf("the launch's /workspace is %q, want a scratch worktree under %s", workspace, scratchRoot)
	}
	if !helpers {
		t.Error("the runner's helper directory is not mounted read-only at /inputs/reproduction")
	}
	if exists(workspace) {
		t.Errorf("the scratch worktree %s was left behind", workspace)
	}
	if exists(filepath.Join(f.repo, "p", "repro_test.go")) || exists(filepath.Join(f.repo, reproductionReportName)) {
		t.Error("the run's own worktree gained the reproduction or its report")
	}
	if res.Run.Outcome != string(reproduction.OutcomeFailed) || res.Run.Tests["TestRepro"] != string(reproduction.KindFail) || res.Run.Commit != f.commit || res.Run.ReportSHA256 != sha256Of([]byte(reproFailReport)) {
		t.Errorf("run = %+v, want a failed TestRepro on %s with the report's hash", res.Run, f.commit)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Kind != reproductionAttemptKind {
		t.Errorf("attempts = %+v, want one review_reproduction attempt", res.Attempts)
	}
}

// A recheck mounts the factory's held copy over the path in a scratch
// checkout of the result commit: what the run's worktree holds at that path
// (here an untracked edit) is never what runs.
func TestRecheckIgnoresWorktreeEditOfReproductionPath(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	writeFile(t, filepath.Join(f.repo, "p", "repro_test.go"), "package p\n\nimport \"testing\"\n\nfunc TestRepro(t *testing.T) {}\n")
	res, err := f.launch(h, run.ReproductionPassRecheck, f.commit)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if res.PathInCommit {
		t.Error("an untracked worktree file counted as the result commit holding the path")
	}
	if len(f.mounted) != 1 || f.mounted[0] != "ro "+h.SHA256 {
		t.Errorf("mounted = %v, want the held copy read-only", f.mounted)
	}
	if d := reproduction.Recheck(runResultOf(res.Run), h.TestIDs, res.PathInCommit); d.Verdict != reproduction.VerdictStillFailing {
		t.Errorf("recheck verdict = %+v, want still failing on the held copy's failure", d)
	}
}

// A result commit that holds the reproduction's path, in any case, is still
// failing with nothing launched.
func TestRecheckOfACommitHoldingThePathLaunchesNothing(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	writeFile(t, filepath.Join(f.repo, "P", "Repro_Test.go"), "package p\n")
	commit := trustedCommit(t, f.repo)
	res, err := f.launch(h, run.ReproductionPassRecheck, commit)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if !res.PathInCommit || len(f.creates) != 0 {
		t.Errorf("PathInCommit = %v, launches = %d; want true and none", res.PathInCommit, len(f.creates))
	}
	if d := reproduction.Recheck(nil, h.TestIDs, res.PathInCommit); d.Verdict != reproduction.VerdictStillFailing {
		t.Errorf("verdict = %+v, want still failing", d)
	}
}

// reviewStepWithReproduction is a review step whose launch writes evidence
// carrying one reproduction, on a run with -review-reproduction and the
// approved spec, whose recorded hash is specSHA.
func reviewStepWithReproduction(t *testing.T, specSHA string) (*reviewFixture, *[]string) {
	t.Helper()
	f := newReviewFixture(t, map[string]string{"go.mod": "module example.com/p\n\ngo 1.22\n", "p/add.go": "package p\n\nfunc Add(a, b int) int { return a + b }\n"}, func(string) {})
	// As a run's worktree does, git ignores the review's evidence file.
	writeFile(t, filepath.Join(gitIn(t, f.repo, "rev-parse", "--absolute-git-dir"), "info", "exclude"), codereview.EvidenceFile+"\n")
	spec := filepath.Join(t.TempDir(), "approved-spec.snapshot.md")
	writeFile(t, spec, reproSpec)
	f.input.ReviewReproduction, f.input.ReviewSpecPath, f.input.ReviewSpecSHA256 = true, spec, specSHA
	var args []string
	f.fakeLaunch(func(a []string) {
		args = a
		data, err := json.Marshal(map[string]any{"schema_version": 1, "review_policy": "advisory", "available": true, "findings": []any{reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`.")}})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(f.repo, codereview.EvidenceFile), string(data))
	}, nil)
	return f, &args
}

// The review step gives its launch the approved spec and holds the
// reproduction its evidence carries in the result it checkpoints.
func TestReviewStepHoldsItsReproductions(t *testing.T) {
	f, args := reviewStepWithReproduction(t, sha256Of([]byte(reproSpec)))
	res, err := f.run()
	if err != nil {
		t.Fatalf("review step: %v", err)
	}
	if !strings.Contains(strings.Join(*args, " "), "--approved-spec "+f.input.ReviewSpecPath) {
		t.Errorf("review args = %v, want --approved-spec %s", *args, f.input.ReviewSpecPath)
	}
	if len(res.Reproductions) != 1 || res.Reproductions[0].Refused != "" || res.Reproductions[0].SHA256 != sha256Of([]byte(reproContent)) || !exists(res.Reproductions[0].HostPath) {
		t.Errorf("Reproductions = %+v, want the one held copy", res.Reproductions)
	}
}

// A review whose hold fails (here the approved spec no longer matches its
// hash) is a non-retryable failure to hold.
func TestReviewStepWhoseHoldFailsIsUnheld(t *testing.T) {
	f, _ := reviewStepWithReproduction(t, strings.Repeat("0", 64))
	_, err := f.run()
	if failureType(err) != ReproductionUnheldFailureType {
		t.Errorf("review step = %v, want %s", err, ReproductionUnheldFailureType)
	}
}

// A pass on a commit where a parent of the reproduction's path is a link (here
// the base tracks p as a symlink to a directory outside the scratch) or a
// file makes no mountpoint and launches nothing: the host writes nothing
// through the link, and the pass is unstaged, which reproduction.Decide reads
// as advice ("base not checked" for the base).
func TestReproductionWithALinkedParentIsNotStaged(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, repo, outside string)
	}{
		{"symlinked parent", func(t *testing.T, repo, outside string) {
			if err := os.Symlink(outside, filepath.Join(repo, "p")); err != nil {
				t.Fatal(err)
			}
		}},
		{"file parent", func(t *testing.T, repo, _ string) {
			writeFile(t, filepath.Join(repo, "p"), "not a directory\n")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newReproFixture(t)
			h := f.held()
			outside := t.TempDir()
			if err := os.RemoveAll(filepath.Join(f.repo, "p")); err != nil {
				t.Fatal(err)
			}
			c.plant(t, f.repo, outside)
			base := trustedCommit(t, f.repo)
			for _, pass := range []string{reproductionPassBase, reproductionPassHead1} {
				res, err := f.launch(h, pass, base)
				if err != nil {
					t.Fatalf("%s: %v", pass, err)
				}
				if res.Run.Outcome != string(reproduction.OutcomeUnstaged) || len(f.creates) != 0 {
					t.Errorf("%s: outcome %q, launches %d; want unstaged and none", pass, res.Run.Outcome, len(f.creates))
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Errorf("the directory the link points to holds %v (%v), want nothing written", entries, err)
			}
			fail := &reproduction.RunResult{Outcome: reproduction.OutcomeFailed, Tests: map[string]reproduction.Kind{"TestRepro": reproduction.KindFail}}
			unstaged := &reproduction.RunResult{Outcome: reproduction.OutcomeUnstaged}
			if d := reproduction.Decide(reproduction.Evidence{Head1: fail, Head2: fail, Base: unstaged}); d.Verdict != reproduction.VerdictAdvice || d.Reason != "base not checked" {
				t.Errorf("Decide with an unstaged base = %+v, want advice: base not checked", d)
			}
		})
	}
}

// A base commit that tracks the reproduction's path as a regular file (the
// build deleted it) is mounted over, with no stub made.
func TestReproductionOverATrackedBaseFileLaunches(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	writeFile(t, filepath.Join(f.repo, "p", "repro_test.go"), "package p\n")
	base := trustedCommit(t, f.repo)
	res, err := f.launch(h, reproductionPassBase, base)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	if len(f.creates) != 1 || len(f.mounted) != 1 || f.mounted[0] != "ro "+h.SHA256 || res.Run.Outcome != string(reproduction.OutcomeFailed) {
		t.Errorf("launches %d, mounted %v, outcome %q; want one launch of the held copy", len(f.creates), f.mounted, res.Run.Outcome)
	}
}

// Evidence already in the worktree when the review starts (the build wrote
// it) is removed before the launch, so a launch that writes none holds
// nothing from it.
func TestReviewStepHoldsNoEvidenceItDidNotWrite(t *testing.T) {
	f, _ := reviewStepWithReproduction(t, sha256Of([]byte(reproSpec)))
	planted := filepath.Join(f.repo, codereview.EvidenceFile)
	data, err := json.Marshal(map[string]any{"schema_version": 1, "review_policy": "advisory", "available": true, "findings": []any{reproFinding("p/repro_test.go", reproContent, 3, "- `Add(1, 2)` returns `3`.")}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, planted, string(data))
	var foundAtLaunch bool
	f.fakeLaunch(func([]string) { foundAtLaunch = exists(planted) }, nil)
	res, err := f.run()
	if err != nil {
		t.Fatalf("review step: %v", err)
	}
	if foundAtLaunch || len(res.Reproductions) != 0 {
		t.Errorf("evidence present at launch %v, held %+v; want it removed and nothing held", foundAtLaunch, res.Reproductions)
	}
}

// A review that gave no verdict (CombinedExitBase+2) holds nothing, even
// when its launch wrote evidence carrying a reproduction.
func TestReviewWithNoVerdictHoldsNothing(t *testing.T) {
	f, _ := reviewStepWithReproduction(t, sha256Of([]byte(reproSpec)))
	f.exitOffset = 2
	res, err := f.run()
	if err != nil {
		t.Fatalf("review step: %v", err)
	}
	if len(res.Reproductions) != 0 || reviewStepPassed(res.Result.ExitCode) {
		t.Errorf("exit %d, held %+v; want no verdict and nothing held", res.Result.ExitCode, res.Reproductions)
	}
}

// A launch whose sandbox started the reproduction's command and whose wait
// then ended on the launch's deadline is a pass with every test a timeout
// (cap_exceeded), fed to the decision as any other timeout. A sandbox that never started its command, or a wait lost for any
// other reason, stays the launch's own failure and gives no verdict.
func TestReproductionThatRunsPastItsCapIsATimeout(t *testing.T) {
	f := newReproFixture(t)
	h := f.held()
	f.rt.WaitErr = context.DeadlineExceeded
	findings := liveReproduce(t, f.acts, f.input, []run.HeldReproduction{h}, f.commit)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v", findings)
	}
	rep := findings[0].Reproduction
	if len(rep.Runs) != 3 {
		t.Fatalf("runs = %+v; want head1, head2 and base", rep.Runs)
	}
	for _, r := range rep.Runs {
		if !r.CapExceeded || r.Outcome != string(reproduction.OutcomeFailed) || r.Tests["TestRepro"] != string(reproduction.KindTimeout) || r.ReportSHA256 != "" {
			t.Errorf("run %s = %+v; want every test a timeout, cap exceeded, no report read", r.Pass, r)
		}
	}
	// The base hangs the same way; the cited line's literals are in the
	// reproduction and not in the base, so it blocks on that.
	if rep.Verdict != string(reproduction.VerdictBlocks) || !strings.Contains(rep.Reason, "fails the same way on the base and holds a literal") {
		t.Errorf("verdict = %s (%s); want blocks on the same timeout and the literal", rep.Verdict, rep.Reason)
	}

	for name, rt := range map[string]*sandboxtest.WorkerRuntime{
		"never started": {CreateErr: context.DeadlineExceeded},
		"wait lost":     {WaitErr: errors.New("the gateway closed the stream")},
	} {
		g := newReproFixture(t)
		hg := g.held()
		g.rt.WorkerRuntime = rt
		if _, err := g.launch(hg, reproductionPassHead1, g.commit); err == nil || failureType(err) == ReproductionUnheldFailureType {
			t.Errorf("%s: err = %v (%s); want the launch's own failure", name, err, failureType(err))
		} else if name == "never started" && failureType(err) != SandboxLaunchFailureType {
			t.Errorf("%s: type %s, want %s", name, failureType(err), SandboxLaunchFailureType)
		}
	}
}

// ranPastLaunchCap reads only the launch's own cap: the Activity's deadline
// ending first, or a context that ended, is not a hang the factory timed.
func TestRanPastLaunchCapReadsOnlyTheLaunchsDeadline(t *testing.T) {
	act, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	launch, cancelLaunch := context.WithTimeout(act, time.Minute)
	defer cancelLaunch()
	started := fmt.Errorf("sandbox x: %w", context.DeadlineExceeded)
	notStarted := fmt.Errorf("%w: %w", sandbox.ErrSandboxNotStarted, context.DeadlineExceeded)
	if !ranPastLaunchCap(act, launch, started) {
		t.Error("a started command cut by the launch's cap: want true")
	}
	if ranPastLaunchCap(act, launch, notStarted) || ranPastLaunchCap(act, launch, errors.New("lost")) {
		t.Error("a sandbox that never started, or another error: want false")
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	inShort, cancelIn := context.WithTimeout(short, time.Minute)
	defer cancelIn()
	if ranPastLaunchCap(short, inShort, started) {
		t.Error("the Activity's deadline before the cap: want false")
	}
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if ranPastLaunchCap(done, launch, started) {
		t.Error("the Activity's context ended: want false")
	}
}
