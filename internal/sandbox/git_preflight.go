package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// This file implements containment-matrix.md's Credentials row, closing
// a gap a 2026-09-15 review found: the sandboxed worker's Git common
// directory is bind-mounted read-only (see
// docker.go's own mount code), but a mount being read-only only stops the
// worker from *writing* it -- the worker can still read it, and its git
// config is exactly the file a stolen credential would live in (an
// embedded-token remote URL, a credential.helper, or an
// http.extraheader-based bearer token injector). A worker that can read
// such a config can exfiltrate whatever it reads through the model relay
// (the worker's only route out). PreflightGitCredentials closes this
// fail-closed, before the mount is ever built: refuse the run rather than
// mount a config carrying one of those three shapes.

// credentialHelperKeyPattern matches "credential.helper" or a URL-scoped
// "credential.<url>.helper" key, exactly as `git config --list` prints keys
// (git always lowercases the section and the trailing key name; only a
// subsection -- here, a URL -- keeps its original case, hence the
// case-sensitive middle segment and case-insensitive anchors).
var credentialHelperKeyPattern = regexp.MustCompile(`(?i)^credential\.(?:.+\.)?helper$`)

// extraHeaderKeyPattern matches "http.extraheader" or a URL-scoped
// "http.<url>.extraheader" key -- git's own mechanism (used legitimately by
// some credential managers) for attaching a fixed header, commonly
// `Authorization: Bearer <token>`, to every request for a URL prefix.
var extraHeaderKeyPattern = regexp.MustCompile(`(?i)^http\.(?:.+\.)?extraheader$`)

// userinfoURLPattern matches a URL value embedding userinfo credentials:
// "https://user:token@host/..." (RFC 3986 userinfo) or the bare-token form
// git also accepts, "https://token@host/...". Checked against every
// config value, not just url-shaped keys (remote.<name>.url,
// url.<base>.insteadOf, http.proxy, and others can all carry a URL).
var userinfoURLPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/@\s]+@`)

// PreflightGitCredentials fails closed when the git config a sandboxed
// worker's Git common-directory mount would expose for the repository at
// workDir declares a credential-helper, an http.extraheader, or a URL with
// embedded userinfo credentials. It reads both the common directory's own
// `config` file and, for a linked worktree with the worktreeConfig
// extension enabled, that worktree's own `config.worktree` -- the same two
// files docker.go's Run mounts read-only (directly, for a plain checkout;
// via the common-dir mount plus the worktree's own `.git` pointer file,
// for a linked worktree). Parsing goes through host-side `git config
// --file <path> --list` rather than a hand-rolled INI reader or regex over
// raw file bytes, so this check agrees with what git itself will resolve
// -- and never executes anything from workDir's own tree. A workDir with
// no `.git` at all (already caught elsewhere by LaunchSpec.Validate for a
// real run) is not this function's concern and returns nil.
func PreflightGitCredentials(workDir string) error {
	commonConfig, worktreeConfig, err := gitConfigPaths(workDir)
	if err != nil {
		return fmt.Errorf("git credential preflight: resolve git config path: %w", err)
	}
	for _, path := range []string{commonConfig, worktreeConfig} {
		if path == "" {
			continue
		}
		if err := checkGitConfigFileForCredentials(path); err != nil {
			return err
		}
	}
	return nil
}

// gitConfigPaths returns the common-directory `config` path and, if this
// workDir is a linked worktree, its own `config.worktree` path (empty if
// workDir has no `.git` at all, or is a plain checkout -- a plain
// checkout's own worktree config, if extensions.worktreeConfig is set,
// already lives at <gitDir>/config.worktree == <workDir>/.git/config.worktree,
// which the first return covers by itself: worktreeConfig is only ever a
// distinct path from commonConfig for a linked worktree).
func gitConfigPaths(workDir string) (commonConfig string, worktreeConfig string, err error) {
	entry, statErr := os.Lstat(filepath.Join(workDir, ".git"))
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", "", nil
		}
		return "", "", statErr
	}
	switch {
	case entry.IsDir():
		gitDir := filepath.Join(workDir, ".git")
		return filepath.Join(gitDir, "config"), filepath.Join(gitDir, "config.worktree"), nil
	case entry.Mode().IsRegular():
		commonDir, _, cerr := gitCommonDir(workDir)
		if cerr != nil {
			return "", "", cerr
		}
		out, gerr := exec.Command("git", "-C", workDir, "rev-parse", "--git-dir").Output()
		if gerr != nil {
			return "", "", gerr
		}
		gitDir := strings.TrimSpace(string(out))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(workDir, gitDir)
		}
		gitDir, aerr := filepath.Abs(gitDir)
		if aerr != nil {
			return "", "", aerr
		}
		return filepath.Join(commonDir, "config"), filepath.Join(gitDir, "config.worktree"), nil
	default:
		return "", "", fmt.Errorf(".git entry at %s is neither a directory nor a regular worktree file", workDir)
	}
}

// checkGitConfigFileForCredentials runs `git config --file <path> --list`
// against one config file (skipped entirely if it doesn't exist -- a
// worktree with no config.worktree, or a fresh repo with no config yet,
// are not errors) and refuses on the first forbidden key or value it
// finds. The refusal names the offending key and the fix; it never
// includes any config value, so a leaked-looking value is never echoed
// back into a log or evidence record by this check itself.
func checkGitConfigFileForCredentials(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("git credential preflight: stat %s: %w", path, err)
	}
	// --includes: an include.path/includeIf.*.path can point at another
	// file inside the mounted tree that holds the credential; reading it
	// here is still host-side parsing, never executing repo content
	// (lead review, 2026-09-24).
	out, err := exec.Command("git", "config", "--file", path, "--includes", "--list").Output()
	if err != nil {
		return fmt.Errorf("git credential preflight: read %s: %w", path, err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, found := strings.Cut(line, "=")
		if !found {
			key, value = line, ""
		}
		switch {
		case credentialHelperKeyPattern.MatchString(key):
			return fmt.Errorf("git credential preflight: %s sets %q, a credential helper a sandboxed worker could invoke to obtain live credentials -- remove it from the repo's git config (keep credential helpers in the operator's own global/system config, outside the mounted tree)", path, key)
		case extraHeaderKeyPattern.MatchString(key):
			return fmt.Errorf("git credential preflight: %s sets %q, a fixed HTTP header (commonly a bearer token) a sandboxed worker could read and exfiltrate -- remove it from the repo's git config", path, key)
		case userinfoURLPattern.MatchString(key):
			// url.<base>.insteadOf / url.<base>.pushInsteadOf carry the URL
			// in the key itself (lead review, 2026-09-24). Name the key's
			// section only: the full key would print the credential.
			section, _, _ := strings.Cut(key, ".")
			return fmt.Errorf("git credential preflight: %s has a %q entry whose URL embeds credentials a sandboxed worker could read and exfiltrate -- rewrite it without userinfo", path, section)
		case userinfoURLPattern.MatchString(value):
			return fmt.Errorf("git credential preflight: %s sets %q to a URL with embedded credentials a sandboxed worker could read and exfiltrate -- rewrite the remote URL without userinfo and authenticate via a credential helper configured outside the mounted tree instead", path, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("git credential preflight: scan %s: %w", path, err)
	}
	return nil
}
