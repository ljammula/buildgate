package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestOpenInBrowserNeverPutsURLOnArgv is the regression test from an
// adversarial review: openInBrowser must pass host.browserCommand a local
// file path, never the raw URL (with its start-token fragment) -- argv is
// visible to any local user via `ps eww <pid>`. Before that fix,
// openInBrowser called host.browserCommand(rawURL) directly; this test
// fails against that old behavior (the captured argument would
// equal/contain the URL) and passes only once the redirect-file
// indirection is in place.
func TestOpenInBrowserNeverPutsURLOnArgv(t *testing.T) {
	dp := newTestDeps(t)
	const secretURL = "http://127.0.0.1:8090/requests/req-1#t=super-secret-token"

	var gotArg string
	orig := fakeHostOf(dp).browserCommandFn
	fakeHostOf(dp).browserCommandFn = func(target string) *exec.Cmd {
		gotArg = target
		return exec.Command("true")
	}
	origDelay := openInBrowserCleanupDelay
	openInBrowserCleanupDelay = time.Hour // don't race the cleanup during the test
	t.Cleanup(func() {
		fakeHostOf(dp).browserCommandFn = orig
		openInBrowserCleanupDelay = origDelay
	})

	if err := openInBrowser(dp, secretURL); err != nil {
		t.Fatalf("openInBrowser: %v", err)
	}

	if gotArg == "" {
		t.Fatal("openBrowserCommand was never called")
	}
	if strings.Contains(gotArg, "super-secret-token") {
		t.Fatalf("openBrowserCommand argument leaked the token: %q", gotArg)
	}
	if gotArg == secretURL {
		t.Fatalf("openBrowserCommand was called with the raw URL, want a local redirect file path")
	}

	data, err := os.ReadFile(gotArg)
	if err != nil {
		t.Fatalf("read redirect file %s: %v", gotArg, err)
	}
	if !strings.Contains(string(data), secretURL) {
		t.Errorf("redirect file content = %q, want it to carry the real URL for location.replace", data)
	}
}

// TestWriteBrowserRedirectFilePermissions proves the redirect file is
// created in a 0700 directory with the file itself 0600 -- the same
// adversarial-review permission requirement above, since the file
// transiently carries the same token the URL itself does.
func TestWriteBrowserRedirectFilePermissions(t *testing.T) {
	path, cleanup, err := writeBrowserRedirectFile("http://example/#t=abc")
	if err != nil {
		t.Fatalf("writeBrowserRedirectFile: %v", err)
	}
	defer cleanup()

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("redirect file mode = %v, want 0600", fileInfo.Mode().Perm())
	}

	dirInfo, err := os.Stat(dirOf(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("redirect dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
}

// TestWriteBrowserRedirectFileEscapesURLSafely proves a URL containing
// characters that could break out of a naive JS string literal (a
// backslash, a quote) is encoded safely rather than corrupting the
// script or, worse, letting the "URL" inject arbitrary script.
func TestWriteBrowserRedirectFileEscapesURLSafely(t *testing.T) {
	tricky := `http://example/#t=abc"; alert(1); //`
	path, cleanup, err := writeBrowserRedirectFile(tricky)
	if err != nil {
		t.Fatalf("writeBrowserRedirectFile: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `t=abc"; alert(1)`) {
		t.Errorf("redirect file content = %q, the tricky URL was not JSON/JS-string-escaped", data)
	}
}

// TestOpenInBrowserCleansUpAfterDelay proves the redirect file/directory
// are removed once openInBrowserCleanupDelay elapses.
func TestOpenInBrowserCleansUpAfterDelay(t *testing.T) {
	dp := newTestDeps(t)
	var gotArg string
	orig := fakeHostOf(dp).browserCommandFn
	fakeHostOf(dp).browserCommandFn = func(target string) *exec.Cmd {
		gotArg = target
		return exec.Command("true")
	}
	origDelay := openInBrowserCleanupDelay
	openInBrowserCleanupDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		fakeHostOf(dp).browserCommandFn = orig
		openInBrowserCleanupDelay = origDelay
	})

	if err := openInBrowser(dp, "http://example/#t=abc"); err != nil {
		t.Fatalf("openInBrowser: %v", err)
	}
	if gotArg == "" {
		t.Fatal("openBrowserCommand was never called")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(gotArg); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("redirect file %s was not cleaned up within the deadline", gotArg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "."
	}
	return path[:i]
}
