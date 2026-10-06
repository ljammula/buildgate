package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ghAuthRunner executes name with args and returns its combined output
// plus error -- injected so doctorCheckGHAuth (and quickstart's own
// preflight call to it) can be exercised in tests against a fake
// `git`/`gh` without either being installed, reachable, or authenticated.
// Its signature mirrors exec.CommandContext's own shape so a test double
// is a one-line closure rather than a fake binary on $PATH.
type ghAuthRunner func(ctx context.Context, name string, args ...string) (output string, err error)

// execGHAuthRunner is the production ghAuthRunner: shells out via
// os/exec with no Env override, so it inherits this process' own
// environment -- deliberately the same thing a real `gh pr create`/
// `gh pr ready` call does (internal/forge.GHPullRequestOpener,
// internal/requestdriver/pr_review_driver.go's forge.markPullRequestReady both just
// call exec.CommandContext with no Env override either). A pass here
// means gh will actually be authenticated when factoryd itself later
// shells out to it -- an adversarial review found a live walk's own auth
// gap came from a config-only env override (XDG_CONFIG_HOME) that hid gh's
// real login from a check scoped to a different environment; inheriting
// the caller's real env here, rather than resolving gh's config
// ourselves, checks exactly what worker/serve's children will see.
func execGHAuthRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// doctorRepoGitHubHost resolves workspaceDir's own "origin" remote to the
// host `gh auth status -h <host>` should be checked against, and whether
// that remote is SSH-shaped (isSSH), or ("", false) when there is
// nothing to check -- no git repo, no "origin" remote, or a remote URL
// this can't parse a host out of. Never itself an error: the gh-auth
// check is skipped, not failed, for a workspace with no GitHub remote at
// all (the same "nothing to check against" convention every other
// doctor check with an optional input already follows).
func doctorRepoGitHubHost(ctx context.Context, run ghAuthRunner, workspaceDir string) (host string, isSSH bool) {
	out, err := run(ctx, "git", "-C", workspaceDir, "remote", "get-url", "origin")
	if err != nil {
		return "", false
	}
	remote := strings.TrimSpace(out)
	// isSSH: an SSH config Host alias (round 2 of the
	// adversarial review) only ever applies to a remote that actually goes through the SSH
	// client -- both the "git@host:path" shorthand and an explicit
	// "ssh://" scheme resolve ~/.ssh/config Host aliases; an https://
	// remote's host is always a real, browser-resolvable DNS name, never
	// an alias, so ghHostToCheck's own alias remap must never apply to
	// one.
	isSSH = strings.HasPrefix(remote, "git@") || strings.HasPrefix(remote, "ssh://")
	return parseGitRemoteHost(remote), isSSH
}

// parseGitRemoteHost extracts the host from a git remote URL, handling
// both the SSH shorthand (git@host:owner/repo.git) and any URL scheme
// (https://host/owner/repo.git, ssh://git@host/owner/repo.git).
func parseGitRemoteHost(remote string) string {
	if rest, ok := strings.CutPrefix(remote, "git@"); ok {
		if i := strings.IndexAny(rest, ":/"); i > 0 {
			return rest[:i]
		}
		return ""
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// ghConfigDirForMessage names the directory gh itself resolves its own
// config (and login) from, purely for this check's own error message --
// mirrors gh's own documented precedence (GH_CONFIG_DIR, then
// $XDG_CONFIG_HOME/gh, then ~/.config/gh) so an operator who redirected
// XDG_CONFIG_HOME for a fresh factoryd config (exactly what happened in
// the walk) is told which directory was actually checked, not left to
// rediscover that gh keeps its own login separate from factoryd's.
func ghConfigDirForMessage() string {
	if dir := os.Getenv("GH_CONFIG_DIR"); dir != "" {
		return dir
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gh")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gh")
}

// ghKnownHosts parses `gh auth status`'s own output (no -hostname filter,
// so it reports every host gh has a login configured for, logged in or
// not) for each host-section's own unindented header line -- gh's stable
// output shape for this command names a hostname alone at the start of a
// line with no leading whitespace; every other line in real output is
// indented ("  Logged in to ...", "  Token: ...", etc). Deliberately
// reads only WHICH LINES are unindented, never any line's own content
// beyond that -- never inspects, stores, or surfaces a Token: line or an
// account name, so this can never leak a credential even indirectly.
// Errors are ignored: `gh auth status` exits non-zero whenever ANY
// configured host isn't logged in (common), which is not itself a
// reason to discard the header lines it still printed; a genuine
// gh-not-installed/unreachable failure is caught earlier by
// doctorCheckGHAuth's own `gh --version` check.
func ghKnownHosts(ctx context.Context, run ghAuthRunner) []string {
	out, _ := run(ctx, "gh", "auth", "status")
	var hosts []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			hosts = append(hosts, trimmed)
		}
	}
	return hosts
}

// ghHostToCheck decides which hostname doctorCheckGHAuth should actually
// run `gh auth status --hostname` against for origin's own resolved
// host, or ("", false) when none applies. An adversarial review of the
// gh-auth check found the original check ran `gh auth status --hostname
// <host>` for WHATEVER host the origin remote resolved to, which fails
// (and blocked quickstart outright) for a GitLab remote, a bare SSH
// config alias (`git@github.com-work:...` resolves to the literal host
// "github.com-work", an alias name, not a real DNS host), or an ssh://
// remote naming a nonstandard host -- none of which gh has ever heard
// of, or needs to.
//
// A round-2 adversarial review found the first version of
// this treated ANY host merely CONTAINING "github" as github.com,
// checked BEFORE consulting gh's own known hosts -- a real, logged-in
// GitHub Enterprise host like "github.acme.com" was wrongly checked
// against github.com instead (a blocking, actively wrong result: either
// a spurious failure if not logged into github.com, or a false pass
// naming the wrong host). Fixed by reordering AND narrowing:
//
//   - host is one gh already has a login configured for (ghKnownHosts,
//     an exact match): checked directly and FIRST, so a real GHE host
//     like "github.acme.com" is always recognized on its own terms
//     before anything else gets a chance to misclassify it.
//   - Otherwise, host == "github.com": checked directly.
//   - Otherwise, isSSH is true AND host starts with "github.com" (e.g.
//     "github.com-work", an SSH config Host alias -- see
//     doctorRepoGitHubHost's own doc comment for why this is restricted
//     to SSH-shaped remotes): checked as github.com, the real host gh
//     authenticates against. A "contains github" substring match (the
//     pre-round-2 rule) is gone entirely -- it's what let
//     "github.acme.com" match in the first place.
//   - anything else (GitLab, an unconfigured GHE host, ...): ("", false)
//     -- doctorCheckGHAuth reports this as advisory, never blocking.
func ghHostToCheck(ctx context.Context, run ghAuthRunner, host string, isSSH bool) (string, bool) {
	for _, known := range ghKnownHosts(ctx, run) {
		if strings.EqualFold(known, host) {
			return host, true
		}
	}
	lower := strings.ToLower(host)
	if lower == "github.com" {
		return "github.com", true
	}
	if isSSH && strings.HasPrefix(lower, "github.com") {
		return "github.com", true
	}
	return "", false
}

// doctorCheckGHAuth exists because an adversarial review found that
// opening a PR is the whole point of a run, but neither `doctor` nor
// `quickstart` checked that `gh` was even installed and logged in before
// this fix -- in the walk, a full build was accepted and only then failed
// to open its PR ("gh pr create: exit status 4 ... gh auth login"),
// spending a whole build cycle to discover an auth gap that costs
// nothing to check up front. Runs only when workspaceDir has a remote
// this can resolve a host from (doctorRepoGitHubHost); silently skipped
// (empty doctorCheck, no error) otherwise, same as every other doctor
// check with nothing concrete to check against. Never surfaces gh's own
// output: `gh auth status`'s exit code alone is the pass/fail signal, so
// this can never leak a token even indirectly through a redacted-looking
// but not-actually-redacted message.
//
// A remote whose host isn't github.com, a github.com SSH alias, or a
// host gh already knows about (ghHostToCheck) is reported as Advisory
// (the same review) -- it warns, but never fails this check or
// blocks quickstart, since gh genuinely has nothing to say about a
// GitLab remote or an as-yet-unconfigured GHE host.
func doctorCheckGHAuth(ctx context.Context, run ghAuthRunner, workspaceDir string) doctorCheck {
	const name = "gh auth (PR open)"
	host, isSSH := doctorRepoGitHubHost(ctx, run, workspaceDir)
	if host == "" {
		return doctorCheck{Name: name}
	}
	if _, err := run(ctx, "gh", "--version"); err != nil {
		return doctorCheck{
			Name: name,
			Err:  fmt.Errorf("gh is not installed or not on $PATH: factoryd shells out to gh to open and ready every PR (%v)", err),
			Fix:  "install the GitHub CLI: https://cli.github.com",
		}
	}
	checkHost, ok := ghHostToCheck(ctx, run, host, isSSH)
	if !ok {
		return doctorCheck{
			Name:     name,
			Err:      fmt.Errorf("origin's host %q is not github.com and gh has no login configured for it -- the PR-open step will need gh configured for this host before it can open a PR", host),
			Fix:      fmt.Sprintf("gh auth login --hostname %s, if this is a GitHub-compatible host", host),
			Advisory: true,
		}
	}
	configDir := ghConfigDirForMessage()
	if _, err := run(ctx, "gh", "auth", "status", "--hostname", checkHost); err != nil {
		return doctorCheck{
			Name: name,
			Err:  fmt.Errorf("gh auth status --hostname %s failed -- checked %s (gh's own config directory; set GH_CONFIG_DIR or log in there if a different one is intended)", checkHost, configDir),
			Fix:  fmt.Sprintf("gh auth login --hostname %s", checkHost),
		}
	}
	return doctorCheck{Name: name}
}
