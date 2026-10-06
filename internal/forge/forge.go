// Package forge provides the factory's evidence-carrying draft-PR
// mechanism: on an accepted run, push its own isolated branch and open a
// draft pull request whose body is the run's own durable evidence
// package. Every peer this project was compared against lands a PR, but
// none attach independently computed evidence the way this factory's own
// durable run record already carries.
//
// Best-effort by design, the same way internal/notify's own Discord
// dispatch already is for a quarantine notification: a failure here must
// never retroactively change a run's own already-recorded, already-saved
// acceptance -- see PullRequestOpener's own doc comment.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
)

// PullRequestOpener pushes headSHA (already committed, inside
// workspaceDir) to branch on its configured remote and opens a draft
// pull request with title/body, returning the PR's own URL. A non-nil
// error means neither the push nor the PR exist -- callers must treat
// this as best-effort and never let it change a run's own already-saved
// state (cmd/factoryd's own call site logs and continues on error,
// exactly the way a failed Discord dispatch does today).
type PullRequestOpener interface {
	// headSHA, when non-empty, is pushed via an explicit
	// "<headSHA>:refs/heads/<branch>" refspec rather than a bare branch
	// push: a bare `git push origin <branch>` pushes whatever the
	// branch ref happens to point at the MOMENT this command runs,
	// which can differ from whatever commit a caller verified just
	// before calling this (the branch could move in between). Pinning
	// the exact SHA in the refspec closes that window entirely rather
	// than narrowing it.
	// Empty falls back to the old bare-branch push (a caller with no
	// specific commit to pin, e.g. reconcileReclaimedRun's own nil-policy
	// call shape).
	//
	// base, when non-empty, opens the PR stacked against that branch (run.
	// Run.PRBase) instead of the repo default branch -- ticket N of a
	// multi-ticket request stacking on ticket N-1's still-open branch, so
	// ticket N's PR shows only its own delta rather than repeating ticket
	// N-1's already-open commits. The named branch is checked against
	// "origin" first (it can have been deleted since -- e.g. ticket N-1
	// merged and GitHub deleted its branch -- between ticketQueueEntry
	// computing base and this PR actually opening): present, --base is
	// passed to `gh pr create`; absent, the PR opens against the default
	// branch instead, logged, never an error -- opening SOME PR still beats
	// failing this whole best-effort call over a branch that only existed
	// to describe a base that no longer needs describing. Empty base opens
	// against the default branch exactly as before this parameter existed,
	// with no extra origin check at all.
	OpenDraftPullRequest(ctx context.Context, workspaceDir, branch, base, headSHA, title, body string) (url string, err error)
	// VerifyExistingPullRequest reads url's own current state -- a
	// finding from that same review: cmd/factoryd's retryPullRequestOpener
	// calls this to confirm a PR recovered from gh's own "already exists" error
	// (OpenDraftPullRequestErrorURL) is actually the right one to link --
	// open, same-repository, and pointing at the exact commit expected --
	// before ever treating it as a success, rather than trusting
	// whatever `gh pr view` returns unconditionally (which can be closed,
	// merged, or a fork's own same-named branch).
	VerifyExistingPullRequest(ctx context.Context, workspaceDir, url string) (state, headRefOid string, isCrossRepository bool, err error)
}

// OpenDraftPullRequestErrorURL conservatively extracts a GitHub pull
// request URL from OpenDraftPullRequest's own error text: `gh pr create`
// failing because the PR already exists is gh's own text, not a network
// response this package parses untrusted -- but conservative regardless,
// matching only an https URL ending in "/pull/<digits>" (GitHub's and every GHE
// instance's own stable PR URL shape), never inventing one from
// surrounding prose. "" when no such URL appears in err's own text.
var embeddedPullRequestURLPattern = regexp.MustCompile(`https://\S+/pull/\d+`)

func OpenDraftPullRequestErrorURL(errText string) string {
	return embeddedPullRequestURLPattern.FindString(errText)
}

// GHPullRequestOpener shells out to the `git`/`gh` CLIs, the tools this
// project's own operator workflow already assumes are installed and
// authenticated (every GitHub operation elsewhere in this codebase's own
// history goes through `gh`).
type GHPullRequestOpener struct {
	// GitBinary/GHBinary default to "git"/"gh" when empty.
	GitBinary string
	GHBinary  string
}

// OpenDraftPullRequest pushes headSHA (or, when empty, whatever branch
// currently points at in workspaceDir) to branch on "origin" (the one
// remote name every git-hosting convention this project targets uses),
// then runs `gh pr create --draft`. gh's own stdout on success is
// exactly the new PR's URL as its last line -- this is gh's own
// documented, stable output shape for `pr create`, not a fragile
// screen-scrape of prose meant for a human.
func (o GHPullRequestOpener) OpenDraftPullRequest(ctx context.Context, workspaceDir, branch, base, headSHA, title, body string) (string, error) {
	gitBinary := o.GitBinary
	if gitBinary == "" {
		gitBinary = "git"
	}
	ghBinary := o.GHBinary
	if ghBinary == "" {
		ghBinary = "gh"
	}

	// refspec: an explicit "<headSHA>:refs/heads/<branch>" when headSHA
	// is known, pinning exactly the commit a caller already verified,
	// not whatever the branch ref happens to point at the moment this
	// command actually runs. Falls back to a bare branch push for a
	// caller with no specific commit to pin.
	refspec := branch
	if headSHA != "" {
		refspec = headSHA + ":refs/heads/" + branch
	}
	push := exec.CommandContext(ctx, gitBinary, "push", "origin", refspec)
	push.Dir = workspaceDir
	if out, err := push.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git push %s: %w: %s", refspec, err, strings.TrimSpace(string(out)))
	}

	args := []string{"pr", "create", "--draft", "--head", branch, "--title", title, "--body", body}
	// base's own existence on "origin" is checked here, not assumed: it
	// can have been deleted since ticketQueueEntry computed it (e.g. the
	// ticket it names has since merged and GitHub deleted its branch) --
	// see OpenDraftPullRequest's own doc comment on the interface for why
	// that isn't an error, just a fallback to the default branch.
	if base != "" {
		lsRemote := exec.CommandContext(ctx, gitBinary, "ls-remote", "--exit-code", "--heads", "origin", base)
		lsRemote.Dir = workspaceDir
		out, err := lsRemote.CombinedOutput()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			args = append(args, "--base", base)
		case errors.As(err, &exitErr) && exitErr.ExitCode() == 2:
			// --exit-code's "no matching ref": the branch is really gone.
			log.Printf("gh pr create: base branch %q no longer exists on origin; opening against the default branch", base)
		default:
			// Network, auth or remote failure: opening against the default
			// branch here would put the lower ticket's unmerged commits in
			// this PR, so fail and let the PR-open retry try again.
			return "", fmt.Errorf("git ls-remote origin %s: %w: %s", base, err, strings.TrimSpace(string(out)))
		}
	}
	create := exec.CommandContext(ctx, ghBinary, args...)
	create.Dir = workspaceDir
	out, err := create.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	url := lastNonEmptyLine(string(out))
	if url == "" {
		return "", fmt.Errorf("gh pr create produced no output naming the new PR's URL: %s", strings.TrimSpace(string(out)))
	}
	return url, nil
}

// VerifyExistingPullRequest runs `gh pr view <url> --json
// url,state,headRefOid,isCrossRepository`. Callers decide whether url is
// actually safe to link (open, same-repository, pointing at the expected
// commit) from the returned fields; this only reads them.
func (o GHPullRequestOpener) VerifyExistingPullRequest(ctx context.Context, workspaceDir, url string) (state, headRefOid string, isCrossRepository bool, err error) {
	ghBinary := o.GHBinary
	if ghBinary == "" {
		ghBinary = "gh"
	}
	view := exec.CommandContext(ctx, ghBinary, "pr", "view", url, "--json", "url,state,headRefOid,isCrossRepository")
	view.Dir = workspaceDir
	out, viewErr := view.CombinedOutput()
	if viewErr != nil {
		return "", "", false, fmt.Errorf("gh pr view %s: %w: %s", url, viewErr, strings.TrimSpace(string(out)))
	}
	var parsed struct {
		State             string `json:"state"`
		HeadRefOid        string `json:"headRefOid"`
		IsCrossRepository bool   `json:"isCrossRepository"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", "", false, fmt.Errorf("parse gh pr view %s output: %w", url, err)
	}
	return parsed.State, parsed.HeadRefOid, parsed.IsCrossRepository, nil
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
