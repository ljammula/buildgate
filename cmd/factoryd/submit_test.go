package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/testfixture"
)

// writeFakeGH writes a fake `gh` executable at dir/fake-gh that prints
// script's output on stdout and exits 0 -- the same fake-binary-injection
// pattern internal/forge's own tests use for git/gh, kept local here since
// each test's own fixture output is small and specific to it.
func writeFakeGH(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return path
}

// TestSubmitMainWritesRequest is submit's end-to-end test: it must write a
// request.md and request.json under -data-dir/requests/<id>/ and return,
// without invoking build_app.py or anything else. There is nothing in
// submitMain itself capable of starting a run (it never calls
// runMainWithReady or any injectable equivalent) -- this test's own
// assertions on the written request are what proves that: nothing about
// worker's own drain behavior is exercised here.
func TestSubmitMainWritesRequest(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Add idempotency keys to POST /refunds"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	req := requests[0]

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if req.Workspace != absWorkspace {
		t.Errorf("req.Workspace = %q, want %q", req.Workspace, absWorkspace)
	}
	if req.State != request.StateSubmitted {
		t.Errorf("req.State = %q, want %q", req.State, request.StateSubmitted)
	}
	if req.Source.Kind != request.SourceText {
		t.Errorf("req.Source.Kind = %q, want %q", req.Source.Kind, request.SourceText)
	}
	if req.SubmittedAt == "" {
		t.Error("req.SubmittedAt is empty")
	}
	if req.VerifyCommand != "make ci-verify" {
		t.Errorf("req.VerifyCommand = %q, want %q (from the workspace's .factory.yml)", req.VerifyCommand, "make ci-verify")
	}

	textBytes, err := os.ReadFile(request.TextPath(dataDir, req.ID))
	if err != nil {
		t.Fatalf("read request text: %v", err)
	}
	if !strings.Contains(string(textBytes), "Add idempotency keys to POST /refunds") {
		t.Errorf("request.md does not contain the request text verbatim: %q", string(textBytes))
	}
}

// TestSubmitMainRecordsExplicitVerifyCommandFlag pins the live bug this
// guards against: a workspace with no .factory.yml at all (so
// applyProjectConfigDefaults has nothing to fall back to), submitted with
// -verify-command passed explicitly. Before req.VerifyCommand existed,
// this resolved value was validated at submit time and then discarded --
// planning later re-read .factory.yml, found nothing, and halted with
// "planning requires verify_command configured in .factory.yml" even
// though the operator had supplied one at submit time.
func TestSubmitMainRecordsExplicitVerifyCommandFlag(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	dataDir := t.TempDir()
	testfixture.CommitAgentsFile(t, workspace)

	// -preflight-profile brownfield: this fixture workspace has no
	// spec/spec.md/contract.md/ARCHITECTURE.md, unrelated to what this
	// test actually exercises (verify-command resolution).
	if err := submitMain(dp, []string{"-verify-command", "python3 -m unittest tests/test_product_lab.py", "-preflight-profile", "brownfield", "-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	if got, want := requests[0].VerifyCommand, "python3 -m unittest tests/test_product_lab.py"; got != want {
		t.Errorf("req.VerifyCommand = %q, want %q (the explicit -verify-command flag)", got, want)
	}
}

// TestSubmitMainRecordsExplicitPreflightProfileFlag mirrors
// TestSubmitMainRecordsExplicitVerifyCommandFlag for -preflight-profile:
// the same live bug (validated at submit time, then discarded) applied
// to this flag too -- a ticket's build later fell back to the strict
// default profile and halted a brownfield workspace's preflight on
// artifacts convention was never asked to produce.
func TestSubmitMainRecordsExplicitPreflightProfileFlag(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-preflight-profile", "brownfield", "-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	if got, want := requests[0].PreflightProfile, "brownfield"; got != want {
		t.Errorf("req.PreflightProfile = %q, want %q (the explicit -preflight-profile flag)", got, want)
	}
}

// TestSubmitMainRecordsFullSuiteCommandFlag: the default release policy
// requires the full_suite_verify gate, and the request path had no per-
// request way to configure one, so a repository without a committed
// .factory.yml full_suite_command could never open a pull request (found
// live, 2026-09-19).
func TestSubmitMainRecordsFullSuiteCommandFlag(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-full-suite-command", "cd backend && go test ./...", "-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("List = %v, %v; want one request", requests, err)
	}
	if got, want := requests[0].FullSuiteCommand, "cd backend && go test ./..."; got != want {
		t.Errorf("req.FullSuiteCommand = %q, want %q", got, want)
	}
}

// TestSubmitDefaultsFullSuiteToVerifyCommand is the regression test for
// submit.go's own half of the deny-all release policy gap: a
// request submitted with no -full-suite-command and no .factory.yml
// full_suite_command must not leave req.FullSuiteCommand empty forever
// (denying every PR -- see internal/release.MergePolicy's own
// RequiredGates comment) -- it gets the request's own resolved verify
// command instead, recorded as a substitution via req.FullSuiteSource.
func TestSubmitDefaultsFullSuiteToVerifyCommand(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("List = %v, %v; want one request", requests, err)
	}
	req := requests[0]
	if req.FullSuiteCommand != "make ci-verify" {
		t.Errorf("req.FullSuiteCommand = %q, want the substituted verify command %q", req.FullSuiteCommand, "make ci-verify")
	}
	if req.FullSuiteSource != fullSuiteSourceVerifyCommand {
		t.Errorf("req.FullSuiteSource = %q, want %q", req.FullSuiteSource, fullSuiteSourceVerifyCommand)
	}
}

// TestSubmitFullSuiteCommandNoneOptsOut proves the explicit
// `-full-suite-command none` opt-out is recorded as such and
// never substituted -- req.FullSuiteCommand stays empty (not the literal
// "none", which would otherwise reach a shell as a nonsense command) and
// req.FullSuiteSource records the opt-out explicitly, distinguishing it
// from a request that simply predates this field.
func TestSubmitFullSuiteCommandNoneOptsOut(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-full-suite-command", "none", "-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("List = %v, %v; want one request", requests, err)
	}
	req := requests[0]
	if req.FullSuiteCommand != "" {
		t.Errorf("req.FullSuiteCommand = %q, want empty under the none opt-out", req.FullSuiteCommand)
	}
	if req.FullSuiteSource != fullSuiteSourceNone {
		t.Errorf("req.FullSuiteSource = %q, want %q", req.FullSuiteSource, fullSuiteSourceNone)
	}
}

// TestSubmitMainRecordsNoCommitOraclesFlag: the opt-out is explicit and
// recorded on the request; absent, it stays false.
func TestSubmitMainRecordsNoCommitOraclesFlag(t *testing.T) {
	dp := newTestDeps(t)
	for _, tc := range []struct {
		args []string
		want bool
	}{{[]string{"-no-commit-oracles"}, true}, {nil, false}} {
		workspace := t.TempDir()
		writeTestSubmitRepo(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
		dataDir := t.TempDir()
		args := append(append([]string{}, tc.args...), "-data-dir", dataDir, workspace, "Add a new func")
		if err := submitMain(dp, args); err != nil {
			t.Fatalf("submitMain: %v", err)
		}
		requests, err := request.List(dataDir)
		if err != nil || len(requests) != 1 {
			t.Fatalf("List = %v, %v; want one request", requests, err)
		}
		if got := requests[0].NoCommitOracles; got != tc.want {
			t.Errorf("args %v: NoCommitOracles = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestSubmitMainRecordsDraftOraclesFlag: -draft-oracles opts one request into
// the staged oracle stage; without it the field stays false (and, being
// omitempty, absent from request.json).
func TestSubmitMainRecordsDraftOraclesFlag(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"-draft-oracles"}, true},
		{nil, false},
	} {
		dataDir := t.TempDir()
		args := append(append([]string{}, tc.args...), "-data-dir", dataDir, workspace, "Add a new func")
		if err := submitMain(dp, args); err != nil {
			t.Fatalf("submitMain(%v): %v", tc.args, err)
		}
		requests, err := request.List(dataDir)
		if err != nil || len(requests) != 1 {
			t.Fatalf("List = %v, %v; want one request", requests, err)
		}
		if requests[0].DraftOracles != tc.want {
			t.Errorf("args %v: DraftOracles = %v, want %v", tc.args, requests[0].DraftOracles, tc.want)
		}
	}
}

// routesModeConfigWithAllowedSonnet writes a routes:/models:/roles:
// session config file with roles.execution.allowed naming both "luna"
// (the default) and "sonnet", and returns its path -- the fixture every
// `-model` test below submits against.
func routesModeConfigWithAllowedSonnet(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	content := "routes:\n  litellm:\n    credential_mode: static\n    upstream: https://litellm.example.invalid\n    credential_env: SUBMIT_MODEL_TEST_KEY\n" +
		"models:\n  luna:\n    id: gpt-5.6-luna\n    routes: [litellm]\n  sonnet:\n    id: sonnet-4\n    routes: [litellm]\n" +
		"roles:\n  execution:\n    model: luna\n    allowed: [luna, sonnet]\n    allowed_harnesses: [pi, pifork]\n  planning:\n    model: luna\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestSubmitMainModelFlagAcceptsAllowedChoice proves `factoryd submit
// -model execution=sonnet` records the choice on the request once
// sonnet is a member of roles.execution.allowed.
func TestSubmitMainModelFlagAcceptsAllowedChoice(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := routesModeConfigWithAllowedSonnet(t)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-config", configPath, "-model", "execution=sonnet", "-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain -model execution=sonnet: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("List = %v, %v; want one request", requests, err)
	}
	if got, want := requests[0].Models["execution"], "sonnet"; got != want {
		t.Errorf("req.Models[execution] = %q, want %q", got, want)
	}
}

// TestSubmitMainModelFlagRejectsModelOutsideAllowed proves a choice
// outside roles.execution.allowed refuses the whole submission, writing
// no request.
func TestSubmitMainModelFlagRejectsModelOutsideAllowed(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := routesModeConfigWithAllowedSonnet(t)
	dataDir := t.TempDir()

	err := submitMain(dp, []string{"-config", configPath, "-model", "execution=opus", "-data-dir", dataDir, workspace, "Add a new func"})
	if err == nil {
		t.Fatal("submitMain -model execution=opus: err = nil, want a refusal (opus is not in roles.execution.allowed)")
	}
	if !strings.Contains(err.Error(), "opus") || !strings.Contains(err.Error(), "roles.execution.allowed") {
		t.Errorf("err = %v, want it to name the rejected model and roles.execution.allowed", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 0 {
		t.Fatalf("List = %v, %v; want zero requests written after a refused submission", requests, err)
	}
}

// TestSubmitMainModelFlagRejectsReviewRole proves review is never
// requester-selectable, even when a review role is configured.
func TestSubmitMainModelFlagRejectsReviewRole(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := routesModeConfigWithAllowedSonnet(t)
	dataDir := t.TempDir()

	err := submitMain(dp, []string{"-config", configPath, "-model", "review=sonnet", "-data-dir", dataDir, workspace, "Add a new func"})
	if err == nil {
		t.Fatal("submitMain -model review=sonnet: err = nil, want a refusal -- review is never requester-selectable")
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("err = %v, want it to name the rejected role", err)
	}
}

// TestSubmitMainModelFlagRejectsMalformedPair proves a `-model` value
// with no "role=model" shape is refused as a bad flag value, not
// silently ignored.
func TestSubmitMainModelFlagRejectsMalformedPair(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := routesModeConfigWithAllowedSonnet(t)

	err := submitMain(dp, []string{"-config", configPath, "-model", "sonnet", "-data-dir", t.TempDir(), workspace, "Add a new func"})
	if err == nil {
		t.Fatal("submitMain -model sonnet: err = nil, want a refusal for a malformed role=model pair")
	}
	if !strings.Contains(err.Error(), "-model") {
		t.Errorf("err = %v, want it to name -model", err)
	}
}

// TestSubmitMainModelFlagRejectedInLegacyMode proves `-model` is refused
// outright with no routes:/models:/roles: session config at all -- there
// is no roles.<role>.allowed to validate the choice against.
func TestSubmitMainModelFlagRejectedInLegacyMode(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	err := submitMain(dp, []string{"-model", "execution=sonnet", "-data-dir", t.TempDir(), workspace, "Add a new func"})
	if err == nil {
		t.Fatal("submitMain -model execution=sonnet with no session config: err = nil, want a refusal")
	}
}

// TestSubmitMainRefusesInvalidRolesBlock proves submit's own startup path
// validates roles: once, explicitly -- an unknown model_aliases entry
// named by a role must refuse the whole invocation before any request is
// ever written.
func TestSubmitMainRefusesInvalidRolesBlock(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("roles:\n  execution:\n    model: does-not-exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	err := submitMain(dp, []string{"-config", configPath, "-data-dir", dataDir, workspace, "Add a new func"})
	if err == nil || !strings.Contains(err.Error(), "roles.execution") {
		t.Fatalf("submitMain with an invalid roles: block: err = %v, want it to name roles.execution", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 0 {
		t.Fatalf("List = %v, %v; want zero requests written after a refused submission", requests, err)
	}
}

// TestSubmitMainRequiresResolvableVerifyCommand exercises the clear-error
// path when neither -verify-command nor .factory.yml supply one --
// submit still validates this up front (see submitMain's own doc comment
// on applyProjectConfigDefaults) even though the value is not persisted
// on the request record.
func TestSubmitMainRequiresResolvableVerifyCommand(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	dataDir := t.TempDir()

	err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Do something"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "verify command") {
		t.Errorf("error = %v, want it to mention a verify command", err)
	}

	requests, listErr := request.List(dataDir)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(requests) != 0 {
		t.Errorf("len(requests) = %d, want 0 -- nothing should be recorded on error", len(requests))
	}
}

// TestSubmitMainRejectsNonExistentWorkspace pins the mistyped-path case:
// projectconfig.Load reports a missing directory as "no config" rather
// than an error, so before this check a typo surfaced as "no verify
// command resolvable" -- or, with -verify-command passed explicitly,
// recorded cleanly and only failed later inside a run.
func TestSubmitMainRejectsNonExistentWorkspace(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "no-such-repo")

	err := submitMain(dp, []string{"-data-dir", dataDir, "-verify-command", "make verify", missing, "do a thing"})
	if err == nil {
		t.Fatal("submitMain with a non-existent workspace = nil, want an error")
	}
	if !strings.Contains(err.Error(), "no-such-repo") {
		t.Fatalf("err = %v, want it to name the workspace that does not exist", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "requests")); statErr == nil {
		t.Fatal("submitMain recorded a request for a non-existent workspace")
	}
}

// TestSubmitMainRejectsWorkspaceThatIsAFile covers the other half of the
// same check: a path that exists but is not a directory.
func TestSubmitMainRejectsWorkspaceThatIsAFile(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	file := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := submitMain(dp, []string{"-data-dir", dataDir, "-verify-command", "make verify", file, "do a thing"})
	if err == nil {
		t.Fatal("submitMain with a file as the workspace = nil, want an error")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("err = %v, want it to say the workspace is not a directory", err)
	}
}

// TestSubmitMainRefusesDataDirInsideWorkspace pins issue #157 Problem 2:
// submitting with cwd inside the workspace, no session config and no explicit
// -data-dir uses the literal "data" default, which then resolves under the
// workspace itself (a session config always names a data dir outside it). A queue entry submitted there is unrecoverable the moment a
// later worker refuses to drain it (run_ticket.go's dataDirInsideWorkspace
// guard fires unconditionally on this condition), so submit must refuse
// this up front instead, mirroring that same guard rather than a warning
// easy to miss in submit's terse output.
func TestSubmitMainRefusesDataDirInsideWorkspace(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make verify"
`)

	t.Chdir(workspace)

	err := submitMain(dp, []string{".", "do a thing"})
	if err == nil {
		t.Fatal("submitMain with a workspace-relative default -data-dir = nil, want an error")
	}
	if !strings.Contains(err.Error(), "-data-dir") || !strings.Contains(err.Error(), "-workspace") {
		t.Fatalf("err = %v, want it to name -data-dir and -workspace", err)
	}
	// The whole default -data-dir must be absent, not just requests/: the
	// refusal has to run before release.RejectProjectCollision, whose own
	// MkdirAll would otherwise still leave <workspace>/data/projects/ behind.
	if _, statErr := os.Stat(filepath.Join(workspace, "data")); !os.IsNotExist(statErr) {
		t.Fatalf("submitMain created the in-workspace default -data-dir despite refusing (stat err = %v)", statErr)
	}
}

// TestSubmitMainRequestFileAndInlineTextAreMutuallyExclusive covers
// -request-file's own documented interaction with a trailing inline
// request argument.
func TestSubmitMainRequestFileAndInlineTextAreMutuallyExclusive(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make verify"
`)
	dataDir := t.TempDir()
	requestFile := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(requestFile, []byte("from file"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := submitMain(dp, []string{"-data-dir", dataDir, "-request-file", requestFile, workspace, "also inline"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want it to mention mutual exclusivity", err)
	}
}

// TestSubmitMainRequestFile covers reading the request text from a file
// instead of the trailing command-line argument, and that the source kind
// is recorded as "file".
func TestSubmitMainRequestFile(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()
	requestFile := filepath.Join(t.TempDir(), "request.txt")
	if err := os.WriteFile(requestFile, []byte("Reading-time endpoint, see issue #42\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := submitMain(dp, []string{"-data-dir", dataDir, "-request-file", requestFile, workspace}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	if requests[0].Source.Kind != request.SourceFile {
		t.Errorf("Source.Kind = %q, want %q", requests[0].Source.Kind, request.SourceFile)
	}
	textBytes, err := os.ReadFile(request.TextPath(dataDir, requests[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(textBytes), "Reading-time endpoint, see issue #42") {
		t.Errorf("request.md does not contain the file's request text: %q", string(textBytes))
	}
}

// TestGHIssueFetcherFetchSuccess covers the successful path: gh's own
// stdout JSON is parsed into title/body/number.
func TestGHIssueFetcherFetchSuccess(t *testing.T) {
	dir := t.TempDir()
	ghBinary := writeFakeGH(t, dir, `echo '{"title":"Reading-time endpoint","body":"See the spec for details.","number":42}'
`)
	f := ghIssueFetcher{GHBinary: ghBinary}
	title, body, number, err := f.fetch(context.Background(), "https://github.com/acme/widgets/issues/42")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if title != "Reading-time endpoint" {
		t.Errorf("title = %q, want %q", title, "Reading-time endpoint")
	}
	if body != "See the spec for details." {
		t.Errorf("body = %q, want %q", body, "See the spec for details.")
	}
	if number != 42 {
		t.Errorf("number = %d, want 42", number)
	}
}

// TestGHIssueFetcherFetchEmptyBody covers an issue with no body -- gh
// still reports an empty string, not a missing JSON field.
func TestGHIssueFetcherFetchEmptyBody(t *testing.T) {
	dir := t.TempDir()
	ghBinary := writeFakeGH(t, dir, `echo '{"title":"Flaky test in test_app.py","body":"","number":7}'
`)
	f := ghIssueFetcher{GHBinary: ghBinary}
	title, body, number, err := f.fetch(context.Background(), "https://github.com/acme/widgets/issues/7")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if title != "Flaky test in test_app.py" {
		t.Errorf("title = %q, want %q", title, "Flaky test in test_app.py")
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
	if number != 7 {
		t.Errorf("number = %d, want 7", number)
	}
}

// TestGHIssueFetcherFetchFailureSurfacesClearError covers gh failing (not
// authenticated, issue doesn't exist, network unreachable): the error must
// name the cause, not silently produce empty text.
func TestGHIssueFetcherFetchFailureSurfacesClearError(t *testing.T) {
	dir := t.TempDir()
	ghBinary := writeFakeGH(t, dir, `echo "gh: not logged in to any hosts" >&2
exit 1
`)
	f := ghIssueFetcher{GHBinary: ghBinary}
	_, _, _, err := f.fetch(context.Background(), "https://github.com/acme/widgets/issues/1")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %v, want it to name the cause (gh's own stderr)", err)
	}
}

// TestResolveSubmitRequestTextThreeWayMutualExclusion covers -request-file,
// -issue, and inline trailing text all being mutually exclusive: any two
// (or all three) set at once must error, with none of the sources
// (including fetchIssue) ever consulted.
func TestResolveSubmitRequestTextThreeWayMutualExclusion(t *testing.T) {
	unreachedFetch := func(context.Context, string) (string, string, int, error) {
		t.Fatal("fetchIssue must not be called when sources are mutually exclusive")
		return "", "", 0, nil
	}

	cases := []struct {
		name        string
		requestFile string
		issueURL    string
		trailing    []string
	}{
		{"file and inline", "req.txt", "", []string{"inline text"}},
		{"file and issue", "req.txt", "https://github.com/acme/widgets/issues/1", nil},
		{"issue and inline", "", "https://github.com/acme/widgets/issues/1", []string{"inline text"}},
		{"all three", "req.txt", "https://github.com/acme/widgets/issues/1", []string{"inline text"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := resolveSubmitRequestText(context.Background(), tc.requestFile, tc.issueURL, tc.trailing, unreachedFetch)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), "mutually exclusive") {
				t.Errorf("error = %v, want it to mention mutual exclusivity", err)
			}
		})
	}
}

// TestResolveSubmitRequestTextIssueBuildsTitleAndBody covers -issue's own
// text assembly (title, blank line, then body) and its fully-qualified
// "<owner>/<repo>#<N>" issue reference, parsed from the issue URL itself.
func TestResolveSubmitRequestTextIssueBuildsTitleAndBody(t *testing.T) {
	fetch := func(context.Context, string) (string, string, int, error) {
		return "Reading-time endpoint", "See the spec for details.", 42, nil
	}
	text, issueRef, idText, err := resolveSubmitRequestText(context.Background(), "", "https://github.com/acme/widgets/issues/42", nil, fetch)
	if err != nil {
		t.Fatalf("resolveSubmitRequestText: %v", err)
	}
	if want := "Reading-time endpoint\n\nSee the spec for details."; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if issueRef != "acme/widgets#42" {
		t.Errorf("issueRef = %q, want %q", issueRef, "acme/widgets#42")
	}
	// Regression: idText is the title alone, never title+body, so a
	// request id slugged from it never picks up issue body words.
	if idText != "Reading-time endpoint" {
		t.Errorf("idText = %q, want just the title", idText)
	}
}

// TestResolveSubmitRequestTextIssueFallsBackToTitleOnEmptyBody covers the
// no-body case: just the title, no trailing blank line.
func TestResolveSubmitRequestTextIssueFallsBackToTitleOnEmptyBody(t *testing.T) {
	fetch := func(context.Context, string) (string, string, int, error) {
		return "Flaky test in test_app.py", "", 7, nil
	}
	text, issueRef, idText, err := resolveSubmitRequestText(context.Background(), "", "https://github.com/acme/widgets/issues/7", nil, fetch)
	if err != nil {
		t.Fatalf("resolveSubmitRequestText: %v", err)
	}
	if text != "Flaky test in test_app.py" {
		t.Errorf("text = %q, want just the title", text)
	}
	if issueRef != "acme/widgets#7" {
		t.Errorf("issueRef = %q, want %q", issueRef, "acme/widgets#7")
	}
	if idText != "Flaky test in test_app.py" {
		t.Errorf("idText = %q, want the title", idText)
	}
}

// TestResolveSubmitRequestTextIssueRefNamesTheIssuesOwnRepoNotWorkspace is
// the regression test for the cross-repo Closes ambiguity (Codex review of
// PR #92): -issue's URL and the workspace being submitted for are
// independent inputs, and the qualified Closes reference must always name
// the ISSUE's own owner/repo -- an issue URL for a repo that looks nothing
// like the workspace still produces a correctly-qualified reference.
func TestResolveSubmitRequestTextIssueRefNamesTheIssuesOwnRepoNotWorkspace(t *testing.T) {
	fetch := func(context.Context, string) (string, string, int, error) {
		return "Upstream dependency bump", "", 99, nil
	}
	_, issueRef, _, err := resolveSubmitRequestText(context.Background(), "", "https://github.com/other-org/unrelated-repo/issues/99", nil, fetch)
	if err != nil {
		t.Fatalf("resolveSubmitRequestText: %v", err)
	}
	if issueRef != "other-org/unrelated-repo#99" {
		t.Errorf("issueRef = %q, want %q (the issue's own owner/repo, regardless of what the workspace is)", issueRef, "other-org/unrelated-repo#99")
	}
}

// TestParseGitHubIssueURLRejectsUnrecognizedShape covers a malformed or
// non-GitHub -issue URL failing with a clear, named error rather than
// silently producing an unqualified or wrong reference.
func TestParseGitHubIssueURLRejectsUnrecognizedShape(t *testing.T) {
	for _, bad := range []string{
		"not a url",
		"https://gitlab.com/acme/widgets/issues/1",
		"https://github.com/acme/widgets/pull/1",
		"https://github.com/acme",
	} {
		if _, _, err := parseGitHubIssueURL(bad); err == nil {
			t.Errorf("parseGitHubIssueURL(%q): expected an error, got nil", bad)
		}
	}
}

// TestResolveSubmitRequestTextIssueFetchFailureSurfacesError covers a
// failing fetch propagating a clear, named error rather than an empty
// ticket.
func TestResolveSubmitRequestTextIssueFetchFailureSurfacesError(t *testing.T) {
	fetch := func(context.Context, string) (string, string, int, error) {
		return "", "", 0, errors.New("gh issue view failed: not logged in")
	}
	_, _, _, err := resolveSubmitRequestText(context.Background(), "", "https://github.com/acme/widgets/issues/1", nil, fetch)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error = %v, want it to name the underlying cause", err)
	}
}

// TestSubmitMainIssueAndInlineTextAreMutuallyExclusive is submitMain's own
// integration point for -issue: since submitMain always wires
// ghIssueFetcher{}.fetch (the real `gh`), this exercises only the mutual
// exclusion / plumbing path, which never reaches gh -- issue and inline
// text together must fail before submitMain records anything.
func TestSubmitMainIssueAndInlineTextAreMutuallyExclusive(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make verify"
`)
	dataDir := t.TempDir()

	err := submitMain(dp, []string{"-data-dir", dataDir, "-issue", "https://github.com/acme/widgets/issues/1", workspace, "also inline"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want it to mention mutual exclusivity", err)
	}

	requests, listErr := request.List(dataDir)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(requests) != 0 {
		t.Errorf("len(requests) = %d, want 0 -- nothing should be recorded on error", len(requests))
	}
}

// TestSubmitMainIssueBeforeWorkspaceReachesFetchPath is the regression
// test for the flag-ordering bug Codex found reviewing PR #92: Go's
// flag.FlagSet stops parsing flags at the first non-flag (positional)
// argument, so `factoryd submit <workspace> -issue <url>` never actually
// parses -issue at all -- "-issue" and "<url>" land in flags.Args() as
// literal trailing request text, and *issue stays "". This test uses
// submitMain's own documented, correct ordering (every flag before
// <workspace>) and asserts against real side effects only reachable if
// gh was actually invoked and *issue was actually non-empty: the recorded
// request's IssueRef, and the issue's title/body landing in request.md --
// unlike a resolveSubmitRequestText-only test, this exercises submitMain's
// actual flag.FlagSet end to end with realistic argv, which is what the
// reviewed bug lived in.
func TestSubmitMainIssueBeforeWorkspaceReachesFetchPath(t *testing.T) {
	dp := newTestDeps(t)
	ghDir := t.TempDir()
	ghPath := filepath.Join(ghDir, "gh")
	script := "#!/bin/sh\necho '{\"title\":\"Reading-time endpoint\",\"body\":\"See the spec for details.\",\"number\":42}'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	// Every flag precedes <workspace>, per submitMain's own documented
	// requirement -- this is the ordering that must work.
	if err := submitMain(dp, []string{"-data-dir", dataDir, "-issue", "https://github.com/acme/notes-demo/issues/42", workspace}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1 (got %v) -- -issue must have reached the fetch path, not been swallowed as trailing text", len(requests), requests)
	}
	if requests[0].Source.Kind != request.SourceIssue {
		t.Errorf("Source.Kind = %q, want %q", requests[0].Source.Kind, request.SourceIssue)
	}
	if requests[0].Source.IssueRef != "acme/notes-demo#42" {
		t.Errorf("Source.IssueRef = %q, want %q", requests[0].Source.IssueRef, "acme/notes-demo#42")
	}

	textBytes, err := os.ReadFile(request.TextPath(dataDir, requests[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	text := string(textBytes)
	if !strings.Contains(text, "Reading-time endpoint") || !strings.Contains(text, "See the spec for details.") {
		t.Errorf("request.md does not contain the fetched issue's title/body -- -issue was not actually parsed as a flag: %q", text)
	}
}

// TestSubmitMainIssueRequestIDSlugsTitleOnly is the regression test: a
// request submitted via -issue used to slug its id from
// title+"\n\n"+body, so an unrelated word from the issue body could end
// up in the id. The id must be slugged from the issue title alone.
func TestSubmitMainIssueRequestIDSlugsTitleOnly(t *testing.T) {
	dp := newTestDeps(t)
	ghDir := t.TempDir()
	ghPath := filepath.Join(ghDir, "gh")
	script := "#!/bin/sh\necho '{\"title\":\"Reading time endpoint\",\"body\":\"Includes zzqqxxbodyword nowhere near the title.\",\"number\":42}'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-data-dir", dataDir, "-issue", "https://github.com/acme/notes-demo/issues/42", workspace}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	id := requests[0].ID
	if !strings.Contains(id, "reading-time-endpoint") {
		t.Errorf("id = %q, want it slugged from the issue title", id)
	}
	if strings.Contains(id, "zzqqxxbodyword") {
		t.Errorf("id = %q, must not contain a word from the issue body", id)
	}
}

// TestSubmitMainRecordsProjectAsWorkspaceBasename pins the project id
// `submit` records: the checkout directory's own basename, not the
// legacy release.ProjectFromWorkspace parent-basename derivation under
// which ~/code/payments and ~/code/notes-demo both collapsed to "code" and
// shared one kill switch.
func TestSubmitMainRecordsProjectAsWorkspaceBasename(t *testing.T) {
	dp := newTestDeps(t)
	workspace := filepath.Join(t.TempDir(), "code", "payments")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestSubmitRepo(t, workspace, `verify_command: "true"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Add idempotency keys"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	if requests[0].Project != "payments" {
		t.Errorf("req.Project = %q, want %q (the checkout's basename, not its parent's)", requests[0].Project, "payments")
	}
}

// TestSubmitMainHarnessFlagAcceptsAllowedChoice proves `factoryd submit
// -harness execution=pifork` records the choice on the request once pifork
// is a member of roles.execution.allowed_harnesses.
func TestSubmitMainHarnessFlagAcceptsAllowedChoice(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := routesModeConfigWithAllowedSonnet(t)
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-config", configPath, "-harness", "execution=pifork", "-data-dir", dataDir, workspace, "Add a new func"}); err != nil {
		t.Fatalf("submitMain -harness execution=pifork: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("List = %v, %v; want one request", requests, err)
	}
	if got, want := requests[0].Harnesses["execution"], "pifork"; got != want {
		t.Errorf("req.Harnesses[execution] = %q, want %q", got, want)
	}
}

// TestSubmitMainHarnessFlagRejections proves each way a -harness choice can
// be wrong refuses the whole submission with a message naming the problem,
// writing no request: outside the role's allowed_harnesses, review (never
// requester-selectable), an unknown harness, a malformed pair, and a session
// with no roles at all.
func TestSubmitMainHarnessFlagRejections(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := routesModeConfigWithAllowedSonnet(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"outside allowed_harnesses": {[]string{"-config", configPath, "-harness", "planning=pifork"}, "roles.planning.allowed_harnesses"},
		"review":                    {[]string{"-config", configPath, "-harness", "review=pi"}, "review"},
		"unknown harness":           {[]string{"-config", configPath, "-harness", "execution=not-a-real-harness"}, "not-a-real-harness"},
		"malformed pair":            {[]string{"-config", configPath, "-harness", "pifork"}, "-harness"},
		"no roles":                  {[]string{"-harness", "execution=pi"}, "roles"},
	} {
		dataDir := t.TempDir()
		err := submitMain(dp, append(append([]string{}, tc.args...), "-data-dir", dataDir, workspace, "Add a new func"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
		if requests, _ := request.List(dataDir); len(requests) != 0 {
			t.Errorf("%s: %d requests written after a refused submission", name, len(requests))
		}
	}
}
