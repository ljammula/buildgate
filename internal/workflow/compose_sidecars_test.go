package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// composeRepo is a git repository with one commit holding files; it returns
// the directory and the commit.
func composeRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "c")
	return dir, git("rev-parse", "HEAD")
}

// A run launches compose sidecars only when its repository has a compose file
// at the base commit with a service the factory would launch; compose being
// enabled alone, which is the default, is not enough.
func TestLaunchesComposeSidecarsNeedsAComposeFileWithAService(t *testing.T) {
	a := &Activities{}
	for name, tc := range map[string]struct {
		files map[string]string
		want  bool
	}{
		"no compose file": {map[string]string{"main.go": "package main"}, false},
		"a service":       {map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n"}, true},
	} {
		dir, sha := composeRepo(t, tc.files)
		in := RunWorkflowInput{WorkspacePath: dir, BaseSHA: sha, ComposeServicesEnabled: true}
		if got := a.launchesComposeSidecars(in, "verify"); got != tc.want {
			t.Errorf("%s: launchesComposeSidecars = %v, want %v", name, got, tc.want)
		}
	}
	dir, sha := composeRepo(t, map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n"})
	if got := a.launchesComposeSidecars(RunWorkflowInput{WorkspacePath: dir, BaseSHA: sha}, "verify"); got {
		t.Errorf("compose disabled: launchesComposeSidecars = true, want false")
	}
}
