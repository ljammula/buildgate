package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"buildgate/internal/sessionconfig"
)

// serveStableStartTokenFileName is the file `factoryd install-service`
// writes the serve LaunchAgent's stable start token into
// (<effective config dir>/serve-start-token, mode 0600), and the one
// `serveMain` reads from -- before ever generating a fresh per-process
// token -- when FACTORYD_API_START_TOKEN is unset. Without this, every
// `launchctl kickstart`/reboot-triggered restart of a supervised `serve`
// would mint a
// brand new generateStartToken() value, silently invalidating every
// browser's already-`captureStartTokenFromLocation`-stored token and
// 403ing New run/release/stats/daemon screens until the operator noticed
// the service had restarted and went hunting for a fresh console link in
// its log file.
const serveStableStartTokenFileName = "serve-start-token"

// resolveEffectiveConfigPath resolves the session config path used to
// locate the stable start-token file: explicit (a real -config flag
// value), made absolute, if given; otherwise the first of
// sessionconfig.DefaultPaths() that already exists on disk. Mirrors
// resolveServiceConfigPath's own algorithm (install_service.go) but never
// errors on "nothing found" -- a caller here (serveMain,
// quickstartEnsureServe, consoleMain, install-service) treats that as
// simply "no stable token file possible", falling back to an ephemeral
// per-process token, not a fatal condition the way install-service's own
// LaunchAgent setup must treat it.
//
// Every one of `serve` (-config), `factoryd quickstart` (its own already-
// resolved session config path), `factoryd console` (-config), and
// `factoryd install-service` (its own -config) now call this exact same
// function so they can never derive four different token-file locations
// for what is supposed to be one shared, stable secret (an adversarial
// review, 2026-09-24): before this fix `serve` had no -config
// flag at all and always used sessionconfig.LoadDefault()'s fixed
// default-path search, while install-service's plist could point -config
// at a non-default path -- a supervised serve LaunchAgent's own token
// file (keyed off that resolution) would then silently disagree with
// what a bare `factoryd serve`/`console` invocation (the default search)
// derived, each reading and writing a DIFFERENT token file with no error
// ever surfaced -- just a permanently 403ing console.
func resolveEffectiveConfigPath(explicit string) (path string, found bool) {
	if explicit != "" {
		abs, err := filepath.Abs(sessionconfig.ResolveArg(explicit))
		if err != nil {
			return "", false
		}
		return abs, true
	}
	p, found, err := sessionconfig.ResolvePath()
	if err != nil || !found {
		return "", false
	}
	return p, true
}

// serveStableStartTokenPathFor returns the stable start-token file path
// for configPath (as resolveEffectiveConfigPath resolved it) -- always
// the config's own directory, never a separate location a caller could
// derive differently.
func serveStableStartTokenPathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), serveStableStartTokenFileName)
}

// serveStartTokenFileIssue names why an existing stable start-token file
// could not be trusted/reused -- distinct from simply being absent (the
// normal "nothing to reuse yet" case, which inspectServeStartTokenFile
// reports as (nil, nil), not an issue). Fix is the exact remedy, e.g. a
// literal `chmod` command, so a caller can print something actionable
// instead of silently falling back to a fresh ephemeral token with no
// explanation.
type serveStartTokenFileIssue struct {
	Path   string
	Reason string
	Fix    string
}

func (i *serveStartTokenFileIssue) Error() string {
	return fmt.Sprintf("%s %s -- %s", i.Path, i.Reason, i.Fix)
}

// inspectServeStartTokenFile Lstat-checks path -- never following a
// symlink, the same "don't trust what a symlink at this path might
// actually point at" reasoning generateServeStartTokenFile's own
// O_NOFOLLOW create uses -- and refuses to treat its content as a usable
// token unless every one of these holds (an adversarial review):
// not a symlink, a regular file, owned by this process's own uid, and
// mode&0o077 == 0 (unreadable by group/other -- a token readable by
// another local account defeats the whole point of a start-class bearer
// credential). Returns (token, nil, nil) only when every check passes
// AND the file's own content is non-empty; (\"\", nil, nil) when the path
// simply does not exist (the ordinary "generate a fresh one" case); and
// (\"\", *serveStartTokenFileIssue, nil) for every other case, naming
// exactly what's wrong and how to fix it. A real stat/read error (a
// permission-denied parent directory, for instance) is returned as err
// instead of either of the above.
func inspectServeStartTokenFile(path string) (token string, issue *serveStartTokenFileIssue, err error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", nil, nil
		}
		return "", nil, statErr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", &serveStartTokenFileIssue{Path: path, Reason: "is a symlink", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	if !info.Mode().IsRegular() {
		return "", &serveStartTokenFileIssue{Path: path, Reason: "is not a regular file", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", &serveStartTokenFileIssue{Path: path, Reason: fmt.Sprintf("has mode %#o (readable beyond its owner)", info.Mode().Perm()), Fix: fmt.Sprintf("chmod 600 %s", path)}, nil
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return "", &serveStartTokenFileIssue{Path: path, Reason: "is not owned by the current user", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return "", nil, readErr
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", &serveStartTokenFileIssue{Path: path, Reason: "is empty", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	return token, nil, nil
}

// readServeStartTokenFile is inspectServeStartTokenFile's simple
// ok-or-not wrapper, for a caller (quickstartEnsureServe, consoleMain)
// that only needs to know whether a usable token exists -- an issue
// (bad permissions, a symlink, wrong owner) reports exactly like "does
// not exist" here, the same "fall back to the next choice" contract this
// function has always had; a caller that should instead warn the
// operator with the concrete fix (serveMain) calls
// inspectServeStartTokenFile directly.
func readServeStartTokenFile(path string) (string, bool) {
	token, issue, err := inspectServeStartTokenFile(path)
	if err != nil || issue != nil || token == "" {
		return "", false
	}
	return token, true
}

// insideGitWorkTree reports whether dir is inside a git working
// tree (`git -C <dir> rev-parse --show-toplevel` succeeds) -- generating
// the stable start token there would risk it being swept into `git add
// -A`/committed/pushed alongside real repo content the next time an
// operator commits from that directory (an adversarial review). A
// boundary method so tests can stub the git invocation instead of depending
// on a real git binary and a real-or-absent repository.
func (impl realForge) insideGitWorkTree(dir string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Run() == nil
}

// generateServeStartTokenFile creates a fresh stable start token (the
// same crypto/rand generateStartToken every ephemeral per-process token
// uses) at path, mode 0600 (operator-owned config directory, readable
// only by this user -- see safety-contract.md's "Console loopback
// writes" row), and returns it. Refuses outright if path's own directory
// is inside a git working tree (forge.insideGitWorkTree) -- point
// -config somewhere outside any repo instead. Creates the file with
// O_EXCL|O_NOFOLLOW (per that same adversarial review): O_EXCL means a
// concurrent creator (a second `install-service`/`quickstart` racing this
// one) fails closed instead of one silently clobbering the other's
// token, and O_NOFOLLOW refuses to write through a symlink some other
// local process may have planted at this exact path ahead of us. Used by
// `install-service` when installing the serve LaunchAgent: the plist
// itself never embeds this value (it is not a ProgramArguments argv
// entry or an EnvironmentVariables dict key, unlike -config/-data-dir) --
// `serveMain` reads this file back at each of its own startups instead.
func generateServeStartTokenFile(dp *deps, path string) (string, error) {
	dir := filepath.Dir(path)
	if dp.forge.insideGitWorkTree(dir) {
		return "", fmt.Errorf("refusing to write a token file inside a git working tree (%s) -- point -config at a session config outside any git repo", dir)
	}
	token, err := generateStartToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(token + "\n"); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return token, nil
}

// ensureServeStartTokenFile returns the stable start token at path,
// reusing it unchanged if the file already exists and passes every
// inspectServeStartTokenFile check (a reinstall or `install-service
// -force` must never rotate the token an operator's browser may already
// have stored -- see generateServeStartTokenFile's own doc comment), and
// generating a fresh one only if it does not exist at all. An existing
// file that fails inspection (bad permissions, a symlink, wrong owner,
// empty) is refused loudly -- returned as err naming the exact fix --
// rather than silently generating a second, different token behind its
// back (which O_EXCL would refuse anyway) or silently trusting content
// that fails these checks.
func ensureServeStartTokenFile(dp *deps, path string) (token string, generated bool, err error) {
	existing, issue, err := inspectServeStartTokenFile(path)
	if err != nil {
		return "", false, err
	}
	if issue != nil {
		return "", false, issue
	}
	if existing != "" {
		return existing, false, nil
	}
	token, err = generateServeStartTokenFile(dp, path)
	return token, true, err
}
