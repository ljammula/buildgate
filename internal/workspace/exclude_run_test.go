package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestDropAllRunBlocksKeepsEveryOtherLineAndATornBlock(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	path, err := infoExcludePath(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# own\n  /keep me  \n# factoryd-run-begin a\n/p/x_test.go\n# factoryd-run-end a\n/mid\n# factoryd-run-begin b\n/q\n# factoryd-run-end b\nlast-no-newline")
	if err := DropAllRunBlocks(repo); err != nil {
		t.Fatal(err)
	}
	if got, want := excludeText(t, repo), "# own\n  /keep me  \n/mid\nlast-no-newline"; got != want {
		t.Fatalf("after drop = %q, want %q", got, want)
	}
	torn := "/a\n# factoryd-run-begin a\n/p/x_test.go\n# factoryd-run-end a\n# factoryd-run-begin t\n/torn\n"
	write(torn)
	if err := DropAllRunBlocks(repo); err != nil {
		t.Fatal(err)
	}
	if got := excludeText(t, repo); got != torn {
		t.Fatalf("torn file changed: %q", got)
	}
	write("# factoryd-run-begin a\n/x\n# factoryd-run-end a\n")
	if err := DropAllRunBlocks(repo); err != nil {
		t.Fatal(err)
	}
	if got := excludeText(t, repo); got != "" {
		t.Fatalf("only blocks: left %q", got)
	}
}
