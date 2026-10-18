package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The token files beside the session config -- the stable start token, the
// override and read tokens, the MCP token and the gate token -- share these
// helpers, so every one of them is created, checked and refused the same way.

// tokenFileIssue names why an existing token file could not be trusted --
// distinct from simply being absent (the normal "nothing to reuse yet"
// case, which inspectTokenFile reports as no issue). Fix is the exact
// remedy, e.g. a literal `chmod` command, so a caller can print something
// actionable instead of silently going on with no token.
type tokenFileIssue struct {
	Path   string
	Reason string
	Fix    string
}

func (i *tokenFileIssue) Error() string {
	return fmt.Sprintf("%s %s -- %s", i.Path, i.Reason, i.Fix)
}

// inspectTokenFile Lstat-checks path -- never following a symlink, the same
// reasoning generateTokenFile's O_NOFOLLOW create uses -- and refuses to
// treat its content as a usable token unless every one of these holds: not
// a symlink, a regular file, owned by this process's own uid, and
// mode&0o077 == 0 (a token readable by another local account defeats the
// point of a bearer credential). Returns (token, nil, nil) only when every
// check passes AND the content is non-empty; ("", nil, nil) when the path
// does not exist (the ordinary "generate a fresh one" case); and
// ("", *tokenFileIssue, nil) for every other case, naming exactly what's
// wrong and how to fix it. A real stat/read error (a permission-denied
// parent directory, for instance) is returned as err instead.
func inspectTokenFile(path string) (token string, issue *tokenFileIssue, err error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", nil, nil
		}
		return "", nil, statErr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", &tokenFileIssue{Path: path, Reason: "is a symlink", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	if !info.Mode().IsRegular() {
		return "", &tokenFileIssue{Path: path, Reason: "is not a regular file", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", &tokenFileIssue{Path: path, Reason: fmt.Sprintf("has mode %#o (readable beyond its owner)", info.Mode().Perm()), Fix: fmt.Sprintf("chmod 600 %s", path)}, nil
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return "", &tokenFileIssue{Path: path, Reason: "is not owned by the current user", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return "", nil, readErr
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", &tokenFileIssue{Path: path, Reason: "is empty", Fix: "remove it and rerun -- a fresh token file will be generated"}, nil
	}
	return token, nil, nil
}

// readTokenFile is inspectTokenFile's ok-or-not wrapper, for a caller
// (quickstartEnsureServe, consoleMain) that only needs to know whether a
// usable token exists: an issue (bad permissions, a symlink, wrong owner)
// reports exactly like "does not exist", so the caller falls back to its
// next choice. A caller that should instead warn the operator with the
// concrete fix (serveMain) calls inspectTokenFile directly.
func readTokenFile(path string) (string, bool) {
	token, issue, err := inspectTokenFile(path)
	if err != nil || issue != nil || token == "" {
		return "", false
	}
	return token, true
}

// insideGitWorkTree reports whether dir is inside a git working
// tree (`git -C <dir> rev-parse --show-toplevel` succeeds) -- generating
// a token file there would risk it being swept into `git add
// -A`/committed/pushed alongside real repo content the next time an
// operator commits from that directory (an adversarial review). A
// boundary method so tests can stub the git invocation instead of depending
// on a real git binary and a real-or-absent repository.
func (impl realForge) insideGitWorkTree(dir string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Run() == nil
}

// generateTokenFile creates a fresh token (the crypto/rand
// generateStartToken every ephemeral per-process token uses) at path, mode
// 0600 (operator-owned config directory, readable only by this user -- see
// safety-contract.md's "Console loopback writes" row), and returns it.
// Refuses outright if path's own directory is inside a git working tree
// (forge.insideGitWorkTree) -- point -config somewhere outside any repo
// instead. Creates the file with O_EXCL|O_NOFOLLOW: O_EXCL means a
// concurrent creator (a second `install-service`/`quickstart` racing this
// one) fails closed instead of one silently clobbering the other's token,
// and O_NOFOLLOW refuses to write through a symlink some other local
// process may have planted at this exact path ahead of us. A plist never
// embeds the value (it is not a ProgramArguments argv entry or an
// EnvironmentVariables dict key, unlike -config/-data-dir): `serve` reads
// the file back at each of its own startups instead.
func generateTokenFile(dp *deps, path string) (string, error) {
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

// ensureTokenFile returns the token at path, reusing it unchanged if the
// file already exists and passes every inspectTokenFile check (a reinstall
// or `install-service -force` must never rotate a token an operator's
// browser or client may already hold), and generating a fresh one only if
// it does not exist at all. An existing file that fails inspection (bad
// permissions, a symlink, wrong owner, empty) is refused loudly -- returned
// as err naming the exact fix -- rather than silently generating a second,
// different token behind its back (which O_EXCL would refuse anyway) or
// silently trusting content that fails these checks.
func ensureTokenFile(dp *deps, path string) (token string, generated bool, err error) {
	existing, issue, err := inspectTokenFile(path)
	if err != nil {
		return "", false, err
	}
	if issue != nil {
		return "", false, issue
	}
	if existing != "" {
		return existing, false, nil
	}
	token, err = generateTokenFile(dp, path)
	return token, true, err
}
