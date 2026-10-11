package testfixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteEmptyFiles creates, under root, an empty file for every path listed
// in listFile (one slash-separated relative path a line, as `git ls-files`
// prints them): a repository's file names without its contents, for a check
// that only asks whether a path exists.
func WriteEmptyFiles(t testing.TB, root, listFile string) {
	t.Helper()
	list, err := os.ReadFile(listFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range strings.Split(strings.TrimSpace(string(list)), "\n") {
		if rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) {
			t.Fatalf("%s: %q is not a path inside the repository", listFile, rel)
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
