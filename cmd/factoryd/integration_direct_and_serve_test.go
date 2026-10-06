package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// TestIntegrationInvalidDirectRunDoesNotCreateOwnershipLock proves pure
// argument rejection happens before the repository metadata lock is opened.
func TestIntegrationInvalidDirectRunDoesNotCreateOwnershipLock(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	commonDir, err := wsisolation.GitCommonDir(ws)
	if err != nil {
		t.Fatalf("resolve Git common directory: %v", err)
	}
	lockPath := filepath.Join(commonDir, "factoryd-direct.lock")
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "../invalid-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-workspace", ws,
		"-spec", specPath,
		"-data-dir", t.TempDir(),
		"-skip-project-check",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid direct run unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "single path component") {
		t.Fatalf("invalid direct run output = %q, want ticket path validation error", out)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("invalid direct run created ownership lock %q, stat error=%v", lockPath, err)
	}
}

// TestIntegrationProjectBootstrapPreflightHaltsWithoutScaffold is the
// regression test for the mandatory project-bootstrap preflight itself
// (converted from opt-in to required, 2026-08-29): a project whose
// filepath.Dir(-workspace) doesn't carry spec/spec.md, spec/contract.md,
// and ARCHITECTURE.md (the same convention `factoryd init` scaffolds and
// `factoryd check-project` has always validated, now actually gating
// `factoryd <run>`) must halt before build_app.py ever starts, rather than
// silently proceed against an unbootstrapped project.
func TestIntegrationProjectBootstrapPreflightHaltsWithoutScaffold(t *testing.T) {
	// not parallel-safe: newFixtureRepoWithoutBootstrapScaffold calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepoWithoutBootstrapScaffold(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a project missing its bootstrap scaffold, got success: %s", out)
	}
	if !strings.Contains(string(out), "project-bootstrap preflight failed") {
		t.Errorf("output = %q, want it to name the project-bootstrap preflight failure", out)
	}
	if strings.Contains(string(out), "fake_build_app: mode=") {
		t.Error("factoryd output contains fake_build_app marker, but build_app.py should never have been invoked before an unbootstrapped project's preflight failure")
	}
}

// TestIntegrationProjectBootstrapPreflightSkippedWithFlag confirms
// -skip-project-check is a real escape hatch: the same unbootstrapped
// project that TestIntegrationProjectBootstrapPreflightHaltsWithoutScaffold
// halts on instead proceeds normally when the operator opts out explicitly.
// TestIntegrationRunRecordsDerivedProjectID is the regression test for the
// 2026-09-05 Opus review finding S4: a run's durable record must carry the
// same project id `factoryd kill-switch -project`/GET
// /projects/{project}/release expect, computed the same way
// (release.ProjectFromWorkspace), rather than leaving it to be recomputed
// later from a workspace path that may since have changed.
func TestIntegrationRunRecordsDerivedProjectID(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil)

	want := release.ProjectFromWorkspace(ws)
	if r.Project != want {
		t.Errorf("run.Project = %q, want %q (release.ProjectFromWorkspace(%q))", r.Project, want, ws)
	}
}

func TestIntegrationProjectBootstrapPreflightSkippedWithFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepoWithoutBootstrapScaffold calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepoWithoutBootstrapScaffold(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-skip-project-check"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — -skip-project-check must let an unbootstrapped project run normally", r.State, run.StateAccepted)
	}
}

// TestIntegrationReferenceOracleCommandNotForwardedToBuildWithoutInLoopFlag
// is the regression test for a real backward-compatibility bug (found via
// review): an operator who already configured the full
// -reference-oracle-dir/-reference-oracle-mount-path/-reference-oracle-
// command trio solely for the existing post-build reference_oracle gate
// (the exact pairing -reference-oracle-command's own flag help
// recommends) must see byte-for-byte identical behavior after this
// binary added its in-loop reference-oracle check -- not have that
// oracle content silently start reaching build_app.py, which requires
// the new, explicit
// -reference-oracle-in-loop-retry opt-in. testdata/fake_build_app.sh
// records the --reference-oracle-command it received in its own log
// banner, so this reads the build log directly: an earlier version of
// this test relied on the fixture exiting 2 for any flag it didn't
// recognize, which stopped proving anything the moment the fixture
// learned this one.
func TestIntegrationReferenceOracleCommandNotForwardedToBuildWithoutInLoopFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if
	// the test also calls t.Parallel.
	ws := newFixtureRepo(t)
	// A real oracle whose command demonstrably executes it: the reference_oracle
	// gate's runtime canary quarantines anything else.
	oracleDir := writeTrustworthyOracle(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "", "30s", canaryDockerEnv(t), []string{
		"-reference-oracle-dir", oracleDir,
		"-reference-oracle-mount-path", "verify",
		"-reference-oracle-command", trustworthyOracleCommand,
	})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if len(r.Attempts) == 0 || r.Attempts[0].Kind != "build" {
		t.Fatalf("Attempts = %+v, want the build attempt first", r.Attempts)
	}
	log, err := os.ReadFile(r.Attempts[0].LogPath)
	if err != nil {
		t.Fatalf("read build log: %v", err)
	}
	if !strings.Contains(string(log), "fake_build_app: reference_oracle_command=\n") {
		t.Errorf("build_app.py received a --reference-oracle-command without -reference-oracle-in-loop-retry; build log:\n%s", log)
	}
	if r.Attempts[0].ReferenceOracleSHA256 != "" {
		t.Errorf("build attempt carries oracle hash %q without the in-loop opt-in", r.Attempts[0].ReferenceOracleSHA256)
	}
}

// TestIntegrationRunRejectsMalformedPRClosesIssueFlag proves -pr-closes-issue
// gets the same fail-fast startup validation -preflight-profile already
// has, on the trusted `factoryd <run>` CLI path where nothing else (unlike
// `factoryd submit -issue`, which only ever writes a value already built
// by parseGitHubIssueURL) guarantees a well-formed value. A malformed
// value must fail before build_app.py ever starts, not produce a broken
// "Closes foo" line silently (found via a local ai-stack code-review pass
// on PR #92).
func TestIntegrationRunRejectsMalformedPRClosesIssueFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
		"-skip-project-check",
		"-pr-closes-issue", "not-a-well-formed-reference",
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("malformed -pr-closes-issue unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "-pr-closes-issue must be") {
		t.Fatalf("output = %q, want it to name the invalid -pr-closes-issue flag", out)
	}
	if strings.Contains(string(out), "fake_build_app: mode=") {
		t.Fatalf("malformed -pr-closes-issue started build_app.py: %s", out)
	}
}

// TestIntegrationRunAcceptsWellFormedPRClosesIssueFlag proves the new
// validation does not reject the value `factoryd worker` actually
// produces: a well-formed "<owner>/<repo>#<N>" reference still runs to
// completion and lands on the durable run record unchanged.
func TestIntegrationRunAcceptsWellFormedPRClosesIssueFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-skip-project-check", "-pr-closes-issue", "acme/widgets#42"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q -- a well-formed -pr-closes-issue must not be rejected", r.State, run.StateAccepted)
	}
	if r.PRCloses != "acme/widgets#42" {
		t.Errorf("r.PRCloses = %q, want %q", r.PRCloses, "acme/widgets#42")
	}
}

// TestIntegrationRunRejectsMalformedPRBaseFlag proves -pr-base gets the
// same fail-fast startup validation -pr-closes-issue already has: a value
// starting with "-" would otherwise reach forge's `git ls-remote` and `gh
// pr create --base` as a bare argument and be parsed as another flag
// rather than the branch name it's meant to be.
func TestIntegrationRunRejectsMalformedPRBaseFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
		"-skip-project-check",
		"-pr-base", "-not-a-branch",
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("malformed -pr-base unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "-pr-base must not start with") {
		t.Fatalf("output = %q, want it to name the invalid -pr-base flag", out)
	}
	if strings.Contains(string(out), "fake_build_app: mode=") {
		t.Fatalf("malformed -pr-base started build_app.py: %s", out)
	}
}

// TestIntegrationRunAcceptsWellFormedPRBaseFlag proves a well-formed
// branch name is not rejected and lands on the durable run record
// unchanged, the same way TestIntegrationRunAcceptsWellFormedPRClosesIssueFlag
// proves for -pr-closes-issue.
func TestIntegrationRunAcceptsWellFormedPRBaseFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-skip-project-check", "-pr-base", "factoryd/run-1"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q -- a well-formed -pr-base must not be rejected", r.State, run.StateAccepted)
	}
	if r.PRBase != "factoryd/run-1" {
		t.Errorf("r.PRBase = %q, want %q", r.PRBase, "factoryd/run-1")
	}
}

// TestIntegrationProjectBootstrapPreflightAcceptsCustomArchitectureSections
// is the regression test for a real brownfield-onboarding gap found
// 2026-09-08: an existing repo's own real ARCHITECTURE.md, written
// organically over real development, essentially never already uses
// goal_pilot.py's own exact "Repo layout"/"Verification"/"Known
// deviations" heading convention -- the mandatory preflight (unlike
// -skip-project-check, which turns off structural checking entirely) had
// no way to accept a real architecture doc's own real heading names.
// -architecture-required-sections closes that: this repo's ARCHITECTURE.md
// uses "System overview"/"Running tests"/"Open issues" instead, and the
// same run halts without the flag but reaches StateAccepted with it.
func TestIntegrationProjectBootstrapPreflightAcceptsCustomArchitectureSections(t *testing.T) {
	// not parallel-safe: newFixtureRepoWithoutBootstrapScaffold calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepoWithoutBootstrapScaffold(t)
	projectRoot := filepath.Dir(ws)
	if err := os.MkdirAll(filepath.Join(projectRoot, "spec", "tickets"), 0o750); err != nil {
		t.Fatalf("mkdir spec/tickets: %v", err)
	}
	writeProjectFile := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(projectRoot, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	writeProjectFile("spec/spec.md", "STATUS: FROZEN -- reviewed 2026-09-08\n\n# Fixture product spec\n")
	writeProjectFile("spec/contract.md", "# Contract\n\n## Conventions\nJSON everywhere\n\n## Endpoint: Add\nPOST /add\n")
	writeProjectFile("ARCHITECTURE.md",
		"# Existing repo architecture\n\n"+
			"## System overview\nGo backend, Flutter frontend.\n\n"+
			"## Running tests\n`make verify`.\n\n"+
			"## Open issues\nNone tracked here.\n")
	// Named NNN-*.md (a separate, unrelated naming convention
	// TicketStructure's own caller enforces on -ticket-file) and passed
	// explicitly below, since discovery-by-ticket-id alone would look for
	// spec/tickets/fixture-ticket.md, not this NNN-prefixed name.
	writeProjectFile("spec/tickets/001-fixture-ticket.md",
		"This is a brand-new, empty, already-git-init-ed repo.\n\n"+
			"## Goal\nbuild it\n\n"+
			"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n"+
			"## Verification\nrun make verify\n\n"+
			"## Commit\nticket(001): x\n")

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	baseArgs := []string{
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-ticket-file", filepath.Join(projectRoot, "spec", "tickets", "001-fixture-ticket.md"),
	}

	// Without the flag: the repo's own real heading names ("System
	// overview"/"Running tests"/"Open issues") do not satisfy the default
	// "Repo layout"/"Verification"/"Known deviations" convention, so the
	// preflight fails before any run record is even created -- matching
	// TestIntegrationProjectBootstrapPreflightHaltsWithoutScaffold's own
	// raw-subprocess-output assertion style, not runFactorydWithSpecAndFlags
	// (which assumes a run record always exists to read back).
	cmdWithout := factorydCommand(t, append(append([]string{}, baseArgs...), "-data-dir", t.TempDir())...)
	cmdWithout.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmdWithout.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit without -architecture-required-sections, got success: %s", out)
	}
	if !strings.Contains(string(out), "architecture is missing the Repo layout section") {
		t.Errorf("output = %q, want it to name the missing default architecture sections", out)
	}

	// With the flag: the same repo, same architecture doc, now passes.
	cmdWith := factorydCommand(t, append(append([]string{}, baseArgs...),
		"-data-dir", t.TempDir(),
		"-architecture-required-sections", "System overview,Running tests,Open issues",
	)...)
	cmdWith.Env = cmdWithout.Env
	out, err = cmdWith.CombinedOutput()
	if err != nil {
		t.Fatalf("run with -architecture-required-sections failed: %v: %s", err, out)
	}
}

// TestIntegrationPiTicketStructurePreflightHaltsBeforeBuild proves the
// pi-harness-native ticket file is checked automatically by a normal direct
// factoryd run. This intentionally uses a malformed native ticket alongside
// a valid internal/ticketspec -spec: the two formats must not be conflated.
func TestIntegrationPiTicketStructurePreflightHaltsBeforeBuild(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	root := filepath.Dir(ws)
	ticketsDir := filepath.Join(root, "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir native tickets directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ticketsDir, "002-malformed.md"), []byte("## Goal\nmissing the rest\n"), 0o644); err != nil {
		t.Fatalf("write malformed native ticket: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# internal factoryd ticket spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write internal ticket spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "002-malformed",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("malformed native ticket unexpectedly allowed run: %s", out)
	}
	if !strings.Contains(string(out), "ticket_structure") {
		t.Fatalf("output = %q, want native ticket preflight failure", out)
	}
	if strings.Contains(string(out), "fake_build_app: mode=") {
		t.Fatal("build_app ran despite malformed native ticket preflight")
	}
}

func TestIntegrationPiTicketStructurePreflightAllowsValidTicket(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	ticketsDir := filepath.Join(filepath.Dir(ws), "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir native tickets directory: %v", err)
	}
	content := "This is an existing repo.\n\n## Goal\nship it\n\n## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n## Verification\n`make verify` must pass.\n\n## Commit\nticket(002): ship it\n"
	if err := os.WriteFile(filepath.Join(ticketsDir, "002-valid.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write valid native ticket: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# internal factoryd ticket spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write internal ticket spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "002-valid",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("valid native ticket run: %v: %s", err, out)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one run record: entries=%v err=%v; output=%s", entries, err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "runs", entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	var result run.Run
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode run record: %v", err)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q; output=%s", result.State, run.StateAccepted, out)
	}
}

// wellFormedRequestTicketspec is a request-pipeline ticketspec-format
// ticket (Verify-Command:/Allowed-Files:/Required-Changed-Files: headers,
// ## Goal/## Plan/## Out of scope sections) that TestIntegration
// RequestTicketPreflightValidatesSpecFile's -request-ticket run below
// exercises against testdata/fake_build_app.sh's own "commit" mode
// (edits and commits content.txt, the one file every newFixtureRepo
// fixture already tracks).
const wellFormedRequestTicketspec = `Verify-Command: true
Allowed-Files: content.txt
Required-Changed-Files: content.txt
Tests-Required: no -- integration fixture doesn't exercise tests_added

## Goal

Exercise the -request-ticket preflight against a strict-profile repo with
no spec/tickets/ directory.

## Plan

### Files to touch

- content.txt

### Steps

1. Edit content.txt.

### Tests to add

- none

### Acceptance criteria covered

- 1

## Out of scope

Nothing else.
`

// TestIntegrationRequestTicketPreflightValidatesSpecFile is the regression
// test for the live bug found 2026-09-25: a request submitted through the
// request pipeline (`factoryd submit`/console POST /requests) to a
// strict-profile repo (real spec/spec.md, spec/contract.md, ARCHITECTURE.md
// -- newFixtureRepo's own scaffold, no spec/tickets/ at all) halted at
// project-bootstrap preflight's ticket_structure check: resolvePiTicketPath
// always resolves a repo-native spec/tickets/<ticket>.md candidate, which a
// request-driven run never has, and strict never marks that check
// advisory the way -preflight-profile=brownfield does (see
// evaluateProjectBootstrapChecks' own doc comment) -- so strict and the
// request pipeline could never both pass. -request-ticket (set by
// QueueEntry.RequestTicket, never by an operator) instead validates -spec
// itself, in the request pipeline's own ticketspec format, and never
// resolves a repo-native ticket at all.
func TestIntegrationRequestTicketPreflightValidatesSpecFile(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	// Deliberately no spec/tickets/ directory anywhere near ws: the exact
	// live-bug shape (a strict-profile repo that has never adopted the
	// pi-harness-native ticket convention at all).
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(wellFormedRequestTicketspec), 0o644); err != nil {
		t.Fatalf("write request ticketspec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001-fixture",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-request-ticket",
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("-request-ticket run against a strict-profile repo with no spec/tickets/: %v: %s", err, out)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one run record: entries=%v err=%v; output=%s", entries, err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "runs", entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	var result run.Run
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode run record: %v", err)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q; output=%s", result.State, run.StateAccepted, out)
	}
}

// TestIntegrationRequestTicketPreflightFailsClosedOnMalformedSpec proves
// -request-ticket still fails closed: a -spec missing the required
// Verify-Command: header (policy.TicketStructureBrownfield) halts the run
// before build_app.py ever starts, the same as a malformed repo-native
// ticket does for an unmarked run
// (TestIntegrationPiTicketStructurePreflightHaltsBeforeBuild above).
func TestIntegrationRequestTicketPreflightFailsClosedOnMalformedSpec(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	malformed := strings.Replace(wellFormedRequestTicketspec, "Verify-Command: true\n", "", 1)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(malformed), 0o644); err != nil {
		t.Fatalf("write malformed request ticketspec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "001-fixture",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
		"-request-ticket",
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("malformed -request-ticket spec unexpectedly allowed run: %s", out)
	}
	if !strings.Contains(string(out), "ticket_structure") {
		t.Fatalf("output = %q, want ticket_structure preflight failure", out)
	}
	if strings.Contains(string(out), "fake_build_app: mode=") {
		t.Fatal("build_app ran despite malformed -request-ticket spec")
	}
}

// TestIntegrationIsolateWorkspaceDefaultsOnWithoutFlag confirms
// isolation's default (converted from opt-in to required,
// 2026-08-29) actually applies when a caller passes no isolation flag at
// all — not just when isolation is passed explicitly, which every
// other isolation test in this file does.
func TestIntegrationIsolateWorkspaceDefaultsOnWithoutFlag(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil)

	if r.Branch == "" || r.WorkspacePath == ws {
		t.Fatalf("run did not execute isolated by default: branch=%q workspacePath=%q workspace=%q", r.Branch, r.WorkspacePath, ws)
	}
}

// fakeSandboxDockerBinary resolves testdata/fake_docker.sh's absolute path
// -- the fixture "docker" every non-live sandboxed test in this package
// now launches through instead of a real Docker daemon, since sandboxing
// is unconditional (no -allow-unsandboxed exists anymore).
func fakeSandboxDockerBinary(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("abs fake docker path: %v", err)
	}
	return path
}

// runFactoryd runs the built binary against a fixture workspace with the
// given fake-build-app mode and verify command, and returns the resulting
// durable run record. It never asserts factoryd's own exit code — a
// halted run is a valid, expected outcome for some scenarios.
func runFactoryd(t *testing.T, workspace, mode, verifyCommand string) *run.Run {
	t.Helper()
	return runFactorydWithSpec(t, workspace, mode, verifyCommand, "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s")
}

// runFactorydWithSpec is runFactoryd with control over the ticket spec's
// content and the supervisor timeout — used by the spec cross-reference
// test to declare a Verify-Command key, and by the timeout test to force
// a build attempt to be killed quickly.
func runFactorydWithSpec(t *testing.T, workspace, mode, verifyCommand, specContent, timeout string) *run.Run {
	t.Helper()
	return runFactorydWithSpecAndEnv(t, workspace, mode, verifyCommand, specContent, timeout, nil)
}

// runFactorydWithSpecAndEnv is runFactorydWithSpec with additional
// environment variables set on the factoryd subprocess — used by the
// Discord notification test to point FACTORYD_DISCORD_WEBHOOK_URLS at a
// fixture HTTP server.
func runFactorydWithSpecAndEnv(t *testing.T, workspace, mode, verifyCommand, specContent, timeout string, extraEnv []string) *run.Run {
	t.Helper()
	return runFactorydWithSpecAndFlags(t, workspace, mode, verifyCommand, specContent, timeout, extraEnv, nil)
}

// runFactorydWithSpecAndFlags is runFactorydWithSpecAndEnv with additional
// CLI flags passed to the factoryd subprocess — used by tests that need to
// pass flags like -require-declared-scope. extraEnv and extraFlags are both
// optional (may be nil).
func runFactorydWithSpecAndFlags(t *testing.T, workspace, mode, verifyCommand, specContent, timeout string, extraEnv, extraFlags []string) *run.Run {
	t.Helper()
	return runFactorydWithSpecFlagsAndDataDir(t, workspace, mode, verifyCommand, specContent, timeout, extraEnv, extraFlags, t.TempDir())
}

// runFactorydWithSpecFlagsAndDataDir is runFactorydWithSpecAndFlags with
// control over -data-dir itself — used by tests that need two sequential
// runs to share one data dir (e.g. a slice-chain test whose second run
// passes -prior-run pointing at the first run's id, which only resolves
// if both runs' durable records live under the same directory).
func runFactorydWithSpecFlagsAndDataDir(t *testing.T, workspace, mode, verifyCommand, specContent, timeout string, extraEnv, extraFlags []string, dataDir string) *run.Run {
	t.Helper()

	// The spec lives outside the workspace, matching real usage (ticket
	// specs live in this repo's data/tickets/, not inside the target
	// workspace being modified) — putting it inside the workspace would
	// let `git add -A` sweep it into the agent's own commit and corrupt
	// the changed-file/diff-size evidence this test and others check.
	//
	// tests_added always runs, and testdata/fake_build_app.sh's
	// fixtures don't add a real *_test.go file — appended here, the one
	// chokepoint every runFactorydWithSpec* helper routes through, rather
	// than in each of this suite's many callers, so none of them (none of
	// which are testing tests_added itself) has to know that gate exists.
	// A caller whose own specContent already declares Tests-Required:
	// keeps its own declaration -- ticketspec's parseTopLevelKeyLine
	// returns only the first occurrence of a key, so this appended line
	// never overrides one already present.
	if !strings.Contains(specContent, "Tests-Required:") {
		specContent += "\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	ticket := "fixture-ticket"
	// Sandboxing is unconditional (no -allow-unsandboxed exists anymore),
	// so this whole file's non-live suite sandboxes through
	// testdata/fake_docker.sh instead of a real Docker engine or the
	// canonical worker image -- see that script's own doc comment. Only
	// DOCKER_SANDBOX_LIVE=1 tests (a distinct set of files) are meant to
	// touch real Docker. A caller that specifically wants to exercise real
	// sandboxing appends its own -sandbox-image/-sandbox-docker to
	// extraFlags; flag parsing keeps whichever occurrence comes last.
	args := []string{
		"-ticket", ticket,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", workspace,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", timeout,
		"-verify-command", verifyCommand,
		"-data-dir", dataDir,
	}
	// Recorded before invocation, not just read after: a caller sharing one
	// dataDir across two sequential calls (a slice-chain test's second run,
	// declaring -prior-run against the first) would otherwise see 2 run
	// directories after this second call and have no way to tell which one
	// it just created.
	before, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read runs dir before invocation: %v", err)
	}
	existed := make(map[string]bool, len(before))
	for _, e := range before {
		existed[e.Name()] = true
	}

	args = append(args, extraFlags...)
	registerRepositoryOwnerCleanupFromArgs(t, extraFlags)
	cmd := factorydCommand(t, args...)
	cmd.Env = append(append(os.Environ(),
		"FAKE_BUILD_APP_MODE="+mode,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	), extraEnv...)
	out, _ := cmd.CombinedOutput() // exit code intentionally unchecked; see doc comment
	t.Logf("factoryd output:\n%s", out)

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	var newEntries []os.DirEntry
	for _, e := range entries {
		if !existed[e.Name()] {
			newEntries = append(newEntries, e)
		}
	}
	if len(newEntries) != 1 {
		t.Fatalf("expected exactly 1 new run directory, got %d (total in dir: %d): %v", len(newEntries), len(entries), entries)
	}

	b, err := os.ReadFile(filepath.Join(dataDir, "runs", newEntries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	return &r
}

// TestIntegrationRelativeDataDirResolvesSnapshotPath pins the fix for a
// finding from review: the spec snapshot path used to be built directly
// from -data-dir without resolving it to absolute. -data-dir defaults to
// the relative "data", and build_app.py runs with its working directory
// set to the *workspace*, not factoryd's own directory — so a relative
// snapshot path resolved underneath the workspace instead of where it was
// actually written, and every default-flag run halted before doing any
// work. This test deliberately runs factoryd from a harness directory
// distinct from the workspace, with a relative -data-dir, to reproduce
// exactly that mismatch.
func TestIntegrationRelativeDataDirResolvesSnapshotPath(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	harnessDir := t.TempDir() // deliberately not ws — reproduces the cwd/workspace split

	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", "data", // relative, and harnessDir != ws — the exact mismatch
	)
	cmd.Dir = harnessDir
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, _ := cmd.CombinedOutput()
	t.Logf("factoryd output:\n%s", out)

	entries, err := os.ReadDir(filepath.Join(harnessDir, "data", "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory, got %d: %v", len(entries), entries)
	}
	b, err := os.ReadFile(filepath.Join(harnessDir, "data", "runs", entries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (relative -data-dir with a different workspace cwd should still resolve the spec snapshot)", r.State, run.StateAccepted)
	}
}

func TestIntegrationServeListsDurableRuns(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	want := run.Run{
		ID:        "served-run",
		Ticket:    "served-ticket",
		State:     run.StateAccepted,
		CreatedAt: "2026-08-26T10:00:00Z",
		UpdatedAt: "2026-08-26T10:01:00Z",
	}
	if err := want.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	cmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	var response *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get("http://" + addr + "/runs")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /runs: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var got []run.Run
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].ID != want.ID || got[0].Ticket != want.Ticket || got[0].State != want.State {
		t.Fatalf("runs = %+v, want the seeded durable run", got)
	}
}

// TestIntegrationServeShutsDownGracefullyOnSIGTERM is the regression test
// for a real P2 finding from codex review: factoryd serve previously had no
// signal handling of its own at all, while an embedded API-started run
// installed its own signal.NotifyContext for SIGTERM inside
// runViaRepositoryOwner. That per-run registration globally suppressed the
// OS's default terminate-on-SIGTERM behavior for as long as any run was in
// flight, so an operator SIGTERM sent to the server could be silently
// absorbed by whichever run happened to be active (canceling that one run)
// while the HTTP server itself kept running with no idea a signal ever
// arrived. serveMain now owns SIGINT/SIGTERM itself and performs a graceful
// http.Server.Shutdown; embedded runs no longer install a competing
// handler. This proves the server process actually exits (not force-killed)
// within a bounded time after SIGTERM, which the pre-fix code could not do
// reliably.
func TestIntegrationServeShutsDownGracefullyOnSIGTERM(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	cmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Get("http://" + addr + "/healthz"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("factoryd serve did not exit cleanly after SIGTERM: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("factoryd serve did not exit within 5s of SIGTERM (found via review: it previously had no signal handling of its own)")
	}
}

// TestIntegrationServeDrainsAPIStartedRunBeforeExiting is the regression
// test for a real P1 finding from codex review of PR #17: an API-started
// run is deliberately detached onto context.Background() (see
// apiStartStarter's own doc comment — it must outlive the HTTP request
// that started it), so by the time server.Shutdown runs, that run's own
// POST handler has already returned and http.Server itself has nothing
// left to wait for. Before the fix, serveMain returned as soon as
// Shutdown did, regardless of whether the run it just reported as
// "started" was still actually executing — silently abandoning its
// Temporal worker/subprocess supervision mid-flight despite logging a
// graceful shutdown.
//
// This starts a run whose build_app.py is made to run for
// longer than an instant (FAKE_BUILD_APP_MODE=hang, bounded by the run's
// own short -timeout so the test itself stays fast), confirms it is
// still non-terminal, then sends SIGTERM and measures how long the
// process actually takes to exit: too fast unambiguously means it did
// NOT wait for the run, and a terminal run.json once it does exit is the
// direct proof that it did.
func TestIntegrationServeDrainsAPIStartedRunBeforeExiting(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel(); this test also
	// depends on a real, shared Temporal server and precise SIGTERM-drain
	// timing.
	// repositoryAPIStarter requires every API-started run to declare
	// -repository/-temporal_address (see its own doc comment) — routing
	// through a real repository owner Workflow is what actually makes the
	// run's own Temporal worker/subprocess supervision the thing this
	// test needs SIGTERM to race against.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	writeNativeTicketForAPI(t, ws, "fixture-drain-ticket")
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	const startToken = "fixture-start-token"
	cmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr, "-api-allowed-sandbox-images", fakeSandboxImage)
	cmd.Env = append(os.Environ(),
		"FACTORYD_API_START_TOKEN="+startToken,
		"FAKE_BUILD_APP_MODE=hang",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output synchronizedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	processWaited := false
	t.Cleanup(func() {
		if !processWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		t.Logf("serve output:\n%s", output.String())
	})

	httpClient := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, getErr := httpClient.Get("http://" + addr + "/healthz"); getErr == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	repository := fmt.Sprintf("fixture/repo-drain-%d", time.Now().UnixNano())
	terminateRepositoryOwnerAtCleanup(t, address, workflow.RepositoryOwnerWorkflowID(repository))
	body, err := json.Marshal(api.StartRequest{
		Ticket:              "fixture-drain-ticket",
		Workspace:           ws,
		Spec:                specPath,
		SandboxImage:        fakeSandboxImage,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		// Sandboxing is unconditional and reserves a fixed teardown margin
		// (sandboxAttemptTeardownMargin, 10s) off this run's own deadline
		// before it will even start an attempt -- long enough to still be
		// short/fast for this test, per its own doc comment.
		Timeout:         "20s",
		VerifyCommand:   "true",
		Repository:      repository,
		TemporalAddress: address,
	})
	if err != nil {
		t.Fatalf("marshal start request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/runs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build start request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+startToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read start response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /runs status = %d, body=%s (output so far:\n%s)", resp.StatusCode, respBody, output.String())
	}
	var started run.Run
	if err := json.Unmarshal(respBody, &started); err != nil {
		t.Fatalf("decode start response: %v (body=%s)", err, respBody)
	}
	if started.ID == "" {
		t.Fatalf("start response has no run ID: %+v", started)
	}

	// Confirm the run is genuinely non-terminal (still executing) before
	// this test ever signals the process — otherwise a fast completion
	// would make the timing assertion below vacuous.
	loaded, loadErr := run.Load(dataDir, started.ID)
	if loadErr != nil {
		t.Fatalf("load just-started run.json: %v", loadErr)
	}
	if loaded.State == run.StateAccepted || loaded.State == run.StateHalted || loaded.State == run.StateQuarantined {
		t.Fatalf("run %s already terminal (%s) immediately after starting — test fixture too fast to prove draining", started.ID, loaded.State)
	}

	sigTermSentAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		processWaited = true
		if err != nil {
			t.Fatalf("factoryd serve did not exit cleanly after SIGTERM: %v (output:\n%s)", err, output.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("factoryd serve did not exit within 60s of SIGTERM (output:\n%s)", output.String())
	}
	elapsed := time.Since(sigTermSentAt)

	// The run's own -timeout is 2s; exiting well before that elapsed
	// unambiguously means serveMain did not wait for it — the exact bug
	// this test guards against. 1s is comfortably below 2s while still
	// tolerant of scheduling noise.
	if elapsed < time.Second {
		t.Fatalf("factoryd serve exited only %s after SIGTERM, well before the in-flight run's own 2s timeout — it did not drain the run before exiting", elapsed)
	}

	reconciled, err := run.Load(dataDir, started.ID)
	if err != nil {
		t.Fatalf("load run.json after serve exited: %v", err)
	}
	if reconciled.State != run.StateHalted {
		t.Fatalf("run.json state = %q after serve exited, want %q — serveMain must not exit while an API-started run is still in flight", reconciled.State, run.StateHalted)
	}
}

// TestIntegrationAPIStartedRunUsesServerConfiguredReleasePolicy is the
// regression test for a real finding from a local codex review pass on
// this same PR: `factoryd serve`'s own -release-* flags previously
// configured only internal/api.Server's override endpoint (via
// api.WithReleasePolicy) -- apiStartStarter's spawned `factoryd <run>`
// subprocess never received them, so an ordinarily accepted API-started
// run always recorded its release decision against runMainWithReady's
// restrictive zero-value policy regardless of what the operator actually
// configured on `serve`. Starts `serve` with a permissive policy (a
// generous file/insertion limit, a declared rollback plan, and
// -release-allow-unsandboxed on `serve`'s own flag -- PR #50's
// release.MergePolicy.AllowUnsandboxed gate, unrelated to Docker
// containment itself, would otherwise deny this run for reasons
// independent of whatever this test is actually proving), submits a run
// through POST /runs (sandboxed through this package's own fake docker --
// see TestMain), and proves the recorded
// decision is actually Allowed: true, which only happens if the
// configured policy genuinely reached the subprocess. repositoryAPIStarter
// requires every
// API-started run to declare -repository/-temporal-address (see its own
// doc comment), so this needs a live Temporal server the same way
// TestIntegrationServeDrainsAPIStartedRunBeforeExiting does.
func TestIntegrationAPIStartedRunUsesServerConfiguredReleasePolicy(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel(); this test also
	// depends on a real, shared Temporal server.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	writeNativeTicketForAPI(t, ws, "fixture-release-policy-ticket")
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	// Declares the full Allowed-Files/Required-Changed-Files/
	// Required-Content set, and this test's own FAKE_BUILD_APP_MODE below
	// is "commit_with_marker" (not the bare "commit" this test used to
	// use) and its StartRequest below sets FullSuiteCommand -- together
	// making every one of policy.AllGateChecks actually run and pass, not
	// just canonical_verify. Needed since release.MergePolicy.
	// RequiredGates now defaults to the complete gate list (2026-09-05
	// Opus review, S1): a decision this test expects Allowed to require
	// every gate to have a real passing result, not merely "none
	// recorded failed".
	specContent := "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n" +
		"Allowed-Files: content.txt\n" +
		"Required-Changed-Files: content.txt\n" +
		"Required-Content: REQUIRED_MARKER_STRING\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	const startToken = "fixture-start-token"
	cmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr,
		"-api-allowed-sandbox-images", fakeSandboxImage,
		"-release-max-files-changed", "10",
		"-release-max-insertions", "100",
		"-release-rollback-plan", "reviewed and reversible",
		// release.MergePolicy's own, separate AllowUnsandboxed gate
		// (restrictive by default, added by the 2026-09-04 Phase 5
		// hardening pass) would otherwise deny the run's release decision
		// regardless of the permissive settings above, which is what this
		// test actually means to check propagated -- unrelated to Docker
		// containment itself (which is unconditional, with no opt-out of
		// its own to gate here anymore).
		"-release-allow-unsandboxed",
	)
	cmd.Env = append(os.Environ(),
		"FACTORYD_API_START_TOKEN="+startToken,
		// commit_with_marker, not the bare commit mode: this test's spec
		// above now declares Required-Content: REQUIRED_MARKER_STRING,
		// which only this mode's fake_build_app.sh actually writes.
		"FAKE_BUILD_APP_MODE=commit_with_marker",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output synchronizedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Logf("serve output:\n%s", output.String())
	})

	httpClient := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, getErr := httpClient.Get("http://" + addr + "/healthz"); getErr == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	repository := fmt.Sprintf("fixture/repo-release-policy-%d", time.Now().UnixNano())
	terminateRepositoryOwnerAtCleanup(t, address, workflow.RepositoryOwnerWorkflowID(repository))
	body, err := json.Marshal(api.StartRequest{
		Ticket:              "fixture-release-policy-ticket",
		Workspace:           ws,
		Spec:                specPath,
		SandboxImage:        fakeSandboxImage,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		Timeout:             "30s",
		VerifyCommand:       "true",
		Repository:          repository,
		TemporalAddress:     address,
		// So full_suite_verify actually runs and passes -- needed for
		// this test's own decision.Allowed=true expectation now that
		// release.MergePolicy.RequiredGates defaults to the complete gate
		// list (2026-09-05 Opus review, S1).
		FullSuiteCommand: "true",
	})
	if err != nil {
		t.Fatalf("marshal start request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/runs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build start request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+startToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read start response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /runs status = %d, body=%s (output so far:\n%s)", resp.StatusCode, respBody, output.String())
	}
	var started run.Run
	if err := json.Unmarshal(respBody, &started); err != nil {
		t.Fatalf("decode start response: %v (body=%s)", err, respBody)
	}
	if started.ID == "" {
		t.Fatalf("start response has no run ID: %+v", started)
	}

	deadline = time.Now().Add(10 * time.Second)
	var loaded *run.Run
	for time.Now().Before(deadline) {
		loaded, err = run.Load(dataDir, started.ID)
		if err == nil && loaded.State == run.StateAccepted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if loaded == nil || loaded.State != run.StateAccepted {
		t.Fatalf("run did not reach accepted (output:\n%s)", output.String())
	}

	// recordReleaseDecision runs after r's own terminal state is already
	// durably saved (see CLAIMS.md's Phase 7 gap register entry, and this
	// codebase's deliberate single-save discipline generally) -- so State
	// == StateAccepted becoming visible above is not itself proof the
	// decision file has been written yet. Found live on a real CI runner,
	// 2026-09-05: the poll above returned the instant state flipped to
	// accepted, racing recordReleaseDecision's own separate write and
	// intermittently reading "no such file" even though the run had
	// genuinely succeeded. Poll for the decision file explicitly instead
	// of assuming it's already there.
	project := release.ProjectFromWorkspace(ws)
	decisionPath := filepath.Join(dataDir, "projects", project, "release-decisions", started.ID+".json")
	var b []byte
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(decisionPath)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("read release decision %s: %v (output:\n%s)", decisionPath, err, output.String())
	}
	var decision release.Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("unmarshal release decision: %v", err)
	}
	if !decision.Allowed {
		t.Errorf("decision.Allowed = false, reasons=%v -- want true: the server's own permissive -release-* configuration must have reached the API-started subprocess, not runMain's restrictive zero-value defaults", decision.Reasons)
	}
}

// TestIntegrationRunRefusesOverrideTokenInEnvironment is the regression
// test for a real P1 finding from codex review: internal/runner's own
// subprocessEnv() strips FACTORYD_API_OVERRIDE_TOKEN from a launched
// build_app.py/canonical-verify subprocess's inherited environment, but
// that alone doesn't isolate the credential from an untrusted subprocess
// determined to read it — same-UID process inspection (e.g.
// /proc/<pid>/environ on Linux) can recover it directly from the *run*
// process's own environment regardless of what env its child is launched
// with. There is no code-level fix once the credential is already
// present there; a run invocation must refuse to proceed at all rather
// than silently run with it present, so a misconfigured shared
// environment (the token meant only for `factoryd serve`) is loud, not
// latent. Proves the guard fires before ever creating a run directory —
// not merely that the process exits non-zero for some other reason.
func TestIntegrationRunRefusesOverrideTokenInEnvironment(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"FACTORYD_API_OVERRIDE_TOKEN=leaked-secret-value",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd exited 0 with the override token set in its own environment; output:\n%s", out)
	}
	if !strings.Contains(string(out), "FACTORYD_API_OVERRIDE_TOKEN") {
		t.Fatalf("output = %q, want it to name the misconfigured variable", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runs")); !os.IsNotExist(err) {
		t.Fatalf("runs dir stat = %v, want it to not exist — the guard must fire before ever creating one", err)
	}
}
