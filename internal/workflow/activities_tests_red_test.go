package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
)

const (
	redOldTest = "package p\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"Add\")\n\t}\n}\n"
	redNewTest = redOldTest + "\nfunc TestAddNegative(t *testing.T) {\n\tif Add(-1, -2) != -3 {\n\t\tt.Fatal(\"Add negative\")\n\t}\n}\n"
)

// redFixture is a repository whose base commit already tracks p/add_test.go
// and whose result commit changes it and adds a second test file, with the
// result commit's SHA.
func redFixture(t *testing.T) (*reproFixture, string) {
	t.Helper()
	f := newReproFixture(t)
	writeFile(t, filepath.Join(f.repo, "p", "add_test.go"), redOldTest)
	f.commit = trustedCommit(t, f.repo)
	f.input.BaseSHA = f.commit
	writeFile(t, filepath.Join(f.repo, "p", "add_test.go"), redNewTest)
	writeFile(t, filepath.Join(f.repo, "p", "helpers_test.go"), "package p\n\nfunc helper() {}\n")
	return f, trustedCommit(t, f.repo)
}

func (f *reproFixture) holdChanged(commit string, files ...string) (TestsRedHoldResult, error) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.HoldChangedTestsActivity)
	val, err := env.ExecuteActivity(f.acts.HoldChangedTestsActivity, TestsRedHoldInput{RunWorkflowInput: f.input, Commit: commit, Base: f.commit, Files: files})
	var res TestsRedHoldResult
	if err != nil {
		return res, err
	}
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

// The hold copies each changed test file a runner takes, as the result commit
// has it, once and read-only, into the run's own directory and records its
// hash; a file with no test function (a helper) is left out and nothing is
// written for it.
func TestHoldChangedTestsCopiesTheResultCommitsFileReadOnly(t *testing.T) {
	f, result := redFixture(t)
	res, err := f.holdChanged(result, "p/add_test.go", "p/helpers_test.go")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if len(res.Held) != 1 || res.Reason != "" {
		t.Fatalf("held = %+v, reason %q; want only p/add_test.go", res.Held, res.Reason)
	}
	h := res.Held[0]
	want := filepath.Join(run.TestsRedOnBaseDir(f.acts.LogDir), "0", "p", "add_test.go")
	if h.Path != "p/add_test.go" || h.SHA256 != sha256Of([]byte(redNewTest)) || h.HostPath != want || h.Runner != "go-test-json" || strings.Join(h.TestIDs, ",") != "TestAdd,TestAddNegative" {
		t.Errorf("held = %+v, want the result commit's file at %s", h, want)
	}
	if info, err := os.Stat(h.HostPath); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("host copy mode = %v, %v; want 0444", info, err)
	}
	if body, _ := os.ReadFile(h.HostPath); string(body) != redNewTest {
		t.Errorf("host copy = %q, want the result commit's content, not the base's", body)
	}
	if exists(filepath.Join(run.TestsRedOnBaseDir(f.acts.LogDir), "1")) {
		t.Error("a helper file with no test function was written")
	}
	again, err := f.holdChanged(result, "p/add_test.go")
	if err != nil || len(again.Held) != 1 || again.Held[0].SHA256 != h.SHA256 {
		t.Errorf("hold again = %+v, %v; want the same copy", again, err)
	}
}

// No file a runner takes is no hold, with a reason, and an error from the
// hold never reaches a verdict: nothing is written.
func TestHoldChangedTestsWithNoRunnableFileSaysWhy(t *testing.T) {
	f, result := redFixture(t)
	res, err := f.holdChanged(result, "p/helpers_test.go", "README_test.md", "p/absent_test.go")
	if err != nil || len(res.Held) != 0 || res.Reason == "" {
		t.Errorf("hold = %+v, %v; want nothing held and a reason", res, err)
	}
}

// A modified test file that the base commit already tracks is staged over the
// base's own copy, read-only, with the held bytes: the base launch runs the
// result's test file against the base's code. Both launches carry the proof's
// attempt kind.
func TestChangedTestFileIsMountedOverTheBasesOwnCopyOnBothCommits(t *testing.T) {
	f, result := redFixture(t)
	res, err := f.holdChanged(result, "p/add_test.go")
	if err != nil || len(res.Held) != 1 {
		t.Fatalf("hold = %+v, %v", res, err)
	}
	h := res.Held[0]
	var mounted []string
	f.rt.onCreate = func(req sandbox.SandboxRequest) {
		for _, m := range req.Mounts {
			switch m.Target {
			case "/workspace":
				if err := os.WriteFile(filepath.Join(m.Source, reproductionReportName), []byte(`{"Action":"pass","Test":"TestAdd"}`+"\n"), 0o644); err != nil {
					t.Errorf("write the report: %v", err)
				}
			case "/workspace/p/add_test.go":
				body, _ := os.ReadFile(m.Source)
				mounted = append(mounted, map[bool]string{true: "ro ", false: "rw "}[m.ReadOnly]+sha256Of(body))
			}
		}
	}
	for pass, commit := range map[string]string{run.TestsRedPassResult: result, run.TestsRedPassBase: f.commit} {
		in := f.input
		in.CheckpointDir = t.TempDir()
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		env.RegisterActivity(f.acts.RunReviewReproductionActivity)
		val, err := env.ExecuteActivity(f.acts.RunReviewReproductionActivity, ReproductionRunInput{RunWorkflowInput: in, Held: h, Pass: pass, Commit: commit, AttemptKind: testsRedAttemptKind})
		if err != nil {
			t.Fatalf("%s launch: %v", pass, err)
		}
		var got ReproductionRunResult
		if err := val.Get(&got); err != nil {
			t.Fatal(err)
		}
		if got.Run.Pass != pass || got.Run.Commit != commit || got.Run.Tests["TestAdd"] != string(reproduction.KindPass) {
			t.Errorf("%s run = %+v, want TestAdd passing on %s", pass, got.Run, commit)
		}
		if len(got.Attempts) != 1 || got.Attempts[0].Kind != testsRedAttemptKind {
			t.Errorf("%s attempts = %+v, want one tests_red_on_base attempt", pass, got.Attempts)
		}
	}
	want := "ro " + h.SHA256
	if len(mounted) != 2 || mounted[0] != want || mounted[1] != want {
		t.Errorf("mounted = %v, want the held copy read-only on both launches", mounted)
	}
	if body, _ := os.ReadFile(filepath.Join(f.repo, "p", "add_test.go")); string(body) != redNewTest {
		t.Error("the run's own worktree was changed by the proof")
	}
}

// With more changed test files than the cap, the files the change adds are
// held before the ones it only modifies, whatever their names sort to.
func TestHoldChangedTestsHoldsAddedFilesBeforeModifiedOnes(t *testing.T) {
	f := newReproFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(f.repo, "p", n+"_test.go"), strings.Replace(redOldTest, "TestAdd", "Test"+strings.ToUpper(n), 1))
	}
	f.commit = trustedCommit(t, f.repo)
	for _, n := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(f.repo, "p", n+"_test.go"), strings.Replace(redNewTest, "TestAdd", "Test"+strings.ToUpper(n), 1))
	}
	writeFile(t, filepath.Join(f.repo, "p", "z_test.go"), strings.Replace(redOldTest, "TestAdd", "TestZ", 1))
	result := trustedCommit(t, f.repo)
	res, err := f.holdChanged(result, "p/a_test.go", "p/b_test.go", "p/c_test.go", "p/z_test.go")
	if err != nil || len(res.Held) != reproduction.MaxReproductions {
		t.Fatalf("hold = %+v, %v; want %d files", res, err, reproduction.MaxReproductions)
	}
	var paths []string
	for _, h := range res.Held {
		paths = append(paths, h.Path)
	}
	if want := "p/z_test.go,p/a_test.go,p/b_test.go"; strings.Join(paths, ",") != want {
		t.Errorf("held = %v, want %s: the added file first, then the modified ones sorted", paths, want)
	}
}
