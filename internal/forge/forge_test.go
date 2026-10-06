package forge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeExecutable writes a shell script at dir/name and returns its
// path, chmod'd executable -- the same fake-binary-injection pattern
// cmd/factoryd's own testdata/fake_build_app.sh establishes, kept inline
// here since each test's own fake behavior is small and specific to it.
func writeFakeExecutable(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

// TestOpenDraftPullRequestPushesThenCreates proves the real sequence and
// argv shape: git push origin <branch> first, then gh pr create
// --draft with the given head/title/body, and the PR URL is read back
// from gh's own stdout.
func TestOpenDraftPullRequestPushesThenCreates(t *testing.T) {
	dir := t.TempDir()
	pushLog := filepath.Join(dir, "push.log")
	prLog := filepath.Join(dir, "pr.log")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `echo "$@" > `+pushLog+`
exit 0
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "$@" > `+prLog+`
echo "https://github.com/acme/widgets/pull/42"
exit 0
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	url, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-1", "", "deadbeef1234", "ticket(001): fixture", "evidence body")
	if err != nil {
		t.Fatalf("OpenDraftPullRequest: %v", err)
	}
	if url != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("url = %q, want the PR URL from gh's own stdout", url)
	}

	// An explicit "<headSHA>:refs/heads/<branch>" refspec, not a bare
	// branch push -- pins exactly the commit a caller already verified,
	// not whatever the branch ref happens to point at the moment this
	// push actually runs.
	pushArgs, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatalf("read push log: %v", err)
	}
	if got := strings.TrimSpace(string(pushArgs)); got != "push origin deadbeef1234:refs/heads/factoryd/run-1" {
		t.Errorf("git argv = %q, want %q", got, "push origin deadbeef1234:refs/heads/factoryd/run-1")
	}

	prArgs, err := os.ReadFile(prLog)
	if err != nil {
		t.Fatalf("read pr log: %v", err)
	}
	got := strings.TrimSpace(string(prArgs))
	for _, want := range []string{"pr create", "--draft", "--head factoryd/run-1", "--title ticket(001): fixture", "--body evidence body"} {
		if !strings.Contains(got, want) {
			t.Errorf("gh argv = %q, want it to contain %q", got, want)
		}
	}
}

// TestOpenDraftPullRequestEmptyHeadSHAFallsBackToBareBranchPush covers a
// caller with no specific commit to pin (e.g. reconcileReclaimedRun's own
// nil-policy call shape) -- OpenDraftPullRequest must still push the
// plain branch name, not an empty-SHA refspec.
func TestOpenDraftPullRequestEmptyHeadSHAFallsBackToBareBranchPush(t *testing.T) {
	dir := t.TempDir()
	pushLog := filepath.Join(dir, "push.log")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `echo "$@" > `+pushLog+`
exit 0
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "https://github.com/acme/widgets/pull/42"
exit 0
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	if _, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-1", "", "", "title", "body"); err != nil {
		t.Fatalf("OpenDraftPullRequest: %v", err)
	}
	pushArgs, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatalf("read push log: %v", err)
	}
	if got := strings.TrimSpace(string(pushArgs)); got != "push origin factoryd/run-1" {
		t.Errorf("git argv = %q, want the bare branch push %q", got, "push origin factoryd/run-1")
	}
}

// TestOpenDraftPullRequestFailsClosedWhenPushFails proves a failed push
// never reaches gh pr create at all -- no PR against a branch that was
// never actually pushed.
func TestOpenDraftPullRequestFailsClosedWhenPushFails(t *testing.T) {
	dir := t.TempDir()
	ghCalledMarker := filepath.Join(dir, "gh-was-called")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `echo "fatal: remote rejected" >&2
exit 1
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `touch `+ghCalledMarker+`
echo "https://github.com/acme/widgets/pull/42"
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	_, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-1", "", "", "title", "body")
	if err == nil {
		t.Fatal("OpenDraftPullRequest succeeded despite a failed push, want an error")
	}
	if !strings.Contains(err.Error(), "remote rejected") {
		t.Errorf("err = %v, want it to name the real push failure", err)
	}
	if _, statErr := os.Stat(ghCalledMarker); statErr == nil {
		t.Error("gh pr create ran despite the push having failed")
	}
}

// TestOpenDraftPullRequestFailsClosedWhenGHFails proves a gh failure (not
// authenticated, no such repo, ...) is reported with gh's own real error
// text, not swallowed.
func TestOpenDraftPullRequestFailsClosedWhenGHFails(t *testing.T) {
	dir := t.TempDir()
	gitBinary := writeFakeExecutable(t, dir, "fake-git", "exit 0\n")
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "gh: not logged in to any hosts" >&2
exit 1
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	_, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-1", "", "", "title", "body")
	if err == nil {
		t.Fatal("OpenDraftPullRequest succeeded despite gh failing, want an error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("err = %v, want gh's own real error text", err)
	}
}

// TestOpenDraftPullRequestFailsClosedOnEmptyGHOutput proves a gh process
// that exits 0 but prints nothing (a gh version whose output shape
// changed, or a genuinely empty response) is reported as an error rather
// than returning a blank, unusable URL.
func TestOpenDraftPullRequestFailsClosedOnEmptyGHOutput(t *testing.T) {
	dir := t.TempDir()
	gitBinary := writeFakeExecutable(t, dir, "fake-git", "exit 0\n")
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", "exit 0\n")

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	_, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-1", "", "", "title", "body")
	if err == nil {
		t.Fatal("OpenDraftPullRequest succeeded with no PR URL in gh's output, want an error")
	}
}

// TestOpenDraftPullRequestWithBaseExistingOnOriginPassesBaseFlag proves a
// non-empty base that `git ls-remote --exit-code --heads origin <base>`
// confirms still exists is passed to `gh pr create` as --base -- the
// stacked-PR case (run.Run.PRBase set, and its named branch not yet
// deleted).
func TestOpenDraftPullRequestWithBaseExistingOnOriginPassesBaseFlag(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "git-calls.log")
	prLog := filepath.Join(dir, "pr.log")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `echo "$@" >> `+callLog+`
exit 0
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "$@" > `+prLog+`
echo "https://github.com/acme/widgets/pull/42"
exit 0
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	if _, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-2", "factoryd/run-1", "", "title", "body"); err != nil {
		t.Fatalf("OpenDraftPullRequest: %v", err)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read git call log: %v", err)
	}
	if !strings.Contains(string(calls), "ls-remote --exit-code --heads origin factoryd/run-1") {
		t.Errorf("git calls = %q, want an ls-remote check of the base branch", string(calls))
	}

	prArgs, err := os.ReadFile(prLog)
	if err != nil {
		t.Fatalf("read pr log: %v", err)
	}
	if got := strings.TrimSpace(string(prArgs)); !strings.Contains(got, "--base factoryd/run-1") {
		t.Errorf("gh argv = %q, want it to contain --base factoryd/run-1", got)
	}
}

// TestOpenDraftPullRequestWithBaseMissingFromOriginOmitsBaseFlag proves a
// base branch ls-remote no longer finds (e.g. deleted after ticket N-1
// merged) falls back to opening against the default branch instead of
// failing the whole call.
func TestOpenDraftPullRequestWithBaseMissingFromOriginOmitsBaseFlag(t *testing.T) {
	dir := t.TempDir()
	prLog := filepath.Join(dir, "pr.log")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `if [ "$1" = "ls-remote" ]; then exit 2; fi
exit 0
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "$@" > `+prLog+`
echo "https://github.com/acme/widgets/pull/42"
exit 0
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	if _, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-2", "factoryd/run-1", "", "title", "body"); err != nil {
		t.Fatalf("OpenDraftPullRequest: %v", err)
	}

	prArgs, err := os.ReadFile(prLog)
	if err != nil {
		t.Fatalf("read pr log: %v", err)
	}
	if got := strings.TrimSpace(string(prArgs)); strings.Contains(got, "--base") {
		t.Errorf("gh argv = %q, want no --base once the named branch is gone from origin", got)
	}
}

// TestOpenDraftPullRequestFailsWhenOriginCannotBeChecked: a network or
// auth failure (exit 128) is not "the base branch is gone". Falling back
// to the default branch would open a PR carrying the lower ticket's
// unmerged commits, so the open fails and no PR is created.
func TestOpenDraftPullRequestFailsWhenOriginCannotBeChecked(t *testing.T) {
	dir := t.TempDir()
	prLog := filepath.Join(dir, "pr.log")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `if [ "$1" = "ls-remote" ]; then echo "fatal: unable to access origin" >&2; exit 128; fi
exit 0
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "$@" > `+prLog+`
echo "https://github.com/acme/widgets/pull/42"
exit 0
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	if _, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-2", "factoryd/run-1", "", "title", "body"); err == nil {
		t.Fatal("OpenDraftPullRequest succeeded, want an error when origin can't be checked")
	}
	if _, err := os.Stat(prLog); err == nil {
		t.Error("gh pr create ran; want no PR opened when the base can't be confirmed")
	}
}

// TestOpenDraftPullRequestEmptyBaseSkipsOriginCheck proves an empty base
// (the ordinary, non-stacked case) makes no ls-remote call at all, not
// just an ls-remote call that happens to come back negative -- a caller
// with nothing to check shouldn't pay for the extra round-trip.
func TestOpenDraftPullRequestEmptyBaseSkipsOriginCheck(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "git-calls.log")
	gitBinary := writeFakeExecutable(t, dir, "fake-git", `echo "$@" >> `+callLog+`
exit 0
`)
	ghBinary := writeFakeExecutable(t, dir, "fake-gh", `echo "https://github.com/acme/widgets/pull/42"
exit 0
`)

	opener := GHPullRequestOpener{GitBinary: gitBinary, GHBinary: ghBinary}
	if _, err := opener.OpenDraftPullRequest(context.Background(), dir, "factoryd/run-1", "", "", "title", "body"); err != nil {
		t.Fatalf("OpenDraftPullRequest: %v", err)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read git call log: %v", err)
	}
	if strings.Contains(string(calls), "ls-remote") {
		t.Errorf("git calls = %q, want no ls-remote call for an empty base", string(calls))
	}
}
