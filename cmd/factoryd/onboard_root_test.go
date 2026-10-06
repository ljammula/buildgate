package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnboardWithRootScaffoldsDirectlyNoPlaceholder is the regression test
// for onboard's own -root flag (P2: -workspace's placeholder-subdirectory
// indirection was a historical implementation detail, not a real
// requirement -- see initMain's own -root, which onboard's -root mirrors).
// Unlike -workspace, -root scaffolds directly into the given directory: no
// placeholder subdirectory is created, and no git-exclude mutation is
// attempted, since there is no placeholder to hide from `git status`.
func TestOnboardWithRootScaffoldsDirectlyNoPlaceholder(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if out, err := runGit(t, root, "init", "-q"); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-skip-doctor"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	for _, p := range []string{
		filepath.Join(root, "spec", "spec.md"),
		filepath.Join(root, "spec", "contract.md"),
		filepath.Join(root, "ARCHITECTURE.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stat %s: %v", p, err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range entries {
			if e.Name() == "workspace" {
				t.Errorf("root has a %q entry, want no placeholder subdirectory created under -root", e.Name())
			}
		}
	}
	if out, err := runGit(t, root, "check-ignore", "-q", "probe"); err == nil {
		t.Errorf("git check-ignore succeeded (%s), want no git-exclude mutation attempted under -root", out)
	}
}

// TestOnboardWithWorkspacePrintsDeprecationHint asserts the deprecation hint
// -workspace usage must print, since -root is now the preferred form.
func TestOnboardWithWorkspacePrintsDeprecationHint(t *testing.T) {
	dp := newTestDeps(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	stdout := captureStdout(t, func() {
		if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-skip-doctor"}); err != nil {
			t.Fatalf("onboardMain: %v", err)
		}
	})
	if !strings.Contains(stdout, "-workspace is deprecated") {
		t.Errorf("output = %q, want a deprecation hint for -workspace", stdout)
	}
}

// TestOnboardWithRootDoesNotPrintDeprecationHint is the -root counterpart:
// no hint is printed since -root is the preferred, non-deprecated form.
func TestOnboardWithRootDoesNotPrintDeprecationHint(t *testing.T) {
	dp := newTestDeps(t)
	root := t.TempDir()
	stdout := captureStdout(t, func() {
		if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-skip-doctor"}); err != nil {
			t.Fatalf("onboardMain: %v", err)
		}
	})
	if strings.Contains(stdout, "deprecated") {
		t.Errorf("output = %q, want no deprecation hint when -root is used", stdout)
	}
}

// TestOnboardScaffoldsOnlyMissingArtifacts is the unit-level counterpart
// to TestIntegrationOnboardScaffoldsOnlyMissingArtifacts, exercised
// through onboardMain/-root directly rather than the built binary: a repo
// with only ARCHITECTURE.md present gets spec/spec.md and
// spec/contract.md created, and ARCHITECTURE.md is left byte-identical.
func TestOnboardScaffoldsOnlyMissingArtifacts(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")
	architectureContent := []byte("# Real, already-written docs\n")
	if err := os.WriteFile(architecturePath, architectureContent, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-skip-doctor"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}

	for _, p := range []string{
		filepath.Join(root, "spec", "spec.md"),
		filepath.Join(root, "spec", "contract.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stat %s: %v, want it scaffolded since it was missing", p, err)
		}
	}

	b, err := os.ReadFile(architecturePath)
	if err != nil {
		t.Fatalf("read ARCHITECTURE.md: %v", err)
	}
	if string(b) != string(architectureContent) {
		t.Errorf("ARCHITECTURE.md content changed: got %q, want unchanged %q", b, architectureContent)
	}
}

func TestOnboardRequiresExactlyOneOfRootOrWorkspace(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	if err := onboardMain(dp, []string{"-project", "widget"}); err == nil {
		t.Fatal("expected an error when neither -root nor -workspace is given")
	}

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-workspace", workspace}); err == nil {
		t.Fatal("expected an error when both -root and -workspace are given")
	}
}

func TestOnboardWithRootRequiresDirectoryToExist(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")
	err := onboardMain(dp, []string{"-project", "widget", "-root", nonexistent, "-skip-doctor"})
	if err == nil {
		t.Fatal("expected an error when -root does not exist")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to name the missing -root directory", err)
	}
}

// TestProjectBootstrapArtifactPathsFindsOnboardRootScaffold is the
// regression test for the P1 finding from Codex review of PR #142: an
// operator who runs `factoryd onboard -root <repo>` (no placeholder
// subdirectory, artifacts scaffolded directly into <repo>) and then a real
// `factoryd <run> -workspace <repo>` (the natural flag value once -root has
// already eliminated the placeholder) must have the mandatory project-
// bootstrap preflight actually find the artifacts it just scaffolded --
// not look one level too high via the historical
// filepath.Dir(-workspace) convention and report a freshly, correctly
// onboarded repo as missing all three.
func TestProjectBootstrapArtifactPathsFindsOnboardRootScaffold(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-skip-doctor"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	spec, contract, architecture := projectBootstrapArtifactPaths(root)
	for _, p := range []string{spec, contract, architecture} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("projectBootstrapArtifactPaths(%q) named %s, which does not exist: %v", root, p, err)
		}
	}
}

// TestProjectBootstrapArtifactPathsPrefersHistoricalConvention confirms the
// fix above changes nothing for the pre-existing, historical -workspace
// convention (a placeholder subdirectory whose parent holds the real
// artifacts) -- the new workspaceAbs-as-root fallback must only apply when
// the historical location has nothing, never override it when both could
// apply.
func TestProjectBootstrapArtifactPathsPrefersHistoricalConvention(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-skip-doctor"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	spec, _, _ := projectBootstrapArtifactPaths(workspace)
	wantSpec := filepath.Join(root, "spec", "spec.md")
	if spec != wantSpec {
		t.Errorf("projectBootstrapArtifactPaths(%q) spec = %q, want %q (the historical filepath.Dir convention)", workspace, spec, wantSpec)
	}
}

// TestProjectBootstrapArtifactPathsFindsRealUnonboardedRepoRoot is the
// regression test for the bug found live (2026-09-16) running a real
// `factoryd -workspace ~/code/todo-service ...` against an actual,
// already-populated, never-onboarded Go repo: before this fix,
// repoRootForWorkspace fell back to historicalRoot (workspaceAbs's own
// parent) whenever NEITHER convention's spec.md existed yet, with no way
// to tell "a real repo checkout, just not onboarded" apart from "an empty
// placeholder subdirectory" -- so the mandatory project-bootstrap preflight
// silently checked one directory above the actual repo and reported a
// real, populated project as unreadable there. Unlike
// TestProjectBootstrapArtifactPathsFindsOnboardRootScaffold (which onboards
// first, so spec/spec.md already exists at root), this test deliberately
// never onboards -- root just holds ordinary repo files -- to reproduce
// the exact pre-onboarding state the live run hit.
func TestProjectBootstrapArtifactPathsFindsRealUnonboardedRepoRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Ordinary repo content, no onboarding artifacts -- e.g. go.mod, a
	// source file. What matters is only that root is non-empty: the real
	// signal workspaceContainsRealContent checks, distinguishing this from
	// the historical convention's own empty placeholder.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, contract, architecture := projectBootstrapArtifactPaths(root)
	wantSpec := filepath.Join(root, "spec", "spec.md")
	wantContract := filepath.Join(root, "spec", "contract.md")
	wantArchitecture := filepath.Join(root, "ARCHITECTURE.md")
	if spec != wantSpec {
		t.Errorf("projectBootstrapArtifactPaths(%q) spec = %q, want %q (root itself, not its parent)", root, spec, wantSpec)
	}
	if contract != wantContract {
		t.Errorf("projectBootstrapArtifactPaths(%q) contract = %q, want %q", root, contract, wantContract)
	}
	if architecture != wantArchitecture {
		t.Errorf("projectBootstrapArtifactPaths(%q) architecture = %q, want %q", root, architecture, wantArchitecture)
	}
}

// TestProjectBootstrapArtifactPathsEmptyWorkspaceStaysHistorical confirms
// the fix above is scoped to a non-empty workspace only: an empty (or
// not-yet-created) -workspace -- the one shape the historical
// placeholder-subdirectory convention ever actually produces, per
// onboardMain's own doc comment ("-workspace is meant to stay an empty,
// ignored directory for its entire life") -- must still fall back to
// historicalRoot exactly as before, never be mistaken for a direct-root
// repo just because it happens to have no spec.md either.
func TestProjectBootstrapArtifactPathsEmptyWorkspaceStaysHistorical(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	spec, _, _ := projectBootstrapArtifactPaths(workspace)
	wantSpec := filepath.Join(root, "spec", "spec.md")
	if spec != wantSpec {
		t.Errorf("projectBootstrapArtifactPaths(%q) spec = %q, want %q (historical convention, workspace's own parent)", workspace, spec, wantSpec)
	}
}

// TestResolvePiTicketPathFindsOnboardRootScaffold is the regression test
// for the round-2 Codex finding on PR #142: resolvePiTicketPath (native-
// ticket discovery for `-ticket`/`-ticket-file`) had the identical
// filepath.Dir(-workspace) assumption projectBootstrapArtifactPaths did --
// a native ticket created at <repo>/spec/tickets/001-x.md after
// `onboard -root <repo>` was invisible to a later
// `factoryd <run> -workspace <repo> -ticket 001-x`, the same silent-
// failure-mode class as the round-1 finding, just for ticket discovery
// instead of spec/contract/architecture discovery.
func TestResolvePiTicketPathFindsOnboardRootScaffold(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-skip-doctor"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	ticketsDir := filepath.Join(root, "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(ticketsDir, "001-add-thing.md")
	if err := os.WriteFile(ticketPath, []byte("# Ticket\n\nThis is an existing repo.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, number, err := resolvePiTicketPath(root, "001-add-thing", "")
	if err != nil {
		t.Fatalf("resolvePiTicketPath: %v", err)
	}
	if resolved != ticketPath {
		t.Errorf("resolvePiTicketPath(%q, ...) = %q, want %q", root, resolved, ticketPath)
	}
	if number != 1 {
		t.Errorf("ticket number = %d, want 1", number)
	}
}

// runGit runs a git subcommand against dir, returning combined output.
func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
