package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A host read of a trusted commit returns that commit's own bytes: a replace
// ref for the commit and a GIT_DIR in the caller's environment that names
// another repository are both ignored.
func TestBlobAtCommitIgnoresReplaceRefsAndInheritedGitVariables(t *testing.T) {
	repo := newFixtureRepo(t)
	commitFactoryYML(t, repo, "setup:\n  - make real\n")
	realCommit := headOf(t, repo)

	// Another repository with the same history, in which the commit is
	// replaced by one whose file differs.
	other := filepath.Join(t.TempDir(), "other")
	if out, err := exec.Command("cp", "-R", repo, other).CombinedOutput(); err != nil {
		t.Fatalf("copy repository: %v: %s", err, out)
	}
	commitFactoryYML(t, other, "setup:\n  - make substitute\n")
	substitute := headOf(t, other)
	if out, err := runGit(t, other, "replace", realCommit, substitute); err != nil {
		t.Fatalf("git replace: %v: %s", err, out)
	}

	read := func(t *testing.T, dir string) string {
		t.Helper()
		data, found, err := realHost{}.blobAtCommit(context.Background(), dir, realCommit, ".factory.yml")
		if err != nil {
			return "refused: " + err.Error()
		}
		if !found {
			return "not found"
		}
		return string(data)
	}
	t.Run("a replace ref", func(t *testing.T) {
		if got := read(t, other); !strings.Contains(got, "make real") {
			t.Errorf("read %q, want the commit's own file", got)
		}
	})
	t.Run("GIT_DIR names another repository", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		t.Setenv("GIT_WORK_TREE", other)
		got := read(t, repo)
		if strings.Contains(got, "substitute") || !(strings.Contains(got, "make real") || strings.HasPrefix(got, "refused: ")) {
			t.Errorf("read %q, want the commit's own file or a refusal", got)
		}
	})
	if _, err := os.Stat(filepath.Join(other, ".git")); err != nil {
		t.Fatal(err)
	}
}
