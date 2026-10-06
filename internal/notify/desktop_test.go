package notify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// withDesktopOS temporarily overrides desktopOS for the duration of a test.
func withDesktopOS(t *testing.T, os string) {
	t.Helper()
	orig := desktopOS
	desktopOS = os
	t.Cleanup(func() { desktopOS = orig })
}

// withDesktopNotifierLookPath temporarily overrides desktopNotifierLookPath.
func withDesktopNotifierLookPath(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := desktopNotifierLookPath
	desktopNotifierLookPath = fn
	t.Cleanup(func() { desktopNotifierLookPath = orig })
}

// withDesktopClickNotifierLookPath temporarily overrides
// desktopClickNotifierLookPath.
func withDesktopClickNotifierLookPath(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := desktopClickNotifierLookPath
	desktopClickNotifierLookPath = fn
	t.Cleanup(func() { desktopClickNotifierLookPath = orig })
}

func TestDesktopNotifierNoOpsWhenOptedOut(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "0")
	withDesktopOS(t, "darwin")
	called := false
	withDesktopNotifierLookPath(t, func() (string, error) {
		called = true
		return "/usr/bin/osascript", nil
	})

	if err := (DesktopNotifier{}).Notify(context.Background(), Notification{}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if called {
		t.Error("desktopNotifierLookPath was called, want the opt-out env var to short-circuit before it")
	}
}

// TestDesktopNotifierIsOnByDefault proves an unset env var means on:
// the notifier reaches the (injected) osascript lookup instead of
// short-circuiting, so an engineer gets notifications without opting in.
func TestDesktopNotifierIsOnByDefault(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "")
	withDesktopOS(t, "darwin")
	called := false
	withDesktopNotifierLookPath(t, func() (string, error) {
		called = true
		return "/usr/bin/true", nil
	})

	if err := (DesktopNotifier{}).Notify(context.Background(), Notification{RunID: "run-1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !called {
		t.Error("desktopNotifierLookPath was never called, want an unset env var to leave desktop notifications on")
	}
}

func TestDesktopNotifierNoOpsOnNonDarwin(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "linux")
	called := false
	withDesktopNotifierLookPath(t, func() (string, error) {
		called = true
		return "/usr/bin/osascript", nil
	})

	if err := (DesktopNotifier{}).Notify(context.Background(), Notification{}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if called {
		t.Error("desktopNotifierLookPath was called, want the non-darwin check to short-circuit before it")
	}
}

// TestDesktopNotifierNoOpsWhenOsascriptMissing proves the "osascript not
// on PATH" case never errors and never touches a real binary — the
// injected lookup always fails, so this test can never accidentally
// shell out to the machine's real osascript.
func TestDesktopNotifierNoOpsWhenOsascriptMissing(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	withDesktopNotifierLookPath(t, func() (string, error) {
		return "", errNotFoundForTest
	})

	if err := (DesktopNotifier{}).Notify(context.Background(), Notification{}); err != nil {
		t.Fatalf("Notify: %v, want nil (best-effort, never errors)", err)
	}
}

// TestDesktopNotifierUsesInjectedBinary proves Notify shells out to
// whatever desktopNotifierLookPath resolves, rather than a hardcoded
// "osascript" — this is what lets tests point it at a harmless stand-in
// binary instead of a real osascript.
func TestDesktopNotifierUsesInjectedBinary(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	// /usr/bin/true exists on darwin and Linux CI runners alike, ignores
	// its arguments, and exits 0 -- a harmless stand-in that proves the
	// injected path is actually invoked without ever calling osascript.
	withDesktopNotifierLookPath(t, func() (string, error) {
		return "/usr/bin/true", nil
	})

	if err := (DesktopNotifier{}).Notify(context.Background(), Notification{RunID: "run-1"}); err != nil {
		t.Fatalf("Notify: %v, want nil (best-effort, never errors)", err)
	}
}

// TestDesktopNotifierPrefersClickNotifierWhenRunDirSet proves that once a
// notification carries a RunDir, Notify shells out to the click notifier
// and never even looks up osascript -- the two are mutually exclusive per
// call, not "try click, then also plain-notify".
func TestDesktopNotifierPrefersClickNotifierWhenRunDirSet(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	clickCalled := false
	withDesktopClickNotifierLookPath(t, func() (string, error) {
		clickCalled = true
		return "/usr/bin/true", nil
	})
	osascriptCalled := false
	withDesktopNotifierLookPath(t, func() (string, error) {
		osascriptCalled = true
		return "/usr/bin/osascript", nil
	})

	n := Notification{RunID: "run-1", Ticket: "TICK-1", Reason: "run halted", RunDir: "/data/runs/run-1"}
	if err := (DesktopNotifier{}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v, want nil (best-effort, never errors)", err)
	}
	if !clickCalled {
		t.Error("desktopClickNotifierLookPath was never called, want it consulted first when RunDir is set")
	}
	if osascriptCalled {
		t.Error("desktopNotifierLookPath was called, want the click-notifier path to short-circuit before it")
	}
}

// TestDesktopNotifierFallsBackToOsascriptWhenRunDirEmpty proves an older
// or path-less notification (RunDir == "") never even attempts the click
// notifier -- there would be nothing for -execute to open.
func TestDesktopNotifierFallsBackToOsascriptWhenRunDirEmpty(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	clickCalled := false
	withDesktopClickNotifierLookPath(t, func() (string, error) {
		clickCalled = true
		return "/usr/bin/true", nil
	})
	osascriptCalled := false
	withDesktopNotifierLookPath(t, func() (string, error) {
		osascriptCalled = true
		return "/usr/bin/osascript", nil
	})

	if err := (DesktopNotifier{}).Notify(context.Background(), Notification{RunID: "run-1"}); err != nil {
		t.Fatalf("Notify: %v, want nil (best-effort, never errors)", err)
	}
	if clickCalled {
		t.Error("desktopClickNotifierLookPath was called, want an empty RunDir to skip it entirely")
	}
	if !osascriptCalled {
		t.Error("desktopNotifierLookPath was never called, want the empty-RunDir path to fall back to it")
	}
}

// TestDesktopNotifierFallsBackWhenClickNotifierMissing proves a RunDir
// being set doesn't itself guarantee the click notifier runs -- when it's
// not on PATH, Notify still falls back to osascript rather than going
// silent.
func TestDesktopNotifierFallsBackWhenClickNotifierMissing(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")
	withDesktopClickNotifierLookPath(t, func() (string, error) {
		return "", errNotFoundForTest
	})
	osascriptCalled := false
	withDesktopNotifierLookPath(t, func() (string, error) {
		osascriptCalled = true
		return "/usr/bin/osascript", nil
	})

	n := Notification{RunID: "run-1", RunDir: "/data/runs/run-1"}
	if err := (DesktopNotifier{}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v, want nil (best-effort, never errors)", err)
	}
	if !osascriptCalled {
		t.Error("desktopNotifierLookPath was never called, want the missing-click-notifier path to fall back to it")
	}
}

// TestDesktopNotifierClickTargetPrefersLinkOverRunDir proves the
// terminal-notifier -execute command opens Link (a console URL) when
// set, not RunDir, and that the banner message gets a trailing "Next:"
// line -- captured via a fake terminal-notifier stand-in that records
// its own argv, since exec.CommandContext's target binary is stubbed
// out, not spied on, by the other tests in this file.
func TestDesktopNotifierClickTargetPrefersLinkOverRunDir(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")

	argsFile := filepath.Join(t.TempDir(), "args")
	script := filepath.Join(t.TempDir(), "fake-terminal-notifier.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+argsFile+"\"\n"), 0o755); err != nil {
		t.Fatalf("write fake terminal-notifier: %v", err)
	}
	withDesktopClickNotifierLookPath(t, func() (string, error) { return script, nil })

	n := Notification{
		RunID:  "run-1",
		Ticket: "TICK-1",
		Reason: "run halted",
		Next:   "factoryd retry req-1",
		RunDir: "/data/runs/run-1",
		Link:   "https://console.example/requests/req-1",
	}
	if err := (DesktopNotifier{}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	gotArgs := string(got)
	if !strings.Contains(gotArgs, n.Link) {
		t.Errorf("recorded args = %q, want -execute to open Link %q", gotArgs, n.Link)
	}
	if strings.Contains(gotArgs, n.RunDir) {
		t.Errorf("recorded args = %q, want RunDir not used when Link is set", gotArgs)
	}
	if !strings.Contains(gotArgs, "run halted. Next: factoryd retry req-1") {
		t.Errorf("recorded args = %q, want the message to include the reason and a Next: suffix", gotArgs)
	}
}

// TestDesktopNotifierClickTargetFallsBackToRunDirWhenLinkEmpty proves
// the pre-existing behavior (open RunDir) still holds when a
// notification has no console Link.
func TestDesktopNotifierClickTargetFallsBackToRunDirWhenLinkEmpty(t *testing.T) {
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")

	argsFile := filepath.Join(t.TempDir(), "args")
	script := filepath.Join(t.TempDir(), "fake-terminal-notifier.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+argsFile+"\"\n"), 0o755); err != nil {
		t.Fatalf("write fake terminal-notifier: %v", err)
	}
	withDesktopClickNotifierLookPath(t, func() (string, error) { return script, nil })

	n := Notification{RunID: "run-1", Reason: "run halted", RunDir: "/data/runs/run-1"}
	if err := (DesktopNotifier{}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	if !strings.Contains(string(got), n.RunDir) {
		t.Errorf("recorded args = %q, want -execute to open RunDir when Link is empty", got)
	}
}

// recordClickNotifierArgs installs a fake terminal-notifier that records
// its argv, fires n through DesktopNotifier, and returns the recorded
// args one per element.
func recordClickNotifierArgs(t *testing.T, n Notification) []string {
	t.Helper()
	t.Setenv(DesktopNotificationsEnvironmentVariable, "1")
	withDesktopOS(t, "darwin")

	argsFile := filepath.Join(t.TempDir(), "args")
	script := filepath.Join(t.TempDir(), "fake-terminal-notifier.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+argsFile+"\"\n"), 0o755); err != nil {
		t.Fatalf("write fake terminal-notifier: %v", err)
	}
	withDesktopClickNotifierLookPath(t, func() (string, error) { return script, nil })

	if err := (DesktopNotifier{}).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
}

// withDesktopIconCacheDir temporarily overrides desktopIconCacheDir.
func withDesktopIconCacheDir(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := desktopIconCacheDir
	desktopIconCacheDir = fn
	t.Cleanup(func() { desktopIconCacheDir = orig })
}

// TestDesktopNotifierAttachesBuildgateIcon proves the click-notifier path
// passes -contentImage pointing at a file holding the embedded Buildgate
// icon, and that a stale copy left by an older binary is replaced.
func TestDesktopNotifierAttachesBuildgateIcon(t *testing.T) {
	cache := t.TempDir()
	withDesktopIconCacheDir(t, func() (string, error) { return cache, nil })
	stale := filepath.Join(cache, "buildgate", "notification-icon.png")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old icon"), 0o644); err != nil {
		t.Fatal(err)
	}

	args := recordClickNotifierArgs(t, Notification{RunID: "run-1", Reason: "run halted", RunDir: "/data/runs/run-1"})

	var icon string
	for i, a := range args {
		if a == "-contentImage" && i+1 < len(args) {
			icon = args[i+1]
		}
	}
	if icon != stale {
		t.Fatalf("recorded args = %q, want -contentImage %q", args, stale)
	}
	got, err := os.ReadFile(icon)
	if err != nil {
		t.Fatalf("read icon: %v", err)
	}
	if !bytes.Equal(got, buildgateIcon) {
		t.Errorf("icon file holds %d bytes, want the embedded %d-byte Buildgate icon", len(got), len(buildgateIcon))
	}
}

// TestDesktopNotifierOmitsIconWhenCacheUnavailable proves an icon failure
// never costs the notification itself: the banner still fires, just
// without -contentImage.
func TestDesktopNotifierOmitsIconWhenCacheUnavailable(t *testing.T) {
	withDesktopIconCacheDir(t, func() (string, error) { return "", errors.New("no cache dir") })

	args := recordClickNotifierArgs(t, Notification{RunID: "run-1", Reason: "run halted", RunDir: "/data/runs/run-1"})

	if slices.Contains(args, "-contentImage") {
		t.Errorf("recorded args = %q, want no -contentImage when the cache dir is unavailable", args)
	}
	if !slices.Contains(args, "-message") {
		t.Errorf("recorded args = %q, want the notification still sent", args)
	}
}

func TestShellQuoteSingleEscapesEmbeddedQuote(t *testing.T) {
	// A crafted RunDir containing a single quote, a `$`, and a backtick
	// must not be able to break out of the single-quoted shell string
	// terminal-notifier's -execute value interpolates it into, nor
	// trigger shell expansion once it's inside.
	in := `/data/runs/it's-$(rm -rf ~)-` + "`whoami`"
	got := shellQuoteSingle(in)

	if !strings.HasPrefix(got, `'`) || !strings.HasSuffix(got, `'`) {
		t.Fatalf("shellQuoteSingle(%q) = %q, want it wrapped in single quotes", in, got)
	}
	// Everything from sh's point of view is inert inside single quotes
	// except a literal `'`, which got must have closed out, escaped, and
	// reopened around (`'"'"'`) rather than left as a bare `'`.
	inner := strings.TrimSuffix(strings.TrimPrefix(got, `'`), `'`)
	if strings.Contains(strings.ReplaceAll(inner, `'"'"'`, ""), `'`) {
		t.Errorf("shellQuoteSingle(%q) = %q, contains an unescaped single quote that would close the shell string early", in, got)
	}
}

func TestEscapeAppleScriptStringNeutralizesInjection(t *testing.T) {
	// A crafted Reason containing a double quote and a backslash must not
	// be able to break out of the AppleScript string literal it's
	// interpolated into.
	in := `already sent" & (do shell script "rm -rf ~") & "`
	got := escapeAppleScriptString(in)

	script := `display notification "` + got + `" with title "factoryd"`
	// The escaped string must contain no unescaped double quote other
	// than the two literal delimiters this test itself wraps around it —
	// i.e. every quote inside got must be preceded by a backslash.
	body := strings.TrimSuffix(strings.TrimPrefix(script, `display notification "`), `" with title "factoryd"`)
	for i := 0; i < len(body); i++ {
		if body[i] == '"' && (i == 0 || body[i-1] != '\\') {
			t.Fatalf("escaped string %q contains an unescaped double quote, breaks out of the AppleScript literal", got)
		}
	}
	if !strings.Contains(got, `\"`) {
		t.Errorf("escapeAppleScriptString(%q) = %q, want the double quote escaped", in, got)
	}
}

func TestEscapeAppleScriptStringEscapesBackslashBeforeQuote(t *testing.T) {
	// Backslash must be escaped first, or escaping the quote afterward
	// would double-escape the backslashes that step introduces.
	got := escapeAppleScriptString(`\"`)
	if got != `\\\"` {
		t.Errorf(`escapeAppleScriptString(\"") = %q, want \\\"`, got)
	}
}

var errNotFoundForTest = &lookPathTestError{}

type lookPathTestError struct{}

func (*lookPathTestError) Error() string { return "osascript: executable file not found in $PATH" }
