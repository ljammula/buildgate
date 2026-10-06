package workflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"buildgate/internal/progress"
	"testing"

	"buildgate/internal/sandbox"
)

func coGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func coFixture(t *testing.T, manifest string) (a *Activities, input CommitOraclesInput, base string) {
	t.Helper()
	ws := t.TempDir()
	coGit(t, ws, "init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(ws, "pkg"), 0o755)
	os.WriteFile(filepath.Join(ws, "pkg", "x.go"), []byte("package pkg\n"), 0o644)
	coGit(t, ws, "add", "-A")
	coGit(t, ws, "commit", "-q", "-m", "base")
	base = coGit(t, ws, "rev-parse", "HEAD")

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "MANIFEST.json"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(src, "o_test.go"), []byte("package pkg\n"), 0o644)
	hash, err := sandbox.SnapshotReferenceOracle(t.TempDir(), src, filepath.Join(t.TempDir(), "probe"))
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	a = &Activities{LogDir: logDir}
	input = CommitOraclesInput{
		RunWorkflowInput:   RunWorkflowInput{Ticket: "t1", RunID: "run-1", WorkspacePath: ws, BaseSHA: base, ReferenceOracleDir: src, LogDir: logDir},
		PinnedOracleSHA256: hash,
	}
	return a, input, base
}

func TestCommitOraclesActivityInertWithoutTargetPath(t *testing.T) {
	a, in, base := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go"}]`)
	out, err := a.CommitOraclesActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Active || out.Committed || out.ResultSHA != "" || out.Oracles != nil {
		t.Fatalf("inert manifest produced output: %+v", out)
	}
	if coGit(t, in.WorkspacePath, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved")
	}
}

func TestCommitOraclesActivityCommitsAndReturnsFreshEvidence(t *testing.T) {
	a, in, base := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	out, err := a.CommitOraclesActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	head := coGit(t, in.WorkspacePath, "rev-parse", "HEAD")
	if !out.Active || !out.Committed || out.ResultSHA != head || head == base {
		t.Fatalf("out=%+v head=%s base=%s", out, head, base)
	}
	if strings.Join(out.ChangedFiles, ",") != ".buildgate/oracles.json,pkg/x_oracle_test.go" {
		t.Fatalf("ChangedFiles = %v (must describe the NEW result commit)", out.ChangedFiles)
	}
	if out.Oracles == nil || !out.Oracles.FactoryAuthoredIntact("pkg/x_oracle_test.go") || !out.Oracles.FactoryAuthoredIntact(".buildgate/oracles.json") {
		t.Fatalf("evidence = %+v", out.Oracles)
	}
	if out.DiffStat == nil || out.DiffStat.FilesChanged != 2 || !out.DiffAvailable {
		t.Fatalf("diff evidence = %+v", out)
	}

	// A retried Activity (crash before its result was recorded) is idempotent.
	again, err := a.CommitOraclesActivity(context.Background(), in)
	if err != nil || !again.Active || again.ResultSHA != head || !again.Committed {
		t.Fatalf("retry: %+v %v", again, err)
	}
}

func TestCommitOraclesActivityRefusesChangedOracleDirectory(t *testing.T) {
	a, in, base := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	os.WriteFile(filepath.Join(in.ReferenceOracleDir, "o_test.go"), []byte("tampered\n"), 0o644)
	if _, err := a.CommitOraclesActivity(context.Background(), in); err == nil {
		t.Fatal("oracle bytes that differ from the gate-verified hash were committed")
	}
	if coGit(t, in.WorkspacePath, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved despite the refusal")
	}
}

func TestCommitOraclesActivityRecomputesRequiredContentFromTheCommittedTree(t *testing.T) {
	a, in, _ := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	in.RequiredChangedFiles = []string{"pkg/x_oracle_test.go"}
	in.RequiredContent = []string{"package pkg"}
	out, err := a.CommitOraclesActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.RequiredContentFinalFiles["pkg/x_oracle_test.go"]; got != "package pkg\n" {
		t.Fatalf("RequiredContentFinalFiles[oracle] = %q, want the committed oracle bytes", got)
	}
	// No required content declared: no recomputation.
	a2, in2, _ := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	out2, err := a2.CommitOraclesActivity(context.Background(), in2)
	if err != nil {
		t.Fatal(err)
	}
	if out2.RequiredContentFinalFiles != nil {
		t.Fatalf("RequiredContentFinalFiles = %v, want nil when the ticket declares no required content", out2.RequiredContentFinalFiles)
	}
}

// The Temporal host commit must validate each manifest target_path against the
// run's configured release protected paths BEFORE touching the checkout, (Codex review of #203).
func TestCommitOraclesActivityRefusesATargetUnderAConfiguredProtectedPath(t *testing.T) {
	a, in, base := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	in.ReleaseProtectedPaths = []string{"pkg/x_oracle_test.go"}
	if _, err := a.CommitOraclesActivity(context.Background(), in); err == nil {
		t.Fatal("a target_path under a configured protected path was committed")
	}
	if coGit(t, in.WorkspacePath, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved despite the refusal")
	}
	if _, err := os.Stat(filepath.Join(in.WorkspacePath, "pkg", "x_oracle_test.go")); err == nil {
		t.Fatal("the protected file was written before the refusal")
	}
}

// commit_oracles progress marks are emitted only when something is committed,
// so a run with no committed oracle keeps showing the stage as skipped.
func TestCommitOraclesActivityEmitsProgressMarksOnlyWhenItCommits(t *testing.T) {
	progressText := func(dir string) string {
		b, _ := os.ReadFile(progress.PathInDir(dir))
		return string(b)
	}
	a, in, _ := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	if _, err := a.CommitOraclesActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := progressText(a.logDirFor(in.RunWorkflowInput))
	var starts, passEnds int
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, `"commit_oracles"`) {
			continue
		}
		if strings.Contains(line, `"start"`) {
			starts++
		}
		if strings.Contains(line, `"end"`) && strings.Contains(line, `"pass"`) {
			passEnds++
		}
	}
	if starts != 1 || passEnds != 1 {
		t.Fatalf("want exactly one commit_oracles start and one end/pass mark after a real commit, got %d and %d:\n%s", starts, passEnds, got)
	}

	inert, inertIn, _ := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go"}]`)
	if _, err := inert.CommitOraclesActivity(context.Background(), inertIn); err != nil {
		t.Fatal(err)
	}
	if got := progressText(inert.logDirFor(inertIn.RunWorkflowInput)); strings.Contains(got, "commit_oracles") {
		t.Fatalf("an inert manifest emitted commit_oracles marks:\n%s", got)
	}
}

// ValidateOnly (the request's -no-commit-oracles) writes and commits nothing
// but still runs every collision check: an agent-planted file with different
// bytes at the target_path is refused, an absent target leaves HEAD alone.
func TestCommitOraclesActivityValidateOnlyWritesNothingButStillRefusesAPlantedTarget(t *testing.T) {
	a, in, base := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
	in.ValidateOnly = true
	out, err := a.CommitOraclesActivity(context.Background(), in)
	if err != nil || out.Active || out.Committed {
		t.Fatalf("validate-only on an absent target: %+v %v", out, err)
	}
	if coGit(t, in.WorkspacePath, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved in validate-only mode")
	}
	if _, err := os.Stat(filepath.Join(in.WorkspacePath, "pkg", "x_oracle_test.go")); err == nil {
		t.Fatal("validate-only wrote the oracle file")
	}
	if _, err := os.Stat(filepath.Join(in.WorkspacePath, ".buildgate")); err == nil {
		t.Fatal("validate-only wrote the index")
	}

	os.WriteFile(filepath.Join(in.WorkspacePath, "pkg", "x_oracle_test.go"), []byte("package pkg\n// agent's own\n"), 0o644)
	coGit(t, in.WorkspacePath, "add", "-A")
	coGit(t, in.WorkspacePath, "commit", "-q", "-m", "agent planted its own file")
	if _, err := a.CommitOraclesActivity(context.Background(), in); err == nil {
		t.Fatal("validate-only accepted an agent-planted file with different bytes at the target_path")
	}
}

// Under the opt-out even a byte-identical agent file at the target_path is
// refused: the host commits nothing, so it would ride the safety-net commit.
// Normal mode keeps accepting it.
func TestCommitOraclesActivityValidateOnlyRefusesAnIdenticalPlantedTarget(t *testing.T) {
	for _, validateOnly := range []bool{true, false} {
		a, in, _ := coFixture(t, `[{"criterion":"c","oracle_file":"o_test.go","target_path":"pkg/x_oracle_test.go"}]`)
		os.WriteFile(filepath.Join(in.WorkspacePath, "pkg", "x_oracle_test.go"), []byte("package pkg\n"), 0o644)
		coGit(t, in.WorkspacePath, "add", "-A")
		coGit(t, in.WorkspacePath, "commit", "-q", "-m", "agent wrote identical bytes")
		in.ValidateOnly = validateOnly
		_, err := a.CommitOraclesActivity(context.Background(), in)
		if validateOnly && err == nil {
			t.Fatal("validate-only accepted an agent-supplied identical file at the target_path")
		}
		if !validateOnly && err != nil {
			t.Fatalf("normal mode must still accept identical bytes: %v", err)
		}
	}
}
