package consolelink

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBaseURLPrecedence(t *testing.T) {
	t.Setenv(EnvVar, "")
	orig := Listening
	t.Cleanup(func() { Listening = orig })
	Listening = func(string) bool { return true }
	origAlive := processAlive
	t.Cleanup(func() { processAlive = origAlive })
	processAlive = func(int) bool { return true }
	dataDir := t.TempDir()

	if got := BaseURL("http://flag:1", dataDir); got != "http://flag:1" {
		t.Errorf("flag: got %q", got)
	}
	t.Setenv(EnvVar, "http://env:2")
	if got := BaseURL("", dataDir); got != "http://env:2" {
		t.Errorf("env: got %q", got)
	}
	t.Setenv(EnvVar, "")
	origEmbedded := embedded
	t.Cleanup(func() { embedded = origEmbedded })
	embedded = func() bool { return true }

	// Another data dir's serve on the default port is never guessed.
	if got := BaseURL("", dataDir); got != "" {
		t.Errorf("no serve recorded for this data dir: got %q, want empty", got)
	}
	remove, err := RecordServeAddress(dataDir, "127.0.0.1:8092")
	if err != nil {
		t.Fatalf("RecordServeAddress: %v", err)
	}
	if got := BaseURL("", dataDir); got != "http://127.0.0.1:8092" {
		t.Errorf("recorded and live: got %q, want this data dir's serve", got)
	}
	Listening = func(string) bool { return false }
	if got := BaseURL("", dataDir); got != "" {
		t.Errorf("recorded but nothing listening: got %q, want empty", got)
	}
	Listening = func(string) bool { return true }
	// A record whose serve died is ignored even if something else now
	// answers on its port (another data dir's serve, say).
	processAlive = func(int) bool { return false }
	if got := BaseURL("", dataDir); got != "" {
		t.Errorf("recorded serve's process gone: got %q, want empty", got)
	}
	processAlive = func(int) bool { return true }
	embedded = func() bool { return false }
	if got := BaseURL("", dataDir); got != "" {
		t.Errorf("not embedded: got %q, want empty", got)
	}
	embedded = func() bool { return true }
	remove()
	if got := BaseURL("", dataDir); got != "" {
		t.Errorf("after serve removed its record: got %q, want empty", got)
	}
}

func TestRecordServeAddressMapsWildcardToLoopback(t *testing.T) {
	orig := Listening
	t.Cleanup(func() { Listening = orig })
	Listening = func(string) bool { return true }
	// This test process is the recording serve, so the real check applies.
	dataDir := t.TempDir()
	if _, err := RecordServeAddress(dataDir, ":8095"); err != nil {
		t.Fatal(err)
	}
	if got := ServeAddress(dataDir); got != "127.0.0.1:8095" {
		t.Errorf("ServeAddress = %q, want 127.0.0.1:8095", got)
	}
	// A second serve's record is not removed by the first one's cleanup.
	removeFirst, _ := RecordServeAddress(dataDir, "127.0.0.1:8096")
	if _, err := RecordServeAddress(dataDir, "127.0.0.1:8097"); err != nil {
		t.Fatal(err)
	}
	removeFirst()
	if got := ServeAddress(dataDir); got != "127.0.0.1:8097" {
		t.Errorf("ServeAddress after the older serve's cleanup = %q, want the newer record", got)
	}
}

func TestDeepLinks(t *testing.T) {
	if got := RequestURL("http://c/", "req-1"); got != "http://c/requests/req-1" {
		t.Errorf("RequestURL = %q", got)
	}
	if got := RunURL("http://c", "run-1"); got != "http://c/runs/run-1" {
		t.Errorf("RunURL = %q", got)
	}
	if got := RunURL("", "run-1"); got != "" {
		t.Errorf("RunURL(empty base) = %q, want empty", got)
	}
}

func TestRemoteBaseURLHoldsOnlyWhileItsHostRecordExists(t *testing.T) {
	dataDir := t.TempDir()
	hostRecord := filepath.Join(t.TempDir(), "profile.remote-console")
	if err := os.WriteFile(hostRecord, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RemoteBaseURL(dataDir); got != "" {
		t.Fatalf("RemoteBaseURL with nothing recorded = %q", got)
	}
	if err := RecordRemoteBaseURL(dataDir, "https://console.example:8443", hostRecord); err != nil {
		t.Fatal(err)
	}
	if got := RemoteBaseURL(dataDir); got != "https://console.example:8443" {
		t.Errorf("RemoteBaseURL = %q", got)
	}
	if got := RequestURL(RemoteBaseURL(dataDir), "req-1"); got != "https://console.example:8443/requests/req-1" {
		t.Errorf("request link = %q", got)
	}
	if err := os.Remove(hostRecord); err != nil {
		t.Fatal(err)
	}
	if got := RemoteBaseURL(dataDir); got != "" {
		t.Errorf("RemoteBaseURL after the remote console was turned off = %q, want none", got)
	}
}

func TestRemoteBaseURLRefusesAnythingButAnHTTPSHost(t *testing.T) {
	hostRecord := filepath.Join(t.TempDir(), "profile.remote-console")
	if err := os.WriteFile(hostRecord, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{
		"http://console.example",
		"https://user:secret@console.example",
		"https://console.example/#gate=secret",
		"https://console.example/?t=secret",
		"https://console.example/elsewhere",
		"console.example",
	} {
		dataDir := t.TempDir()
		if err := RecordRemoteBaseURL(dataDir, base, hostRecord); err != nil {
			t.Fatal(err)
		}
		if got := RemoteBaseURL(dataDir); got != "" {
			t.Errorf("RemoteBaseURL for %q = %q, want none", base, got)
		}
	}
}

func TestTabNotifierIsPresentOnlyWhileFresh(t *testing.T) {
	dataDir := t.TempDir()
	if TabNotifierPresent(dataDir) {
		t.Fatal("present with nothing recorded")
	}
	if err := TouchTabNotifier(dataDir); err != nil {
		t.Fatal(err)
	}
	if !TabNotifierPresent(dataDir) {
		t.Fatal("not present right after a touch")
	}
	old := time.Now().Add(-2 * TabNotifierFresh)
	if err := os.Chtimes(filepath.Join(dataDir, tabNotifierFile), old, old); err != nil {
		t.Fatal(err)
	}
	if TabNotifierPresent(dataDir) {
		t.Error("still present after the serve that touched it stopped")
	}
	if err := TouchTabNotifier(dataDir); err != nil {
		t.Fatal(err)
	}
	if !TabNotifierPresent(dataDir) {
		t.Error("a touch of an old record did not refresh it")
	}
}
