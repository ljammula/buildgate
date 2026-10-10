package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/consolelink"
	"buildgate/internal/run"
)

func requestNotification() Notification {
	return Notification{
		RequestID: "req-1",
		Ask:       "Spec ready for your review",
		Subject:   "checkouts: Add a coupon field",
		Reason:    "Read it, then approve or request changes.",
		State:     run.State("spec_review"),
		Next:      "factoryd approve req-1",
		Link:      "http://127.0.0.1:1/requests/req-1",
	}
}

// recordingNotifier writes a script that records its arguments, one per
// line, in the file it returns, and exits with status.
func recordingNotifier(t *testing.T, status string) (script, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script = filepath.Join(dir, "notifier.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+argsFile+"\"\nexit "+status+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, argsFile
}

func TestRequestBannerSaysWhatIsAskedAndItsClickOpensTheRequestPage(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	script, argsFile := recordingNotifier(t, "0")
	withDesktopClickNotifierLookPath(t, func() (string, error) { return script, nil })

	if err := (DesktopNotifier{}).Notify(context.Background(), requestNotification()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"-title", "Spec ready for your review",
		"-subtitle", "checkouts: Add a coupon field",
		"-message", "Read it, then approve or request changes.",
		"-execute", "open 'http://127.0.0.1:1/requests/req-1'",
		"-group", "buildgate-req-1",
	}, "\n")
	if got := string(b); !strings.HasPrefix(got, want+"\n") {
		t.Errorf("terminal-notifier arguments:\n%s\nwant them to start:\n%s", got, want)
	}
	if strings.Contains(string(b), "factoryd approve") {
		t.Errorf("the banner names a command; its one action is its click:\n%s", b)
	}
}

func TestBannerFallsBackToOsascriptWhenTerminalNotifierIsRefused(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	refused, _ := recordingNotifier(t, "3")
	withDesktopClickNotifierLookPath(t, func() (string, error) { return refused, nil })
	osascript, argsFile := recordingNotifier(t, "0")
	withDesktopNotifierLookPath(t, func() (string, error) { return osascript, nil })

	if err := (DesktopNotifier{}).Notify(context.Background(), requestNotification()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("osascript was not run after terminal-notifier exited 3: %v", err)
	}
	for _, want := range []string{
		`with title "Spec ready for your review"`,
		`subtitle "checkouts: Add a coupon field"`,
		"Open the console to act on it",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("osascript script %q does not contain %q", b, want)
		}
	}
}

func TestHostBannerStandsDownOnlyForARequestWhileATabNotifies(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	withDesktopClickNotifierLookPath(t, func() (string, error) { return "", errNotFoundForTest })
	osascript, argsFile := recordingNotifier(t, "0")
	withDesktopNotifierLookPath(t, func() (string, error) { return osascript, nil })
	shown := func() bool {
		_, err := os.Stat(argsFile)
		_ = os.Remove(argsFile)
		return err == nil
	}

	dataDir := t.TempDir()
	DispatchDesktop(dataDir, requestNotification())
	if !shown() {
		t.Fatal("no banner with no console tab notifying")
	}
	if err := consolelink.TouchTabNotifier(dataDir); err != nil {
		t.Fatal(err)
	}
	DispatchDesktop(dataDir, requestNotification())
	if shown() {
		t.Error("the host banner was shown for a request while a console tab raises its notifications")
	}
	DispatchDesktop(dataDir, Notification{RunID: "run-1", Ticket: "t", State: run.StateHalted, Reason: "halted"})
	if !shown() {
		t.Error("a run's own notification was held back; no tab raises it")
	}
}

func TestSlackAndDiscordLinkToTheRemoteConsoleWhenOneIsRecorded(t *testing.T) {
	n := requestNotification()
	n.RemoteLink = "https://console.example/requests/req-1"
	for name, send := range map[string]func(url string) error{
		"slack":   func(url string) error { return SlackNotifier{WebhookURL: url}.Notify(context.Background(), n) },
		"discord": func(url string) error { return DiscordNotifier{WebhookURL: url}.Notify(context.Background(), n) },
	} {
		var body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusOK)
		}))
		if err := send(srv.URL); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		srv.Close()
		if !strings.Contains(body, "https://console.example/requests/req-1") || strings.Contains(body, "127.0.0.1") {
			t.Errorf("%s message %s: want the remote console link and not the loopback one", name, body)
		}
		if !strings.Contains(body, "Spec ready for your review: checkouts: Add a coupon field (request req-1)") {
			t.Errorf("%s message %s does not lead with what is asked", name, body)
		}
	}
}

func TestTerminalNotifierTextCannotBeReadAsAnOption(t *testing.T) {
	for in, want := range map[string]string{
		"-sound default":    `\-sound default`,
		"[ci] fix the gate": `\[ci] fix the gate`,
		"Add a coupon":      "Add a coupon",
		"":                  "",
	} {
		if got := terminalNotifierText(in); got != want {
			t.Errorf("terminalNotifierText(%q) = %q, want %q", in, got, want)
		}
	}
}
