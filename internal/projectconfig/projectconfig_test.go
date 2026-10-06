package projectconfig

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// commitFile writes name at dir/name and commits it, giving the repo a
// real HEAD so Load's committed-tree read path is exercised.
func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", name).CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", name, err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-q", "-m", "add "+name).CombinedOutput(); err != nil {
		t.Fatalf("git commit %s: %v\n%s", name, err, out)
	}
}

func TestLoadValidConfig(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	sub := filepath.Join(root, "workspace")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := `verify_command: "make verify"
fast_check_command: "make fast-check"
full_suite_command: "go test ./..."
lint_command: "golangci-lint run ./..."
security_command: "govulncheck ./..."
unit_test_command: "go test ./..."
integration_test_command: "go test -tags=integration ./..."
test_patterns:
  - "*_test.go"
  - "*.spec.ts"
preflight_profile: brownfield
protected_paths:
  - "README.md"
  - "go.mod"
token_ceiling: 1000
cost_ceiling_micro_usd: 5000000
`
	// No HEAD commit yet in this repo, so Load falls back to reading the
	// plain worktree file -- see TestLoadReadsCommittedContentNotModifiedWorktreeCopy
	// for the committed-tree path.
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, found, err := Load(sub)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if cfg.VerifyCommand != "make verify" {
		t.Errorf("VerifyCommand = %q", cfg.VerifyCommand)
	}
	if cfg.FastCheckCommand != "make fast-check" {
		t.Errorf("FastCheckCommand = %q", cfg.FastCheckCommand)
	}
	if cfg.LintCommand != "golangci-lint run ./..." {
		t.Errorf("LintCommand = %q", cfg.LintCommand)
	}
	if cfg.SecurityCommand != "govulncheck ./..." {
		t.Errorf("SecurityCommand = %q", cfg.SecurityCommand)
	}
	if cfg.FullSuiteCommand != "go test ./..." {
		t.Errorf("FullSuiteCommand = %q", cfg.FullSuiteCommand)
	}
	if cfg.UnitTestCommand != "go test ./..." {
		t.Errorf("UnitTestCommand = %q", cfg.UnitTestCommand)
	}
	if cfg.IntegrationTestCommand != "go test -tags=integration ./..." {
		t.Errorf("IntegrationTestCommand = %q", cfg.IntegrationTestCommand)
	}
	if want := []string{"*_test.go", "*.spec.ts"}; len(cfg.TestPatterns) != len(want) || cfg.TestPatterns[0] != want[0] || cfg.TestPatterns[1] != want[1] {
		t.Errorf("TestPatterns = %v, want %v", cfg.TestPatterns, want)
	}
	if cfg.PreflightProfile != "brownfield" {
		t.Errorf("PreflightProfile = %q", cfg.PreflightProfile)
	}
	if len(cfg.ProtectedPaths) != 2 {
		t.Errorf("ProtectedPaths = %v", cfg.ProtectedPaths)
	}
	if cfg.TokenCeiling != 1000 {
		t.Errorf("TokenCeiling = %d", cfg.TokenCeiling)
	}
	if cfg.CostCeilingMicroUSD != 5000000 {
		t.Errorf("CostCeilingMicroUSD = %d", cfg.CostCeilingMicroUSD)
	}
}

func TestLoadNoFileReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	cfg, found, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found || cfg != nil {
		t.Fatalf("expected not found, got found=%v cfg=%v", found, cfg)
	}
}

func TestLoadNonGitWorkspaceUsesWorkspaceDirItself(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".factory.yml"), []byte("verify_command: \"pytest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if cfg.VerifyCommand != "pytest" {
		t.Errorf("VerifyCommand = %q", cfg.VerifyCommand)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("bogus_field: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

// TestLoadRejectsSandboxImageAsUnknownField documents that sandbox_image
// is deliberately not part of the schema (it would bypass
// -api-allowed-sandbox-images on an API-started run, since that allowlist
// check happens in serve before the run process reads this file) -- a
// .factory.yml naming it is rejected the same way any other unknown key
// is.
func TestLoadRejectsSandboxImageAsUnknownField(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("sandbox_image: \"registry.example/org/img@sha256:deadbeef\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error: sandbox_image is not a recognized key")
	}
}

// TestLoadRejectsProtectedPathContainingComma is the Codex review finding
// on PR #84: applyProjectConfigDefaults joins ProtectedPaths into
// -release-protected-paths' own comma-separated form, so a path
// containing a comma would silently split into two paths and never
// protect the file it actually named.
func TestLoadRejectsProtectedPathContainingComma(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("protected_paths:\n  - \"schema,v1.json\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error: protected_paths entry contains a comma")
	}
}

// TestLoadAcceptsEmptyFile is the Codex review finding on PR #84: every
// field in Config is optional, so a repo committing an empty or
// comment-only .factory.yml (a placeholder, or one that only sets a
// preflight profile via a later edit) must decode as a valid, all-zero
// Config rather than fail on YAML's own io.EOF for an empty document.
func TestLoadAcceptsEmptyFile(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("# nothing configured yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for an existing, empty file")
	}
	if cfg.VerifyCommand != "" || cfg.FastCheckCommand != "" || cfg.PreflightProfile != "" ||
		len(cfg.ProtectedPaths) != 0 || cfg.TokenCeiling != 0 || cfg.CostCeilingMicroUSD != 0 {
		t.Errorf("Load returned non-zero Config for an empty file: %+v", cfg)
	}
}

func TestLoadRejectsBadPreflightProfile(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("preflight_profile: strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for invalid preflight_profile")
	}
}

func TestLoadRejectsEmptyTestPatternEntry(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	yml := "test_patterns:\n  - \"\"\n"
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(root); err == nil {
		t.Fatal("expected error for an empty test_patterns entry")
	}
}

// TestLoadRejectsRemovedReviewPolicyKey covers the per-round
// -review-policy reviewer path's removal (it drove a Pi "reviewer"
// extension never present in the sandbox image, always reporting
// "unavailable"): an old .factory.yml still declaring review_policy now
// fails to load like any other unknown key, via Load's own
// dec.KnownFields(true), rather than being silently accepted and
// ignored.
func TestLoadRejectsRemovedReviewPolicyKey(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("review_policy: advisory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(root); err == nil {
		t.Fatal("expected error for a removed review_policy key")
	}
}

func TestLoadFindsGitTopLevelFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"go test ./...\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(nested)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("expected found=true from a nested subdirectory")
	}
	if cfg.VerifyCommand != "go test ./..." {
		t.Errorf("VerifyCommand = %q", cfg.VerifyCommand)
	}
}

// TestLoadReadsCommittedContentNotModifiedWorktreeCopy is the regression
// test for the threat a coordinator review flagged: with
// isolation off a sandboxed agent can edit .factory.yml
// directly in the shared checkout. Load must use the committed HEAD
// revision, not whatever the worktree currently holds.
func TestLoadReadsCommittedContentNotModifiedWorktreeCopy(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, ".factory.yml", "verify_command: \"make verify\"\n")

	// An uncommitted edit in the worktree, as an agent running unisolated
	// might make.
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo pwned\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, found, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if cfg.VerifyCommand != "make verify" {
		t.Errorf("VerifyCommand = %q, want the committed value, not the worktree edit", cfg.VerifyCommand)
	}
}

// TestLoadIgnoresUntrackedFactoryYML covers the other half of the same
// threat: a .factory.yml that was never committed at all (only created in
// the worktree) must not be treated as this repo's config, even though a
// HEAD commit exists.
func TestLoadIgnoresUntrackedFactoryYML(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, "README.md", "placeholder\n")

	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo pwned\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, found, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found || cfg != nil {
		t.Fatalf("expected untracked .factory.yml to be ignored, got found=%v cfg=%v", found, cfg)
	}
}

// captureLog redirects the standard logger's output to a buffer for the
// duration of fn, restoring it afterwards -- used by the regression
// tests below to count warnIfWorktreeDiverges's own log lines.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOutput := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOutput)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// TestLoadWarnsOnlyOnceAcrossRepeatedLoads is the regression test:
// internal/requestsubmit.Submit calls Load twice against the same
// workspace for a single submission (ApplyProjectConfigDefaults, then its
// own full-suite-command lookup), which used to print the identical
// "exists in the worktree but is not committed" warning twice for one
// `factoryd submit`/quickstart run. A second Load against the same path
// and reason must log nothing more.
func TestLoadWarnsOnlyOnceAcrossRepeatedLoads(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, "README.md", "placeholder\n")
	path := filepath.Join(root, ".factory.yml")
	if err := os.WriteFile(path, []byte("verify_command: \"make verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := captureLog(t, func() {
		if _, _, err := Load(root); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, _, err := Load(root); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	count := strings.Count(output, "is not committed")
	if count != 1 {
		t.Errorf("warning printed %d times across two Loads, want exactly 1: %q", count, output)
	}
}

// TestLoadWarnsAgainAfterTheFileIsEdited: the dedup is per content, so a
// long-lived process (serve, worker) still warns about a later edit.
func TestLoadWarnsAgainAfterTheFileIsEdited(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, "README.md", "placeholder\n")
	path := filepath.Join(root, ".factory.yml")
	output := captureLog(t, func() {
		for _, content := range []string{"verify_command: \"make a\"\n", "verify_command: \"make b\"\n"} {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load(root); err != nil {
				t.Fatalf("Load: %v", err)
			}
		}
	})
	if count := strings.Count(output, "is not committed"); count != 2 {
		t.Errorf("warning printed %d times across two different contents, want 2: %q", count, output)
	}
}

// TestNoteReminderShownSuppressesNotCommittedWarning is the other half:
// `factoryd quickstart` writes a fresh .factory.yml and prints its
// own commit reminder, then calls NoteReminderShown so the very next Load
// (submitRequest, moments later) doesn't repeat the same information as
// projectconfig's own warning.
func TestNoteReminderShownSuppressesNotCommittedWarning(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	commitFile(t, root, "README.md", "placeholder\n")
	path := filepath.Join(root, ".factory.yml")
	if err := os.WriteFile(path, []byte("verify_command: \"make verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// warnIfWorktreeDiverges builds its own path from `git rev-parse
	// --show-toplevel`'s output, which resolves symlinks (e.g. macOS's
	// /var -> /private/var) that t.TempDir()'s own path may not have
	// resolved yet -- match that so the (path, reason) key actually
	// coincides, the same way quickstart's real factoryYMLPath (already
	// git-toplevel-relative) would.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	NoteReminderShown(filepath.Join(resolvedRoot, ".factory.yml"))

	output := captureLog(t, func() {
		if _, _, err := Load(root); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	if strings.Contains(output, "is not committed") {
		t.Errorf("expected the warning to be suppressed after NoteReminderShown, got: %q", output)
	}
}

// TestLoadRepoWithNoCommitsFallsBackToWorktreeFile covers a freshly
// `git init`ed repo with no HEAD yet: there is no committed tree to
// prefer, so the worktree file is read directly. TestLoadValidConfig
// above already exercises this implicitly; this test names it directly.
func TestLoadRepoWithNoCommitsFallsBackToWorktreeFile(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"make verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("expected found=true from the worktree file when no HEAD commit exists yet")
	}
	if cfg.VerifyCommand != "make verify" {
		t.Errorf("VerifyCommand = %q", cfg.VerifyCommand)
	}
}
