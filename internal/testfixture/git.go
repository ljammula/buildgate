// Package testfixture provides fixtures shared by integration tests.
package testfixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// NewGitRepo creates a hermetic git repo with one tracked file and an initial
// commit, so HEAD exists and the workspace starts clean. The returned path
// is a "workspace" subdirectory of a project root that also carries
// spec/spec.md, spec/contract.md, and ARCHITECTURE.md — a minimal,
// already-passing project-bootstrap scaffold (see cmd/factoryd's
// projectBootstrapArtifactPaths and `factoryd init`) so every caller of
// this fixture satisfies factoryd <run>'s mandatory project-bootstrap
// preflight (converted from opt-in to required, 2026-08-29) without having
// to know about it. A test that specifically exercises that preflight
// builds its own workspace/root pair instead of using this fixture.
//
// Rooted under t.TempDir() (macOS's default $TMPDIR) — fine for every
// caller that never bind-mounts the result into a real container. A
// DOCKER_SANDBOX_LIVE=1 test that does needs NewGitRepoAt instead: on
// colima, only paths under $HOME are visible inside containers, and
// $TMPDIR is not one of them (found live 2026-09-14 validating this
// exact fixture against a real engine).
func NewGitRepo(t testing.TB) string {
	t.Helper()
	return NewGitRepoAt(t, t.TempDir())
}

// NewGitRepoAt is NewGitRepo with an explicit root instead of t.TempDir() —
// for a DOCKER_SANDBOX_LIVE=1 test that must bind-mount the resulting
// workspace into a real container, root should be (or be under) the
// DOCKER_SANDBOX_LIVE_ROOT convention every other live test already
// follows (see internal/sandbox/docker_test.go), not t.TempDir() itself.
func NewGitRepoAt(t testing.TB, root string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	// A distinct repository basename per fixture (t.TempDir's own
	// per-test counter): the project id is the repository's basename,
	// and two fixture repos sharing one data directory must be two
	// projects, not a refused collision (release.RejectProjectCollision).
	dir := filepath.Join(root, "workspace-"+filepath.Base(root))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// -b main: several call sites (e.g. TestIntegrationDiffBaseComputesCumulativeChangedFiles)
	// `git checkout main` by name, assuming this fixture's initial branch
	// is called that. Leaving it to git's own ambient default breaks that
	// assumption in exactly the environment this fixture exists to be
	// hermetic against: GIT_CONFIG_GLOBAL/SYSTEM are nulled above, so the
	// name actually produced depends entirely on the git binary's own
	// compiled-in fallback ("main" on this repo's dev Macs' Homebrew git,
	// but "master" on the GitHub Actions Ubuntu runner once its own
	// system-level init.defaultBranch=main config is nulled the same way)
	// -- found live: CI failing on every push since 2026-09-12 while every
	// local run passed. Pin it explicitly instead of inferring it.
	run("init", "-q", "-b", "main")
	run("config", "user.email", "factoryd-test@example.com")
	run("config", "user.name", "factoryd-test")
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write content.txt: %v", err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")

	WriteProjectBootstrapScaffold(t, root)
	return dir
}

// AgentsFileName and AgentsFileContent are the root instruction file
// CommitAgentsFile commits: a request is refused on a repository without one
// at HEAD (requestsubmit.RequireAgentsFile). NewGitRepo commits none, since a
// single-ticket run needs none and the review-instruction tests start from a
// repository without it.
const (
	AgentsFileName    = "AGENTS.md"
	AgentsFileContent = "# AGENTS.md\n\nFixture repository.\n\n- Setup: none.\n- Test: `make verify`.\n- Build: none.\n- Lint: none.\n"
)

// CommitAgentsFile writes a root AGENTS.md in dir and commits everything
// there, making dir a git repository first when it is not one, for a test
// that builds its own workspace instead of using NewGitRepo. Call it after
// the test's own files are written: once a repository has a commit, its
// .factory.yml is read from HEAD too.
func CommitAgentsFile(t testing.TB, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=factoryd-test@example.com", "-c", "user.name=factoryd-test"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		run("init", "-q", "-b", "main")
	}
	if err := os.WriteFile(filepath.Join(dir, AgentsFileName), []byte(AgentsFileContent), 0o644); err != nil {
		t.Fatalf("write %s: %v", AgentsFileName, err)
	}
	run("add", "-A")
	run("commit", "-q", "--allow-empty", "-m", "fixture files with "+AgentsFileName)
}

// WriteProjectBootstrapScaffold writes an already-passing project-bootstrap
// scaffold at root (a project root, one level above a "workspace"
// subdirectory the caller controls directly, e.g. via -workspace) —
// structurally the same shape `factoryd init` produces, but pre-frozen so
// it passes policy.ProductSpecFrozen without a further edit, since test
// fixtures have no human review step. NewGitRepo calls this itself; a test
// that builds its own workspace directly (rather than through NewGitRepo)
// calls it too, unless it specifically means to exercise factoryd <run>'s
// mandatory project-bootstrap preflight (converted from opt-in to
// required, 2026-08-29).
func WriteProjectBootstrapScaffold(t testing.TB, root string) {
	t.Helper()
	specDir := filepath.Join(root, "spec")
	if err := os.MkdirAll(specDir, 0o750); err != nil {
		t.Fatalf("mkdir spec: %v", err)
	}
	files := map[string]string{
		filepath.Join(specDir, "spec.md"):                          "STATUS: FROZEN -- test fixture\n\n# Fixture Project — Product Spec\n",
		filepath.Join(specDir, "contract.md"):                      "# Fixture Project — API Contract\n\n## Conventions\n\n- Fixture conventions.\n\n## Endpoint: fixture\n\nFixture endpoint.\n",
		filepath.Join(root, "ARCHITECTURE.md"):                     "# Fixture Project — Architecture\n\n## Repo layout\n\n- Fixture layout.\n\n## Verification\n\n- Fixture verification.\n\n## Known deviations\n\n- None.\n",
		filepath.Join(specDir, "tickets", "001-fixture-ticket.md"): "This is a brand-new, empty, already-git-init-ed repo.\n\n## Goal\nexercise the factoryd fixture\n\n## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n## Verification\n`make verify` must pass.\n\n## Commit\nticket(001): fixture\n",
	}
	if err := os.MkdirAll(filepath.Join(specDir, "tickets"), 0o750); err != nil {
		t.Fatalf("mkdir ticket specs: %v", err)
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}
