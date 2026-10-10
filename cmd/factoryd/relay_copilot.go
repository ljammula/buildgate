package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// gitHubCopilotTokenEnv is the raw-token fallback source for
// -relay-credential-mode=github-copilot, consulted only when
// -relay-github-token-file is unset -- see resolveGitHubCopilotToken.
const gitHubCopilotTokenEnv = "GITHUB_COPILOT_TOKEN"

// defaultGitHubCopilotTokenKey is -relay-github-token-key's own default:
// the provider id pi itself registers its GitHub Copilot OAuth flow under
// (see github-copilot.ts's own githubCopilotProvider, id: "github-copilot").
// A pi fork that renamed the provider
// needs this flag set to whatever key its own auth.json actually uses.
const defaultGitHubCopilotTokenKey = "github-copilot"

// piAuthFileCredential is the shape of one entry in a pi (or pi fork) auth.json
// (badlogic/pi-mono, commit 2176b9dd8f0020bfb383bcf074f78a5f30efc29e,
// packages/ai/src/auth/types.ts's OAuthCredential): auth.json itself is a
// JSON object keyed by provider id ("github-copilot" for this one), one
// entry per configured provider. Only the two fields this package needs
// are modeled; every other field (availableModelIds, enterpriseUrl, ...)
// is ignored.
type piAuthFileCredential struct {
	Type string `json:"type"`
	// Refresh holds the long-lived GitHub OAuth token pi's own device-code
	// login produced -- confusingly named (it is not a classic OAuth
	// refresh token, and never itself expires the way one does), but it is
	// exactly what pi's own refreshGitHubCopilotAccessToken sends to mint a
	// new Copilot API token, which is exactly what this relay needs to do
	// too.
	Refresh string `json:"refresh"`
	// Access holds pi's own short-lived, already-cached Copilot API token
	// -- never used by this relay, which performs its own exchange (and
	// its own refresh-before-expiry) against Refresh instead, so a stale
	// cached Access value here never matters.
	Access string `json:"access"`
}

// resolveGitHubCopilotToken resolves the GitHub OAuth token for
// -relay-credential-mode=github-copilot. Explicit sources win first; when
// neither is supplied, Pi's host-only auth location is checked
// (a Pi fork's own auth.json is named with the route's github_token_file). The wrapper keeps the historic token-only API for callers that
// do not need audit metadata.
func resolveGitHubCopilotToken(tokenFile, key string) (string, error) {
	token, _, err := resolveGitHubCopilotTokenSource(tokenFile, key)
	return token, err
}

// resolveGitHubCopilotTokenSource is the only function that performs
// automatic auth-file discovery. It runs in the host process; callers pass
// only the returned credential to the relay and may retain source as
// redacted audit metadata.
func resolveGitHubCopilotTokenSource(tokenFile, key string) (string, string, error) {
	if tokenFile != "" {
		path, err := expandAuthPath(tokenFile)
		if err != nil {
			return "", "", fmt.Errorf("expand route github_token_file: %w", err)
		}
		token, err := parseGitHubCopilotAuthFile(path, key, "route github_token_file")
		return token, path, err
	}
	if token := os.Getenv(gitHubCopilotTokenEnv); token != "" {
		return token, "env:" + gitHubCopilotTokenEnv, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home directory for GitHub Copilot auth discovery: %w", err)
	}
	paths := githubCopilotAuthPaths(home)
	var checked []string
	for _, path := range paths {
		checked = append(checked, path)
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return "", "", fmt.Errorf("stat discovered GitHub Copilot auth file %q: %w", path, statErr)
		}
		if info.IsDir() {
			return "", "", fmt.Errorf("discovered GitHub Copilot auth path %q is a directory; checked %s; authenticate with `pi` login, or set %s / the route's github_token_file", path, strings.Join(checked, ", "), gitHubCopilotTokenEnv)
		}
		token, parseErr := parseGitHubCopilotAuthFile(path, key, "discovered auth file")
		if parseErr != nil {
			return "", path, fmt.Errorf("%w; checked %s; authenticate with `pi` login, or set %s / the route's github_token_file", parseErr, strings.Join(checked, ", "), gitHubCopilotTokenEnv)
		}
		return token, path, nil
	}
	return "", "", fmt.Errorf("no GitHub OAuth token configured; checked %s; authenticate with `pi` login, or set %s / the route's github_token_file", strings.Join(checked, ", "), gitHubCopilotTokenEnv)
}

func githubCopilotAuthPaths(home string) []string {
	return []string{
		filepath.Join(home, ".pi", "agent", "auth.json"),
	}
}

func expandAuthPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

// copilotAuthFilePermissionWarning returns a human-readable warning (or
// "" when nothing's wrong) for path's own info, checking the two
// conditions inspectServeStartTokenFile's own doc comment names for a
// bearer-credential file: readable beyond its owner (mode&0o077 != 0) or
// not owned by the current user. A round-2 review found quickstart now
// persists this exact path into relay_github_token_file, so a
// loosely-permissioned auth.json becomes a standing config-referenced
// risk rather than a one-off env var -- worth a warning here. Advisory
// only, unlike inspectServeStartTokenFile's own fail-closed check on a
// bearer-token file THIS process creates and fully controls: auth.json
// belongs to pi or its fork, not this process, so a permission bit alone
// must never block credential resolution.
func copilotAuthFilePermissionWarning(path string, info os.FileInfo) string {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("warning: %s has mode %#o (readable beyond its owner) -- consider chmod 600 %s", path, info.Mode().Perm(), path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Sprintf("warning: %s is not owned by the current user", path)
	}
	return ""
}

func parseGitHubCopilotAuthFile(path, key, source string) (string, error) {
	if key == "" {
		key = defaultGitHubCopilotTokenKey
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s %q: %w", source, path, err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		if warning := copilotAuthFilePermissionWarning(path, info); warning != "" {
			fmt.Fprintln(os.Stderr, warning)
		}
	}
	var auth map[string]piAuthFileCredential
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", fmt.Errorf("parse %s %q as pi's auth.json: %w", source, path, err)
	}
	entry, ok := auth[key]
	if !ok {
		// Lists the keys actually present, never their values (an entry's
		// own Refresh/Access are real credentials) -- found via review: a
		// pi fork that registers its Copilot provider under
		// a different id than "github-copilot" previously failed with only
		// "has no ... entry", giving the operator no way to tell a wrong
		// -relay-github-token-key apart from a genuinely empty auth.json.
		present := make([]string, 0, len(auth))
		for k := range auth {
			present = append(present, k)
		}
		sort.Strings(present)
		return "", fmt.Errorf("%s %q has no %q entry (present: %s) -- set -relay-github-token-key if your pi fork registers this provider under a different id", source, path, key, strings.Join(present, ", "))
	}
	if entry.Type != "oauth" {
		return "", fmt.Errorf("%s %q's %q entry is not an oauth credential (type %q)", source, path, key, entry.Type)
	}
	if entry.Refresh == "" {
		return "", fmt.Errorf("%s %q's %q entry has no GitHub OAuth token (its \"refresh\" field)", source, path, key)
	}
	return entry.Refresh, nil
}
