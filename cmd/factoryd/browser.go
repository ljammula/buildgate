package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"buildgate/internal/sessionconfig"
)

// browserCommand builds the OS-appropriate "open a file/URL in the
// default browser" command -- `open` on macOS, `xdg-open` elsewhere
// (Linux's own convention; there is no Windows factoryd build target
// today). a boundary method, like every other external-process seam in this
// package (e.g. host.launchctl, host.spawnWorker), so
// `factoryd console -open` and `factoryd quickstart` (unless -no-open)
// can be tested without actually launching a real browser.
func (impl realHost) browserCommand(target string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", target)
	}
	return exec.Command("xdg-open", target)
}

// browserRedirectDir is where openInBrowser writes its redirect file ("" is
// the OS temp directory), or why no browser can be opened here. On macOS it
// is the per-user temp directory. On Linux it is the data root under
// $HOME, never /tmp, which a snap browser (Ubuntu's default Firefox and
// Chromium) sees only as its own private copy, nor a hidden directory,
// which a snap cannot read either; and with no graphical session (an ssh
// login, a server) there is no browser to open: the link is printed
// instead.
func (impl realHost) browserRedirectDir() (string, error) {
	if runtime.GOOS == "darwin" {
		return "", nil
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return "", errors.New("no graphical session here (DISPLAY and WAYLAND_DISPLAY are unset): open the link above in a browser; from another machine, forward the port first (ssh -L 8090:127.0.0.1:8090 <this host>)")
	}
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return "", errors.New("xdg-open is not installed: open the link above in a browser")
	}
	dir := sessionconfig.DataRoot()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s for the browser redirect file: %w", dir, err)
	}
	return dir, nil
}

// openInBrowserCleanupDelay bounds how long the redirect file
// writeBrowserRedirectFile creates stays on disk before openInBrowser
// removes it -- long enough for even a slow-to-launch browser to have
// actually read and navigated away from it, short enough that it doesn't
// linger as a forgotten, still-token-bearing file. A package var so tests
// don't have to wait a real 10 seconds.
var openInBrowserCleanupDelay = 10 * time.Second

// openInBrowser starts this OS's default-browser opener, not on rawURL
// directly, but on a local HTML redirect file that itself navigates to
// rawURL via location.replace() -- an adversarial review, 2026-09-24, found
// that `open`/`xdg-open <url>` puts the complete URL, start token
// fragment included, on this process's own argv, which any local user can
// read for as long as the (short-lived, but real) open/xdg-open process
// exists via `ps eww <pid>` -- a second, unnecessary exposure of a
// permanent start-class credential beyond its three documented homes
// (safety-contract.md's "Console loopback writes" row: serve's own log,
// the URL fragment, this browser's own storage). Routing through a local
// file means only a filesystem path -- never the token -- ever appears in
// this process's argv or any process list.
func openInBrowser(dp *deps, rawURL string) error {
	dir, err := dp.host.browserRedirectDir()
	if err != nil {
		return err
	}
	path, cleanup, err := writeBrowserRedirectFile(dir, rawURL)
	if err != nil {
		return err
	}
	startErr := dp.host.browserCommand(path).Start()
	time.AfterFunc(openInBrowserCleanupDelay, cleanup)
	return startErr
}

// writeBrowserRedirectFile creates a 0700 temp directory under parent ("" is
// the OS temp directory) holding a single
// 0600 HTML file (per the adversarial review above) whose entire content is a
// `location.replace(<rawURL>)` redirect, and returns that file's path
// plus a cleanup closure that removes the whole directory. rawURL is
// encoded via encoding/json (a JSON string literal is also a safe,
// correctly-escaped JavaScript string literal -- no separate escaping
// scheme to get wrong) so it can never break out of the script's own
// string context regardless of what characters it contains.
func writeBrowserRedirectFile(parent, rawURL string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp(parent, "factoryd-open-*")
	if err != nil {
		return "", nil, fmt.Errorf("create browser redirect dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("chmod browser redirect dir: %w", err)
	}
	encodedURL, err := json.Marshal(rawURL)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("encode redirect URL: %w", err)
	}
	html := fmt.Sprintf("<!doctype html><meta charset=utf-8><title>Opening…</title><script>location.replace(%s)</script>", encodedURL)
	filePath := filepath.Join(dir, "open.html")
	if err := os.WriteFile(filePath, []byte(html), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("write browser redirect file: %w", err)
	}
	return filePath, func() { _ = os.RemoveAll(dir) }, nil
}
