package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

func TestRoundsCommandPrintsTheLedger(t *testing.T) {
	dataDir := t.TempDir()
	r := request.New("req-3", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	sha1, sha2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	r.Tickets = []request.Ticket{{Index: 1, RoundTags: []request.RoundTag{
		{N: 1, RunID: "x", Kind: request.RoundTagBuild, SHA: sha1, Ref: request.RoundRef("req-3", 1, 1), Outcome: "quarantined", FailedChecks: []string{"spec_conformity", "code_review"}},
		{N: 2, RunID: "y", Kind: request.RoundTagConformity, SHA: sha2, Ref: request.RoundRef("req-3", 1, 2), Outcome: "accepted"},
	}}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := roundsRun([]string{"-data-dir", dataDir, "req-3"}, &out); err != nil {
		t.Fatalf("rounds: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("output = %q, want a header and two rounds", out.String())
	}
	for i, want := range [][]string{
		{"1", "1", "build", sha1[:12], "quarantined", "spec_conformity,code_review", "refs/buildgate/rounds/req-3/t1/r1", "no"},
		{"1", "2", "conformity", sha2[:12], "accepted", "-", "refs/buildgate/rounds/req-3/t1/r2", "no"},
	} {
		if got := strings.Fields(lines[i+1]); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("line %d = %v, want %v", i+1, got, want)
		}
	}

	r.Tickets[0].PRURL = "https://github.com/acme/app/pull/1"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := roundsRun([]string{"-data-dir", dataDir, "req-3"}, &out); err != nil || !strings.Contains(out.String(), "  yes") {
		t.Errorf("a pushed ticket's rounds = %q (err %v), want pushed yes", out.String(), err)
	}
	if err := roundsRun([]string{"-data-dir", dataDir, "no-such"}, &out); err == nil || !strings.Contains(err.Error(), "no request") {
		t.Errorf("rounds of an unknown request = %v, want it refused", err)
	}
}

// TestRoundRefsAreNeverInAPushRefspec: the round refs are local. No code that
// runs `git push` names them, and a real push of a branch leaves them behind.
func TestRoundRefsAreNeverInAPushRefspec(t *testing.T) {
	root := filepath.Join("..", "..")
	var pushers []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "node_modules" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(data)
		if strings.Contains(src, `"push"`) && (strings.Contains(src, "refs/buildgate/rounds") || strings.Contains(src, "RoundRefPrefix") || strings.Contains(src, "RoundRef(")) {
			pushers = append(pushers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pushers) != 0 {
		t.Errorf("%v run `git push` and name the round refs; they are never pushed", pushers)
	}

	dir := t.TempDir()
	origin, work := filepath.Join(dir, "origin.git"), filepath.Join(dir, "work")
	gitIn := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn(dir, "init", "--bare", origin)
	gitIn(dir, "init", "-b", "main", work)
	gitIn(work, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(work, "add", "f")
	gitIn(work, "commit", "-m", "one")
	sha := gitIn(work, "rev-parse", "HEAD")
	dp := newTestDeps(t)
	ref := request.RoundRef("req-3", 1, 1)
	if err := dp.CreateRef(work, ref, sha); err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
	if got := gitIn(work, "rev-parse", ref); got != sha {
		t.Fatalf("%s = %s, want %s", ref, got, sha)
	}
	if err := requestdriver.RealPushExistingBranch(context.Background(), work, sha, "feature"); err != nil {
		t.Fatalf("push: %v", err)
	}
	remote := gitIn(dir, "--git-dir", origin, "for-each-ref", "--format=%(refname)")
	if strings.Contains(remote, "refs/buildgate") || !strings.Contains(remote, "refs/heads/feature") {
		t.Errorf("origin refs = %q, want the branch and no refs/buildgate", remote)
	}
}
