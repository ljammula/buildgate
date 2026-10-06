package workflow

import (
	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// TestCollectEvidenceActivityHaltsOnAmbiguousPriorIntent is
// TestPostBuildActivityHaltsOnAmbiguousPriorIntent's counterpart for this
// Activity's own safety-net commit of verification's dirt.
func TestCollectEvidenceActivityHaltsOnAmbiguousPriorIntent(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatalf("simulate verification dirt: %v", err)
	}

	dir := t.TempDir()
	activities := &Activities{LogDir: dir}
	input := CollectEvidenceInput{WorkspacePath: workspacePath, BaseSHA: baseSHA}
	wrapper := func(ctx context.Context, input CollectEvidenceInput) (CollectedEvidence, error) {
		info := activity.GetInfo(ctx)
		if _, err := recordActivityIntentForExecution(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "collect-evidence-commit", []string{"stale"}, "2024-01-01T00:00:00Z"); err != nil {
			return CollectedEvidence{}, err
		}
		return activities.CollectEvidenceActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err = env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute collect-evidence Activity with pre-existing intent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "prior attempt") {
		t.Fatalf("error = %q, want it to mention the prior attempt", err.Error())
	}
	clean, err := runner.GitIsClean(workspacePath)
	if err != nil {
		t.Fatalf("check workspace cleanliness: %v", err)
	}
	if clean {
		t.Error("workspace is clean after the halt; the stale intent must have prevented a second commit attempt entirely")
	}
}

// callCollectEvidenceActivity runs CollectEvidenceActivity through a real
// Activity context (testsuite.TestActivityEnvironment) rather than calling
// it directly with context.Background() — required since it now loads/
// saves a durable checkpoint (see CollectEvidenceActivity's doc comment),
// which needs activity.GetInfo(ctx) to identify the execution.
func callCollectEvidenceActivity(t *testing.T, activities *Activities, input CollectEvidenceInput) (CollectedEvidence, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.CollectEvidenceActivity)
	raw, err := env.ExecuteActivity(activities.CollectEvidenceActivity, input)
	if err != nil {
		return CollectedEvidence{}, err
	}
	var result CollectedEvidence
	if decodeErr := raw.Get(&result); decodeErr != nil {
		t.Fatalf("decode CollectEvidenceActivity result: %v", decodeErr)
	}
	return result, nil
}

// TestCollectEvidenceActivityRecordsSemanticPackageLockChanges proves the
// Temporal collector carries package-lock dependency changes into its result
// using the run's base commit and final repository state.
func TestCollectEvidenceActivityRecordsSemanticPackageLockChanges(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	lockPath := filepath.Join(workspacePath, "package-lock.json")
	baseLock := `{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.0.0"}}}`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base package-lock.json: %v", err)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "add", "package-lock.json").CombinedOutput(); err != nil {
		t.Fatalf("git add package-lock.json: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "commit", "-q", "-m", "add package lock").CombinedOutput(); err != nil {
		t.Fatalf("commit base package-lock.json: %v: %s", err, out)
	}
	baseSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.1.0"},"node_modules/b":{"name":"b","version":"2.0.0"}}}`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result package-lock.json: %v", err)
	}

	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir()}, CollectEvidenceInput{
		WorkspacePath: workspacePath,
		BaseSHA:       baseSHA,
		BuildExitCode: 0,
		// A failed verification must not trigger the collector's
		// safety-net commit; the semantic diff still needs to include
		// the final, uncommitted workspace content.
		VerifyExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "a", base: "1.0.0", result: "1.1.0"},
		{name: "b", result: "2.0.0"},
	}
	if len(result.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", result.DependencyChanges, want)
	}
	for i, want := range want {
		got := result.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestCollectEvidenceActivityRecordsSemanticComposerLockChanges proves the
// Temporal collector carries composer.lock dependency changes into its result
// using the run's base commit and final repository state.
func TestCollectEvidenceActivityRecordsSemanticComposerLockChanges(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	lockPath := filepath.Join(workspacePath, "composer.lock")
	baseLock := `{"packages":[{"name":"vendor/a","version":"1.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base composer.lock: %v", err)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "add", "composer.lock").CombinedOutput(); err != nil {
		t.Fatalf("git add composer.lock: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "commit", "-q", "-m", "add composer lock").CombinedOutput(); err != nil {
		t.Fatalf("commit base composer.lock: %v: %s", err, out)
	}
	baseSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `{"packages":[{"name":"vendor/a","version":"1.1.0"},{"name":"vendor/b","version":"3.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result composer.lock: %v", err)
	}

	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir()}, CollectEvidenceInput{
		WorkspacePath:  workspacePath,
		BaseSHA:        baseSHA,
		BuildExitCode:  0,
		VerifyExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "vendor/a", base: "1.0.0", result: "1.1.0"},
		{name: "vendor/b", result: "3.0.0"},
	}
	if len(result.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", result.DependencyChanges, want)
	}
	for i, want := range want {
		got := result.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestCollectEvidenceActivityRecordsSemanticPubspecLockChanges proves the
// Temporal collector carries pubspec.lock dependency changes into its
// result using the run's base commit and final repository state.
func TestCollectEvidenceActivityRecordsSemanticPubspecLockChanges(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	lockPath := filepath.Join(workspacePath, "pubspec.lock")
	baseLock := `packages:
  a:
    version: "1.0.0"`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base pubspec.lock: %v", err)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "add", "pubspec.lock").CombinedOutput(); err != nil {
		t.Fatalf("git add pubspec.lock: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "commit", "-q", "-m", "add pubspec lock").CombinedOutput(); err != nil {
		t.Fatalf("commit base pubspec.lock: %v: %s", err, out)
	}
	baseSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `packages:
  a:
    version: "1.1.0"
  b:
    version: "2.0.0"`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result pubspec.lock: %v", err)
	}

	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir()}, CollectEvidenceInput{
		WorkspacePath: workspacePath,
		BaseSHA:       baseSHA,
		BuildExitCode: 0,
		// A failed verification must not trigger the collector's
		// safety-net commit; the semantic diff still needs to include
		// the final, uncommitted workspace content.
		VerifyExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "a", base: "1.0.0", result: "1.1.0"},
		{name: "b", result: "2.0.0"},
	}
	if len(result.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", result.DependencyChanges, want)
	}
	for i, want := range want {
		got := result.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestCollectEvidenceActivityRecordsSemanticGoSumChanges proves the Temporal
// collector carries go.sum dependency changes into its result using the
// run's base commit and final repository state.
func TestCollectEvidenceActivityRecordsSemanticGoSumChanges(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	lockPath := filepath.Join(workspacePath, "go.sum")
	baseLock := `github.com/a/a v1.0.0 h1:abc=
github.com/a/a v1.0.0/go.mod h1:def=`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base go.sum: %v", err)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "add", "go.sum").CombinedOutput(); err != nil {
		t.Fatalf("git add go.sum: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "commit", "-q", "-m", "add go.sum").CombinedOutput(); err != nil {
		t.Fatalf("commit base go.sum: %v: %s", err, out)
	}
	baseSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `github.com/a/a v1.1.0 h1:mno=
github.com/a/a v1.1.0/go.mod h1:pqr=
github.com/b/b v2.0.0 h1:stu=
github.com/b/b v2.0.0/go.mod h1:vwx=`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result go.sum: %v", err)
	}

	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir()}, CollectEvidenceInput{
		WorkspacePath: workspacePath,
		BaseSHA:       baseSHA,
		BuildExitCode: 0,
		// A failed verification must not trigger the collector's
		// safety-net commit; the semantic diff still needs to include
		// the final, uncommitted workspace content.
		VerifyExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "github.com/a/a", base: "v1.0.0", result: "v1.1.0"},
		{name: "github.com/b/b", result: "v2.0.0"},
	}
	if len(result.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", result.DependencyChanges, want)
	}
	for i, want := range want {
		got := result.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestCollectEvidenceActivityRecordsAdditionalSemanticLockChanges proves the
// Temporal evidence path wires all supported lockfile readers to the final
// worktree, including additions/updates that remain uncommitted.
func TestCollectEvidenceActivityRecordsAdditionalSemanticLockChanges(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	files := map[string][2]string{
		"yarn.lock": {`"foo@^1.0.0":
  version "1.0.0"
`, `"foo@^1.0.0":
  version "1.1.0"
`},
		"pnpm-lock.yaml": {`lockfileVersion: '9.0'
packages:
  "@scope/foo@1.0.0_peer@2.0.0": {}
`, `lockfileVersion: '9.0'
packages:
  "@scope/foo@1.1.0_peer@2.0.0": {}
`},
		"Gemfile.lock": {`GEM
  remote: https://rubygems.org/
  specs:
    rack (3.0.0)
`, `GEM
  remote: https://rubygems.org/
  specs:
    rack (3.1.0)
`},
		"poetry.lock": {`[[package]]
name = "foo"
version = "1.0.0"
`, `[[package]]
name = "foo"
version = "1.1.0"
`},
		"Cargo.lock": {`version = 3
[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://example"
`, `version = 3
[[package]]
name = "foo"
version = "1.1.0"
source = "registry+https://example"
`},
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(workspacePath, name), []byte(contents[0]), 0o644); err != nil {
			t.Fatalf("write base %s: %v", name, err)
		}
	}
	if out, err := exec.Command("git", "-C", workspacePath, "add", "--", "yarn.lock", "pnpm-lock.yaml", "Gemfile.lock", "poetry.lock", "Cargo.lock").CombinedOutput(); err != nil {
		t.Fatalf("git add lockfiles: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspacePath, "commit", "-q", "-m", "add additional lockfiles").CombinedOutput(); err != nil {
		t.Fatalf("commit base lockfiles: %v: %s", err, out)
	}
	baseSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(workspacePath, name), []byte(contents[1]), 0o644); err != nil {
			t.Fatalf("write result %s: %v", name, err)
		}
	}

	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir()}, CollectEvidenceInput{
		WorkspacePath:  workspacePath,
		BaseSHA:        baseSHA,
		BuildExitCode:  0,
		VerifyExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	if len(result.DependencyChanges) != len(files)+3 {
		t.Fatalf("DependencyChanges = %+v, want lossless changes for additional lockfiles", result.DependencyChanges)
	}
	for _, want := range []struct {
		name, base, result string
	}{
		{"foo", "1.0.0", "1.1.0"},
		{"@scope/foo", "", "1.1.0_peer@2.0.0"},
		{"@scope/foo", "1.0.0_peer@2.0.0", ""},
		{"rack", "3.0.0", "3.1.0"},
		{"foo", "", "1.1.0"},
		{"foo", "1.0.0", ""},
		{"foo", "", "1.1.0"},
		{"foo", "1.0.0", ""},
	} {
		found := false
		for _, got := range result.DependencyChanges {
			if got.Name == want.name && got.Base == want.base && got.Result == want.result {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DependencyChanges = %+v, missing %s %s -> %s", result.DependencyChanges, want.name, want.base, want.result)
		}
	}
	for i := 1; i < len(result.DependencyChanges); i++ {
		previous, current := result.DependencyChanges[i-1], result.DependencyChanges[i]
		if previous.Name > current.Name || (previous.Name == current.Name && previous.Base > current.Base) || (previous.Name == current.Name && previous.Base == current.Base && previous.Result > current.Result) {
			t.Fatalf("DependencyChanges are not globally sorted at %d: %+v then %+v", i, previous, current)
		}
	}
}

// TestCollectEvidenceActivityCommitsDirtyVerificationOutput is real-git
// proof of a bug found live in review: canonical verification can itself
// leave the workspace dirty (a formatter, codegen) without committing its
// own output. CollectEvidenceActivity used to capture ResultSHA before
// checking for that dirt, then fold the dirt into ChangedFiles/DiffStat
// anyway — recording evidence for an accepted run that its own result_sha
// commit couldn't reproduce or be merged from. It must now commit that
// dirt itself, the same safety-net guarantee PostBuildActivity already
// makes for build-time dirt, before ResultSHA is ever captured.
func TestCollectEvidenceActivityCommitsDirtyVerificationOutput(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := string(baseSHABytes[:len(baseSHABytes)-1])

	// Simulates canonical verification (e.g. a formatter) rewriting a
	// tracked file without committing it — left uncommitted, exactly as
	// a real "make verify" or "flutter test" run's own side effects
	// would leave it.
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatalf("simulate verification dirt: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	result, err := callCollectEvidenceActivity(t, activities, CollectEvidenceInput{
		WorkspacePath: workspacePath,
		BaseSHA:       baseSHA,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}

	if !result.Committed {
		t.Error("Committed = false, want true: verification's dirt should have been committed as a safety net")
	}
	clean, err := runner.GitIsClean(workspacePath)
	if err != nil {
		t.Fatalf("check workspace cleanliness: %v", err)
	}
	if !clean {
		t.Error("workspace is still dirty after CollectEvidenceActivity; verification's output was never committed")
	}
	headSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD after CollectEvidenceActivity: %v", err)
	}
	headSHA := string(headSHABytes[:len(headSHABytes)-1])
	if result.ResultSHA != headSHA {
		t.Fatalf("ResultSHA = %q, want the post-commit HEAD %q", result.ResultSHA, headSHA)
	}
	// The whole point: content.txt's new content must actually be
	// reachable from ResultSHA, not just sitting uncommitted in the
	// worktree while ResultSHA points at an earlier commit.
	committedContent, err := exec.Command("git", "-C", workspacePath, "show", result.ResultSHA+":content.txt").Output()
	if err != nil {
		t.Fatalf("git show %s:content.txt: %v", result.ResultSHA, err)
	}
	if string(committedContent) != "formatted\n" {
		t.Fatalf("content.txt as of ResultSHA = %q, want %q", committedContent, "formatted\n")
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "content.txt" {
		t.Fatalf("ChangedFiles = %v, want [content.txt]", result.ChangedFiles)
	}
}

// TestCollectEvidenceActivityPreservesCommittedOnLaterFailure is the
// regression test for a real P1 finding from review: this Activity's own
// verification-output commit above can land, and then a later step in the
// very same invocation (here, parsing a malformed package-lock.json for
// semantic dependency evidence) can still fail — Temporal's Get on a
// failed Activity never populates its return value, only its error, so
// RunWorkflow's own result.CommittedByWorker (updated only on success) had
// no way to learn the commit happened. A malformed package-lock.json is
// both the uncommitted dirt that triggers the safety-net commit and the
// cause of the later dependency-diff failure.
func TestCollectEvidenceActivityPreservesCommittedOnLaterFailure(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := string(baseSHABytes[:len(baseSHABytes)-1])

	if err := os.WriteFile(filepath.Join(workspacePath, "package-lock.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("simulate malformed verification dirt: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	_, err = callCollectEvidenceActivity(t, activities, CollectEvidenceInput{
		WorkspacePath: workspacePath,
		BaseSHA:       baseSHA,
	})
	if err == nil {
		t.Fatal("CollectEvidenceActivity: want an error from the malformed package-lock.json, got nil")
	}
	if !ActivityCommittedFromError(err) {
		t.Error("ActivityCommittedFromError(err) = false, want true: the safety-net commit landed before the later dependency-diff step failed")
	}
	clean, cleanErr := runner.GitIsClean(workspacePath)
	if cleanErr != nil {
		t.Fatalf("check workspace cleanliness: %v", cleanErr)
	}
	if !clean {
		t.Error("workspace is still dirty; the safety-net commit should have landed despite the later failure")
	}
}

// TestCollectEvidenceActivityDoesNotCommitDirtWhenVerificationFailed is
// real-git proof of a bug found live in a second review round: the commit
// above used to run unconditionally, even when build or verification had
// already failed and this run will quarantine regardless — advancing HEAD
// with a quarantined run's dirt anyway, which a later accepted run would
// then silently inherit as part of its own base_sha. The commit must only
// happen when both BuildExitCode and VerifyExitCode are 0.
func TestCollectEvidenceActivityDoesNotCommitDirtWhenVerificationFailed(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := string(baseSHABytes[:len(baseSHABytes)-1])

	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatalf("simulate verification dirt: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	result, err := callCollectEvidenceActivity(t, activities, CollectEvidenceInput{
		WorkspacePath:  workspacePath,
		BaseSHA:        baseSHA,
		VerifyExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}

	if result.Committed {
		t.Error("Committed = true, want false: verification failed, so its dirt must not have been committed as a safety net")
	}
	clean, err := runner.GitIsClean(workspacePath)
	if err != nil {
		t.Fatalf("check workspace cleanliness: %v", err)
	}
	if clean {
		t.Error("workspace is clean after CollectEvidenceActivity; expected the failed verification's dirt to remain uncommitted")
	}
	if result.ResultSHA != baseSHA {
		t.Fatalf("ResultSHA = %q, want it to remain baseSHA %q (no commit should have happened)", result.ResultSHA, baseSHA)
	}
}

// TestCollectEvidenceActivityDoesNotCommitDirtWhenFullSuiteFailed is
// TestCollectEvidenceActivityDoesNotCommitDirtWhenVerificationFailed's
// counterpart for FullSuiteRan/FullSuiteExitCode (gap 3 of the plan's
// 2026-08-28 readiness review, the regression oracle) — a real P1 finding
// from both a GitHub Codex App review round and a local `codex review`
// pass on PR #33: build and verify can both be 0 while a full-suite
// command that mutated the checkout then failed, and without this gate
// that mutation was committed and HEAD advanced anyway, silently
// poisoning a later chained run's base_sha with a quarantined run's
// rejected output (gap 4).
func TestCollectEvidenceActivityDoesNotCommitDirtWhenFullSuiteFailed(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := string(baseSHABytes[:len(baseSHABytes)-1])

	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("full-suite output\n"), 0o644); err != nil {
		t.Fatalf("simulate full-suite dirt: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	result, err := callCollectEvidenceActivity(t, activities, CollectEvidenceInput{
		WorkspacePath:     workspacePath,
		BaseSHA:           baseSHA,
		BuildExitCode:     0,
		VerifyExitCode:    0,
		FullSuiteRan:      true,
		FullSuiteExitCode: 1,
	})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}

	if result.Committed {
		t.Error("Committed = true, want false: full-suite failed, so its dirt must not have been committed as a safety net")
	}
	clean, err := runner.GitIsClean(workspacePath)
	if err != nil {
		t.Fatalf("check workspace cleanliness: %v", err)
	}
	if clean {
		t.Error("workspace is clean after CollectEvidenceActivity; expected the failed full-suite's dirt to remain uncommitted")
	}
	if result.ResultSHA != baseSHA {
		t.Fatalf("ResultSHA = %q, want it to remain baseSHA %q (no commit should have happened)", result.ResultSHA, baseSHA)
	}
}

// TestCollectEvidenceActivityDuplicateInvocationCommitsOnce is
// TestPostBuildActivityDuplicateInvocationCommitsOnce's counterpart for
// this Activity's own safety-net commit (verification's own dirt, rather
// than the build's).
func TestCollectEvidenceActivityDuplicateInvocationCommitsOnce(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatalf("simulate verification dirt: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	input := CollectEvidenceInput{WorkspacePath: workspacePath, BaseSHA: baseSHA}
	wrapper := func(ctx context.Context, input CollectEvidenceInput) ([2]CollectedEvidence, error) {
		first, err := activities.CollectEvidenceActivity(ctx, input)
		if err != nil {
			return [2]CollectedEvidence{}, err
		}
		second, err := activities.CollectEvidenceActivity(ctx, input)
		if err != nil {
			return [2]CollectedEvidence{}, err
		}
		return [2]CollectedEvidence{first, second}, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute duplicate collect-evidence Activity: %v", err)
	}
	var results [2]CollectedEvidence
	if err := raw.Get(&results); err != nil {
		t.Fatalf("decode collect-evidence Activity results: %v", err)
	}
	if !results[0].Committed || !results[1].Committed {
		t.Fatalf("results = %+v, want Committed=true on both calls", results)
	}
	if results[0].ResultSHA != results[1].ResultSHA {
		t.Fatalf("ResultSHA changed between duplicate invocations: %q -> %q, want the same (cached) value", results[0].ResultSHA, results[1].ResultSHA)
	}
	headBytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD after duplicate invocation: %v", err)
	}
	if head := strings.TrimSpace(string(headBytes)); head != results[0].ResultSHA {
		t.Fatalf("workspace HEAD = %q, want it still at %q — the second call must not have committed again", head, results[0].ResultSHA)
	}
}
