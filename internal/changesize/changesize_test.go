package changesize_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/changesize"
	"buildgate/internal/runner"
)

func TestMeasureCommittedSourceDiffIgnoresRepositoryAttributes(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		filename := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	message := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(message, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(".gitattributes", "*.go -diff\n*.py -diff\n")
	write("src/main.go", "package p\n\nfunc Alpha() {\n println(1)\n println(2)\n}\nfunc Beta() {\n println(1)\n}\n")
	write("src/nested.py", "class Thing:\n    def method(self):\n        return 'old'\n")
	for _, name := range []string{"src/main_test.go", "src/special.go", "docs/code.go", "src/model.pb.go", "go.sum", "README.md"} {
		write(name, "old\n")
	}
	write("src/image.bin", "old\x00binary\n")
	git("add", ".")
	git("commit", "-q", "-F", message)
	base := git("rev-parse", "HEAD")
	write("src/main.go", "package p\n\nfunc Alpha() {\n println(3)\n}\nfunc Beta() {\n println(3)\n}\n")
	write("src/nested.py", "class Thing:\n    def method(self):\n        return 'new'\n")
	for _, name := range []string{"src/main_test.go", "src/special.go", "docs/code.go", "src/model.pb.go", "go.sum", "README.md"} {
		write(name, "new\n")
	}
	write("src/image.bin", "new\x00binary\n")
	git("add", ".")
	git("commit", "-q", "-F", message)
	result := git("rev-parse", "HEAD")
	// Even the repository's highest-priority info/attributes must not select
	// a driver or make the source appear binary.
	write(".git/info/attributes", "*.go -diff\n*.py -diff\n")
	view, err := runner.NewSizeDiff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = view.Close() })
	got, err := changesize.Measure(context.Background(), view, base, result, []string{"*_test.go", "special.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceLines != 7 || got.SourceFiles != 2 || got.Functions != 3 || len(got.Files) != 2 {
		t.Fatalf("size = %+v, want 7 source lines, 2 files, 3 functions", got)
	}
	file := got.Files[0]
	if file.Path != "src/main.go" || file.Insertions != 2 || file.Deletions != 3 || !reflect.DeepEqual(file.Functions, []string{"func Alpha() {", "func Beta() {"}) {
		t.Fatalf("file = %+v, want the two git hunk names and 2 additions / 3 deletions", file)
	}
	python := got.Files[1]
	if python.Path != "src/nested.py" || !reflect.DeepEqual(python.Functions, []string{"def method(self):"}) {
		t.Fatalf("Python hunk names = %+v, want the built-in Python driver's indented method", python)
	}
}
