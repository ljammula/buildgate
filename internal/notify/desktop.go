package notify

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DesktopNotificationsEnvironmentVariable names the environment variable
// that opts a machine out of local desktop notifications: set to "0",
// nothing is ever shown. Unset, or any other value, leaves them on --
// the default, since the engineer this channel exists for (no
// terminal open on any run) should not have to discover an opt-in
// first. Test suites set it to "0" in their TestMain
// so a `go test` on a Mac never fires a real notification.
const DesktopNotificationsEnvironmentVariable = "FACTORYD_DESKTOP_NOTIFICATIONS"

// desktopOS holds the running OS, as runtime.GOOS -- a package-level var
// rather than a direct runtime.GOOS reference so a test can fake a
// non-darwin OS without needing to actually run on one.
var desktopOS = runtime.GOOS

// desktopNotifierLookPath resolves the osascript binary -- a package-level
// var rather than a direct exec.LookPath("osascript") call so a test can
// simulate osascript being absent from PATH without needing to actually
// remove it.
var desktopNotifierLookPath = func() (string, error) {
	return exec.LookPath("osascript")
}

// desktopClickNotifierLookPath resolves the optional terminal-notifier
// binary -- a package-level var, mirroring desktopNotifierLookPath, so a
// test can simulate it being present or absent without touching the
// machine's real PATH. install.sh best-effort installs it (never fatal
// if Homebrew or the install itself is unavailable) but factoryd doesn't
// require it -- this lookup, not install.sh, is the actual gate; see
// DesktopNotifier's own doc comment for why it stays best-effort rather
// than a hard dependency.
var desktopClickNotifierLookPath = func() (string, error) {
	return exec.LookPath("terminal-notifier")
}

// buildgateIcon is the Buildgate mark (rendered from
// assets/brand/buildgate.svg by scripts/render-icons.sh), attached to
// terminal-notifier banners as -contentImage. That is the only icon slot
// left: terminal-notifier's -appIcon/-sender no longer work on current
// macOS, so the banner's own app icon stays terminal-notifier's, and
// osascript's display notification has no image option at all.
//
//go:embed buildgate-icon.png
var buildgateIcon []byte

// desktopIconCacheDir resolves the per-user cache directory the icon is
// written under -- a package-level var so tests write into a temp dir
// instead of the real ~/Library/Caches.
var desktopIconCacheDir = os.UserCacheDir

// desktopIconPath returns a local file holding buildgateIcon, writing it
// first when it's missing or stale (a newer binary with a redrawn icon),
// since terminal-notifier's -contentImage takes local paths only. The
// write goes through a temp file + rename so a concurrent factoryd
// process never hands terminal-notifier a half-written image. Any
// failure returns "" and the banner simply goes out without an image.
func desktopIconPath() string {
	base, err := desktopIconCacheDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(base, "buildgate")
	path := filepath.Join(dir, "notification-icon.png")
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, buildgateIcon) {
		return path
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	tmp, err := os.CreateTemp(dir, "notification-icon-*.png")
	if err != nil {
		return ""
	}
	_, writeErr := tmp.Write(buildgateIcon)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
		return ""
	}
	return path
}

// DesktopNotifier shows a single macOS desktop notification, via
// terminal-notifier when it's on PATH and the run has a directory to
// point at, falling back to plain osascript otherwise. It is a
// best-effort, out-of-band channel alongside LogNotifier — the durable
// audit trail — not a replacement for it, and per the Notifier contract
// it never errors: every failure mode (opted out, wrong OS, no notifier
// binary on PATH, the command itself failing) is a silent no-op.
//
// The osascript fallback is not clickable, by design decision rather
// than oversight (raised 2026-09-11: "clicking the notification takes me
// nowhere" — clicking it foregrounded a blank Script Editor window,
// which is macOS's fallback "activate the sender" behavior for a
// notification attributed to bare osascript, since display notification
// has no click-action primitive at all for anything to attach). Fixed
// there by making the run directory path -- already in n.Reason -- land
// in a readable subtitle/body split instead of one truncated line.
//
// terminal-notifier is the click-action path: `open <n.Link>` (the
// console's own deep link to the run or request, when a console base URL
// is configured -- see cmd/factoryd's resolveConsoleBaseURL/
// consoleRunURL and internal/notify's own consoleRunLink), so the click
// lands the operator on the run/request itself, opened in a browser.
// Falls back to `open <n.RunDir>` (Finder) when no console URL is known,
// so a click always reveals something rather than nothing.
//
// install.sh best-effort installs terminal-notifier via Homebrew (never
// fatal if Homebrew or the brew install itself is unavailable — see its
// own comment there), but it is still not a hard requirement of
// factoryd, and deliberately not added to cmd/factoryd/doctor.go's
// checks (unlike dockerBinary, which is load-bearing): this channel's
// own reason for existing ("no terminal open on any run") is a wake-up
// tap for an engineer who may not even be at the machine — click
// behavior is moot for that reader, and
// Discord/Slack (see halt.go's DispatchDiscord/DispatchSlack) are the
// channels that actually reach them regardless. So doctor.go has
// nothing to say about it either way; this lookup is the only gate.
//
// One trade worth knowing before relying on it: unlike osascript (which
// rides on an already-trusted system helper), terminal-notifier is its
// own bundle id and needs separate approval in System Settings →
// Notifications the first time it fires; until approved it fails with
// no signal at all — worse than "click does nothing" for a channel
// whose contract is "never errors" (silent failure here is
// indistinguishable from "opted out" or "no halts happened"). install.sh
// prints a one-time reminder about this when it installs the binary.
type DesktopNotifier struct{}

// Notify shows a desktop notification for n, or silently does nothing
// when desktop notifications are opted out of, the OS isn't darwin, or
// neither notifier binary is on PATH.
func (DesktopNotifier) Notify(ctx context.Context, n Notification) error {
	if os.Getenv(DesktopNotificationsEnvironmentVariable) == "0" {
		return nil
	}
	if desktopOS != "darwin" {
		return nil
	}

	subtitle := subject(n)

	// message states the reason and, when the notification carries a
	// Next, the one action the operator should take -- so the banner
	// itself is the answer, not just a pointer at a directory to go dig
	// through.
	message := n.Reason
	if n.Next != "" {
		message = fmt.Sprintf("%s. Next: %s", n.Reason, n.Next)
	}

	// target is what a click opens: the console page when one is known
	// (Link), else the run's own directory -- preferring the console
	// since it lands the operator on the request/run itself rather than
	// Finder. Preferred notifier: terminal-notifier, so the banner is
	// clickable at all; but only when there's actually somewhere to
	// send the click -- a notification predating RunDir/Link (see
	// run.NotificationRecord) falls straight through to the osascript
	// path below instead of firing a dead click target.
	target := n.Link
	if target == "" {
		target = n.RunDir
	}
	if target != "" {
		if clickPath, err := desktopClickNotifierLookPath(); err == nil {
			// terminal-notifier itself runs -execute's value through
			// `sh -c`, so target -- an operator-configured -data-dir
			// joined with a run/ticket ID, or a console URL -- must be
			// shell-quoted here, not just passed as a literal argv
			// element the way -title/-subtitle/-message safely are.
			args := []string{
				"-title", "factoryd",
				"-subtitle", subtitle,
				"-message", message,
				"-execute", "open " + shellQuoteSingle(target),
			}
			if icon := desktopIconPath(); icon != "" {
				args = append(args, "-contentImage", icon)
			}
			_ = exec.CommandContext(ctx, clickPath, args...).Run()
			return nil
		}
	}

	path, err := desktopNotifierLookPath()
	if err != nil {
		return nil
	}

	// Split across subtitle and body rather than one flattened line:
	// macOS truncates a long single-line banner, and message is where
	// the operator's actual next step lives -- but a single line
	// "run RUN_ID (TICKET) -> STATE: <message>" buried that path past
	// the point macOS clips it.
	script := fmt.Sprintf(`display notification "%s" with title "factoryd" subtitle "%s"`,
		escapeAppleScriptString(message), escapeAppleScriptString(subtitle))
	_ = exec.CommandContext(ctx, path, "-e", script).Run()
	return nil
}

// escapeAppleScriptString escapes s for safe interpolation inside a
// double-quoted AppleScript string literal. Backslash must be escaped
// first, or escaping the double quote afterward would double-escape the
// backslashes it just introduced.
func escapeAppleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// shellQuoteSingle wraps s in single quotes for safe interpolation into a
// command string that will itself be run through `sh -c` (as
// terminal-notifier's -execute value is) -- s is otherwise unescaped, so
// without this an embedded `'` could break out of the quoting and a `$`
// or backtick could trigger shell expansion. A literal single quote in s
// closes the quoting, appends an escaped quote, then reopens it --
// there's no escape sequence for a quote inside single quotes.
func shellQuoteSingle(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'"'"'`) + `'`
}
