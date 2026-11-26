package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testRouteKeyPath is the credential_file a test route named name reads:
// a file under a directory of this test process (shards are separate
// processes), so a config can name it before setTestRouteKey writes it.
func testRouteKeyPath(name string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("bg-test-route-keys-%d", os.Getpid()), name)
}

// setTestRouteKey writes value to testRouteKeyPath(name), mode 0600, for the
// rest of t ("" removes it, as an unset key). Like t.Setenv, it is process
// state, so a test that calls it must not run in parallel.
func setTestRouteKey(t testing.TB, name, value string) {
	t.Helper()
	path := testRouteKeyPath(name)
	previous, readErr := os.ReadFile(path)
	t.Cleanup(func() {
		if readErr == nil {
			_ = os.WriteFile(path, previous, 0o600)
			return
		}
		_ = os.Remove(path)
	})
	if value == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
