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
	"time"
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
// machine's real PATH.
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

// DesktopNotifier shows a single macOS desktop notification. It is a
// best-effort, out-of-band channel alongside LogNotifier (the durable audit
// trail), and per the Notifier contract it never errors: every failure mode
// (opted out, wrong OS, no notifier binary on PATH, the command itself
// failing) is a silent no-op.
//
// terminal-notifier, when it is on PATH and the notification has somewhere
// to send a click, gives the banner its one action: `open <n.Link>` (the
// console page of the request or run), or `open <n.RunDir>` (Finder) when no
// console is known. It is its own application to macOS and shows nothing
// until allowed in System Settings -> Notifications; it exits non-zero then,
// and the osascript banner is shown instead.
//
// osascript's `display notification` has no click action at all: clicking it
// foregrounds Script Editor. Its text is therefore the whole message.
//
// `make install` installs terminal-notifier with Homebrew when it is
// missing, and `factoryd doctor` says when a click would do nothing.
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

	title, subtitle, message := desktopText(n)

	// target is what a click opens: the console page when one is known
	// (Link), else the run's own directory. A notification with neither
	// goes straight to osascript: terminal-notifier would show a banner
	// whose click does nothing.
	target := n.Link
	if target == "" {
		target = n.RunDir
	}
	if target != "" {
		if _, err := showClickable(ctx, n, target); err == nil {
			return nil
		}
	}

	path, err := desktopNotifierLookPath()
	if err != nil {
		return nil
	}

	// This banner has no click action: a request's says where to go.
	if n.Ask != "" {
		message += " Open the console to act on it (`factoryd console`)."
	}
	// Split across subtitle and body rather than one flattened line:
	// macOS truncates a long single-line banner, and message is where
	// the operator's actual next step lives.
	script := fmt.Sprintf(`display notification "%s" with title "%s" subtitle "%s"`,
		escapeAppleScriptString(message), escapeAppleScriptString(title), escapeAppleScriptString(subtitle))
	_ = exec.CommandContext(ctx, path, "-e", script).Run()
	return nil
}

// clickableTimeout bounds terminal-notifier, so that one that hangs leaves
// the osascript banner time to be shown within the caller's own deadline.
const clickableTimeout = 3 * time.Second

// showClickable shows n through terminal-notifier with target as what its
// click opens, and returns terminal-notifier's output and error: not on
// PATH, or a non-zero exit, which is how it reports that macOS does not
// allow its notifications.
func showClickable(ctx context.Context, n Notification, target string) ([]byte, error) {
	clickPath, err := desktopClickNotifierLookPath()
	if err != nil {
		return nil, err
	}
	title, subtitle, message := desktopText(n)
	// terminal-notifier itself runs -execute's value through `sh -c`, so
	// target -- an operator-configured -data-dir joined with a run/ticket
	// ID, or a console URL -- must be shell-quoted here, not just passed as
	// a literal argv element the way the text arguments are.
	args := []string{
		"-title", terminalNotifierText(title),
		"-subtitle", terminalNotifierText(subtitle),
		"-message", terminalNotifierText(message),
		"-execute", "open " + shellQuoteSingle(target),
	}
	if n.RequestID != "" {
		// One banner per request: a later one replaces the earlier.
		args = append(args, "-group", "buildgate-"+n.RequestID)
	}
	if icon := desktopIconPath(); icon != "" {
		args = append(args, "-contentImage", icon)
	}
	ctx, cancel := context.WithTimeout(ctx, clickableTimeout)
	defer cancel()
	return exec.CommandContext(ctx, clickPath, args...).CombinedOutput()
}

// ShowClickableTest sends one notification through terminal-notifier alone,
// with dir as what its click opens, and returns terminal-notifier's own
// output and error. `factoryd doctor -notify-test` uses it to say whether
// macOS allows terminal-notifier's banners, which DesktopNotifier.Notify,
// falling back to osascript without a word, cannot.
func ShowClickableTest(ctx context.Context, message, dir string) ([]byte, error) {
	return showClickable(ctx, Notification{Reason: message}, dir)
}

// terminalNotifierText makes s safe as the value of a terminal-notifier text
// option: one that begins with "-" would be read as an option, and
// terminal-notifier asks for a leading bracket to be escaped with a
// backslash.
func terminalNotifierText(s string) string {
	if s != "" && strings.ContainsRune("-[({<", rune(s[0])) {
		return `\` + s
	}
	return s
}

// desktopText is a banner's three lines. A request's notification leads
// with what is asked and names the request as the console does; its click,
// not its text, is how the operator gets there, so it carries no command.
// A run's own notification states the reason and, when it has one, the one
// action to take.
func desktopText(n Notification) (title, subtitle, message string) {
	if n.Ask != "" {
		return n.Ask, n.Subject, n.Reason
	}
	message = n.Reason
	if n.Next != "" {
		message = fmt.Sprintf("%s. Next: %s", n.Reason, n.Next)
	}
	return "factoryd", subject(n), message
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
