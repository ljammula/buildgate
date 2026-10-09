package projectconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func headIn(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestConfirmReadCommitReturnsTheCommitTheFileWasReadFrom(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, FileName, "verify_command: make verify\n")
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	before := HeadCommit(sub)
	if before != headIn(t, root) {
		t.Fatalf("HeadCommit = %q, want HEAD %q", before, headIn(t, root))
	}
	cfg, found, err := Load(sub)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	got, err := ConfirmReadCommit(sub, before, cfg.SHA256)
	if err != nil || got != before {
		t.Fatalf("ConfirmReadCommit = %q, %v; want %q", got, err, before)
	}
	// A worktree edit after the commit changes neither the read nor the proof.
	if err := os.WriteFile(filepath.Join(root, FileName), []byte("verify_command: rm -rf /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ConfirmReadCommit(root, before, cfg.SHA256); err != nil || got != before {
		t.Errorf("after a worktree edit: %q, %v", got, err)
	}
}

func TestConfirmReadCommitForARepositoryWithoutTheFile(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, "README.md", "x\n")
	got, err := ConfirmReadCommit(root, HeadCommit(root), "")
	if err != nil || got != headIn(t, root) {
		t.Fatalf("ConfirmReadCommit = %q, %v; want HEAD", got, err)
	}
	// The file was read as present, and this commit has none.
	if _, err := ConfirmReadCommit(root, "", strings.Repeat("a", 64)); err == nil {
		t.Error("a hash for a file the commit lacks was confirmed")
	}
}

func TestConfirmReadCommitRefusesAHeadThatMoved(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, FileName, "verify_command: make verify\n")
	before := HeadCommit(root)
	cfg, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, root, FileName, "verify_command: make other\n")
	if _, err := ConfirmReadCommit(root, before, cfg.SHA256); err == nil || !strings.Contains(err.Error(), "HEAD moved") {
		t.Errorf("error = %v, want a refusal naming the moved HEAD", err)
	}
	// Without the earlier commit to compare, the hash still catches it.
	if _, err := ConfirmReadCommit(root, "", cfg.SHA256); err == nil || !strings.Contains(err.Error(), "not the file that was read") {
		t.Errorf("error = %v, want a refusal for the changed file", err)
	}
}

func TestHeadCommitIsEmptyWithoutACommit(t *testing.T) {
	if got := HeadCommit(t.TempDir()); got != "" {
		t.Errorf("outside git: HeadCommit = %q", got)
	}
	root := t.TempDir()
	initGitRepo(t, root)
	if got := HeadCommit(root); got != "" {
		t.Errorf("no commit yet: HeadCommit = %q", got)
	}
	if got, err := ConfirmReadCommit(root, "", ""); got != "" || err != nil {
		t.Errorf("no commit yet: ConfirmReadCommit = %q, %v", got, err)
	}
}
