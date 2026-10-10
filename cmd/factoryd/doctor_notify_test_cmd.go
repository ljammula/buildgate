package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"buildgate/internal/notify"
)

// doctorRegisterNotifyTestFlag adds -notify-test to an already-constructed
// doctor FlagSet, deliberately not threaded through newDoctorFlags' own
// long return tuple -- flag.FlagSet.Bool may be called any number of
// times on the same *flag.FlagSet as long as it happens before Parse, so
// this keeps the -notify-test flag's addition to a one-line call at
// doctorMain's own call site (cmd/factoryd/doctor.go) instead of a
// signature change there, which another concurrent change to that
// file's run-start preflight checks would otherwise conflict with.
func doctorRegisterNotifyTestFlag(flags *flag.FlagSet) *bool {
	return flags.Bool("notify-test", false, "send one real desktop test notification, report which mechanism delivered it (terminal-notifier or osascript) and the known silent-failure caveat, then exit -- does not run the rest of doctor's checks. Not part of a default `factoryd doctor` run since it pops real UI")
}

// doctorNotifyTestMain implements `factoryd doctor -notify-test`.
// It determines which mechanism internal/notify.DesktopNotifier.Notify
// will actually use -- mirroring that function's own documented
// precedence (terminal-notifier when present and there's a click target,
// else the osascript fallback, else nothing) -- fires one real
// notification through it, and prints the mechanism plus, when
// terminal-notifier was used, the known silent-failure caveat that
// package's own doc comment describes: an installed-but-unapproved
// terminal-notifier fails with no signal at all, indistinguishable from
// success. This command cannot detect delivery itself (Notifier's own
// contract never errors); it only reports what it attempted and asks the
// operator to confirm.
func doctorNotifyTestMain(dataDir string) error {
	mechanism := "none (no notifier binary on PATH)"
	switch {
	case runtime.GOOS != "darwin":
		mechanism = "none (not macOS -- desktop notifications are darwin-only)"
	default:
		if _, err := exec.LookPath("terminal-notifier"); err == nil {
			mechanism = "terminal-notifier"
		} else if _, err := exec.LookPath("osascript"); err == nil {
			mechanism = "osascript (not clickable)"
		}
	}

	delivered := true
	n := notify.Notification{
		Reason:    "factoryd doctor -notify-test: this is a real test notification",
		SentAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Delivered: &delivered,
		// RunDir gives terminal-notifier a click target, matching a real
		// halt/accept notification's own shape -- without one,
		// DesktopNotifier.Notify falls straight through to osascript even
		// when terminal-notifier is on PATH, which would make this
		// command's own mechanism determination above wrong.
		RunDir: dataDir,
	}
	if mechanism == "terminal-notifier" {
		// Sent here, not through DesktopNotifier, which falls back to
		// osascript without a word: this command exists to say which
		// mechanism works.
		out, err := exec.Command("terminal-notifier", "-title", "factoryd", "-message", n.Reason, "-execute", "open "+shellQuoteForNotifyTest(dataDir)).CombinedOutput()
		if err != nil {
			fmt.Printf("terminal-notifier could not show a notification: %s\n", sanitizeFirstLine(out, err))
			fmt.Println("fix: System Settings -> Notifications -> terminal-notifier -> Allow Notifications, alert style Banners or Alerts. Until then factoryd shows the plain osascript banner, whose click does nothing.")
			return nil
		}
		fmt.Println("sent a test desktop notification via: terminal-notifier (its click opens the data directory)")
		fmt.Println("confirm you actually saw a notification banner: macOS can also hide one that was accepted (Focus, alert style None).")
		return nil
	}
	if err := (notify.DesktopNotifier{}).Notify(context.Background(), n); err != nil {
		return fmt.Errorf("send test notification: %w", err)
	}

	fmt.Printf("sent a test desktop notification via: %s\n", mechanism)
	fmt.Println("confirm you actually saw a notification banner -- this command has no way to detect delivery itself.")
	return nil
}

// shellQuoteForNotifyTest single-quotes s for terminal-notifier's -execute,
// which runs its value through `sh -c`.
func shellQuoteForNotifyTest(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'"'"'`) + `'`
}
