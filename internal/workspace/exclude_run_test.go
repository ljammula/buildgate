package workspace

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func excludeText(t *testing.T, dir string) string {
	t.Helper()
	path, err := infoExcludePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// A release removes its own block only: the same pattern in another run's
// block, and a line another writer added in between, stay as they were.
func TestExcludeForRunReleaseKeepsEveryOtherLine(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	a, err := ExcludeForRun(repo, "a", []string{"/p/held_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ExcludeForRun(repo, "b", []string{"/p/held_test.go", "/q/x_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ExcludeHarnessArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	a()
	a()
	got := excludeText(t, repo)
	if strings.Count(got, "/p/held_test.go") != 1 || !strings.Contains(got, "/q/x_test.go") || !strings.Contains(got, "BUILD_REPORT.md") {
		t.Fatalf("after releasing a: %q", got)
	}
	b()
	got = excludeText(t, repo)
	if strings.Contains(got, "held_test.go") || strings.Contains(got, "factoryd-run") || !strings.Contains(got, "BUILD_REPORT.md") {
		t.Errorf("after releasing b: %q", got)
	}
}

func TestExcludeForRunRefusesAMultiLineToken(t *testing.T) {
	if _, err := ExcludeForRun(t.TempDir(), "a\nb", nil); err == nil {
		t.Error("a token with a newline was accepted")
	}
}

// A retried add with the same token (a retried activity) replaces its block:
// one block, the newest patterns, and one release leaves nothing.
func TestExcludeForRunRetryWithTheSameTokenLeavesOneBlock(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := excludeText(t, repo)
	if _, err := ExcludeForRun(repo, "run-1@abc", []string{"/old_test.go"}); err != nil {
		t.Fatal(err)
	}
	release, err := ExcludeForRun(repo, "run-1@abc", []string{"/new_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	got := excludeText(t, repo)
	if strings.Count(got, excludeBlockBegin) != 1 || strings.Contains(got, "old_test.go") || !strings.Contains(got, "/new_test.go") {
		t.Fatalf("after the retry: %q", got)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if got := excludeText(t, repo); got != before {
		t.Errorf("after the release: %q", got)
	}
}

func TestExcludeForRunRefusesAMultiLinePattern(t *testing.T) {
	for _, p := range []string{"/a\n/b", "/a\r/b"} {
		if _, err := ExcludeForRun(t.TempDir(), "t", []string{p}); err == nil {
			t.Errorf("pattern %q was accepted", p)
		}
	}
}
