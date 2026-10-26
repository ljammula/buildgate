package requestdrivertest

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// UpgradeFixture copies the data directory of a request as v0.0.13 wrote it
// (internal/request/testdata/v0.0.13/<name>) to a new temporary directory and
// returns it. The record's data directory and workspace are the new temporary
// directories, and the workspace declares verify_command: make verify, as the
// repository the request was made against did. The request's id is req-1.
func UpgradeFixture(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the fixtures")
	}
	src := filepath.Join(filepath.Dir(file), "..", "..", "request", "testdata", "v0.0.13", name)
	dst := t.TempDir()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: make verify\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b = bytes.ReplaceAll(b, []byte("@DATA@"), []byte(dst))
		b = bytes.ReplaceAll(b, []byte("@WORKSPACE@"), []byte(workspace))
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}
