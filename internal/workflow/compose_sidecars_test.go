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

// A run is known to start no compose sidecars only when compose services are
// off or the base commit positively holds no compose file. Any compose file
// makes BeginComposeServicesLifecycle create the run's network, whatever it
// holds; a git error is not an absent file.
func TestNoComposeSidecarsIsTrueOnlyWithoutAComposeFile(t *testing.T) {
	a := &Activities{}
	for name, tc := range map[string]struct {
		files map[string]string
		sha   string // overrides the commit
		want  bool
	}{
		"no file":            {files: map[string]string{"main.go": "package main"}, want: true},
		"an image service":   {files: map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n"}},
		"a build-only file":  {files: map[string]string{"compose.yaml": "services:\n  app:\n    build: .\n"}},
		"an empty services":  {files: map[string]string{"docker-compose.yml": "services: {}\n"}},
		"a rejected file":    {files: map[string]string{"compose.yml": "services:\n  db:\n    image: postgres:16\n    privileged: true\n"}},
		"a malformed file":   {files: map[string]string{"compose.yaml": "services: [unclosed\n"}},
		"an unknown commit":  {files: map[string]string{"main.go": "package main"}, sha: "0123456789012345678901234567890123456789"},
		"no commit recorded": {files: map[string]string{"main.go": "package main"}, sha: "-"},
	} {
		dir, sha := composeRepo(t, tc.files)
		switch tc.sha {
		case "":
		case "-":
			sha = ""
		default:
			sha = tc.sha
		}
		in := RunWorkflowInput{WorkspacePath: dir, BaseSHA: sha, ComposeServicesEnabled: true}
		if got := a.noComposeSidecarsFor(in, "verify"); got != tc.want {
			t.Errorf("%s: noComposeSidecarsFor = %v, want %v", name, got, tc.want)
		}
	}
	dir, sha := composeRepo(t, map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n"})
	if !a.noComposeSidecarsFor(RunWorkflowInput{WorkspacePath: dir, BaseSHA: sha}, "verify") {
		t.Errorf("compose disabled: noComposeSidecarsFor = false, want true")
	}
	// A spec the verify Activity already loaded is reused, not loaded again.
	if !a.noComposeSidecars(nil) {
		t.Errorf("no spec (compose off): noComposeSidecars = false, want true")
	}
}
