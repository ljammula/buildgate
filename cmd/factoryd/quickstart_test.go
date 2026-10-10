package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/meter"
	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
)

// ---- resolveQuickstartRepoRoot ------------------------------------------

func TestResolveQuickstartRepoRootRefusesNonGitPath(t *testing.T) {
	dir := t.TempDir()
	toplevel := func(d string) (string, error) {
		return "", errors.New("fatal: not a git repository")
	}
	_, err := resolveQuickstartRepoRoot(dir, toplevel)
	if err == nil {
		t.Fatal("expected an error for a non-git directory")
	}
	if !strings.Contains(err.Error(), "not inside a git repository") {
		t.Errorf("error = %q, want a message pointing at the non-git-repo refusal", err.Error())
	}
	if !strings.Contains(err.Error(), "factoryd intake") || !strings.Contains(err.Error(), "factoryd init") {
		t.Errorf("error = %q, want it to point at intake/init as the from-scratch path", err.Error())
	}
}

func TestResolveQuickstartRepoRootDetectsGitRepo(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	var gotDir string
	toplevel := func(d string) (string, error) {
		gotDir = d
		return dir, nil
	}
	root, err := resolveQuickstartRepoRoot(sub, toplevel)
	if err != nil {
		t.Fatalf("resolveQuickstartRepoRoot: %v", err)
	}
	if root != dir {
		t.Errorf("root = %q, want %q", root, dir)
	}
	if gotDir != sub {
		t.Errorf("toplevel called with %q, want the resolved absolute path %q", gotDir, sub)
	}
}

func TestResolveQuickstartRepoRootDefaultsToDot(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	toplevel := func(d string) (string, error) { return dir, nil }
	root, err := resolveQuickstartRepoRoot("", toplevel)
	if err != nil {
		t.Fatalf("resolveQuickstartRepoRoot: %v", err)
	}
	// Resolve both sides through EvalSymlinks-free filepath.Abs so a
	// symlinked TMPDIR (e.g. macOS's /tmp -> /private/tmp) doesn't make an
	// otherwise-correct comparison fail.
	wantAbs, _ := filepath.Abs(dir)
	if root != wantAbs {
		t.Errorf("root = %q, want %q", root, wantAbs)
	}
}

func TestResolveQuickstartRepoRootRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveQuickstartRepoRoot(file, func(string) (string, error) { return dir, nil })
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want a not-a-directory error", err)
	}
}

// ---- quickstartValidateRequestSource (-issue/-request-file) -------------

// TestQuickstartValidateRequestSourceMutualExclusion mirrors
// TestResolveSubmitRequestTextThreeWayMutualExclusion (submit_test.go): any
// two (or all three) of -goal/trailing text, -issue, -request-file set at
// once must be rejected, before any side effect (doctor/config/daemon) --
// this function itself performs none, so a passing test here already
// proves that for whatever calls it.
func TestQuickstartValidateRequestSourceMutualExclusion(t *testing.T) {
	cases := []struct {
		name        string
		goal        string
		issue       string
		requestFile string
	}{
		{"goal and issue", "do the thing", "https://github.com/acme/widgets/issues/1", ""},
		{"goal and file", "do the thing", "", "req.txt"},
		{"issue and file", "", "https://github.com/acme/widgets/issues/1", "req.txt"},
		{"all three", "do the thing", "https://github.com/acme/widgets/issues/1", "req.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &quickstartOptions{Goal: tc.goal, Issue: tc.issue, RequestFile: tc.requestFile}
			err := quickstartValidateRequestSource(opts)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), "mutually exclusive") {
				t.Errorf("error = %v, want it to mention mutual exclusivity", err)
			}
		})
	}
}

// TestQuickstartStdinIsInteractiveDevNullIsNotInteractive is the
// regression test for a bug found during onboarding (2026-09-26):
// /dev/null is a character device but not a terminal, so a bare
// os.ModeCharDevice check
// misidentified `factoryd quickstart </dev/null` (the shape cron/launchd/CI
// invoke it with) as interactive and hung it on a prompt it could never
// answer.
func TestQuickstartStdinIsInteractiveDevNullIsNotInteractive(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	if quickstartStdinIsInteractive(f) {
		t.Errorf("quickstartStdinIsInteractive(%s) = true, want false", os.DevNull)
	}
}

// TestQuickstartValidateRequestSourceNonInteractiveAcceptsIssueAlone covers
// -non-interactive with only -issue set: it must pass the "a source is
// required" check that used to only accept -goal (a plain refactor of the
// pre-existing opts.Goal == "" check would otherwise silently regress this).
func TestQuickstartValidateRequestSourceNonInteractiveAcceptsIssueAlone(t *testing.T) {
	opts := &quickstartOptions{NonInteractive: true, Issue: "https://github.com/acme/widgets/issues/1"}
	if err := quickstartValidateRequestSource(opts); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// TestQuickstartValidateRequestSourceNonInteractiveAcceptsRequestFileAlone
// is TestQuickstartValidateRequestSourceNonInteractiveAcceptsIssueAlone's
// own -request-file counterpart.
func TestQuickstartValidateRequestSourceNonInteractiveAcceptsRequestFileAlone(t *testing.T) {
	dir := t.TempDir()
	reqFile := filepath.Join(dir, "req.txt")
	if err := os.WriteFile(reqFile, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{NonInteractive: true, RequestFile: reqFile}
	if err := quickstartValidateRequestSource(opts); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// TestQuickstartValidateRequestSourceNonInteractiveRejectsNoSource covers
// the original bare-goal-required case still working once -issue/
// -request-file exist alongside it.
func TestQuickstartValidateRequestSourceNonInteractiveRejectsNoSource(t *testing.T) {
	opts := &quickstartOptions{NonInteractive: true}
	err := quickstartValidateRequestSource(opts)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "-non-interactive") {
		t.Errorf("error = %v, want it to name -non-interactive", err)
	}
}

// TestQuickstartValidateRequestSourceRejectsBadIssueURL covers a malformed
// -issue URL failing before any side effect, the same
// TestParseGitHubIssueURLRejectsUnrecognizedShape cases submit's own
// parseGitHubIssueURL already rejects.
func TestQuickstartValidateRequestSourceRejectsBadIssueURL(t *testing.T) {
	opts := &quickstartOptions{Issue: "not a url"}
	err := quickstartValidateRequestSource(opts)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "GitHub issue URL") {
		t.Errorf("error = %v, want it to name the malformed -issue URL", err)
	}
}

// TestQuickstartValidateRequestSourceRejectsUnreadableRequestFile covers a
// -request-file that does not exist, or is a directory (which passes a bare
// os.Stat), failing before any side effect.
func TestQuickstartValidateRequestSourceRejectsUnreadableRequestFile(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "missing.txt"),
		"directory": dir,
	} {
		t.Run(name, func(t *testing.T) {
			err := quickstartValidateRequestSource(&quickstartOptions{RequestFile: path})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), "-request-file") {
				t.Errorf("error = %v, want it to name -request-file", err)
			}
		})
	}
}

// ---- quickstartSubmitAndWatch (-issue/-request-file plumbing) -----------

// TestQuickstartSubmitAndWatchRequestFileReachesRequest covers -request-file
// text reaching the submitted request, the same way
// TestSubmitMainWritesRequest proves it for plain `factoryd submit`.
func TestQuickstartSubmitAndWatchRequestFileReachesRequest(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()
	reqFile := filepath.Join(t.TempDir(), "req.txt")
	if err := os.WriteFile(reqFile, []byte("Reading-time endpoint, see issue #42"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := quickstartSubmitAndWatch(dp, &buf, workspace, dataDir, "", "", reqFile, "", false, "", false, time.Millisecond, time.Millisecond, "", false)
	if err != nil {
		t.Fatalf("quickstartSubmitAndWatch: %v", err)
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
	text, err := os.ReadFile(request.TextPath(dataDir, requests[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "Reading-time endpoint, see issue #42") {
		t.Errorf("request.md does not contain the file's request text: %q", string(text))
	}
}

// TestQuickstartSubmitAndWatchIssueReachesRequest covers -issue text (and
// its fully-qualified issue reference) reaching the submitted request,
// stubbing forge.fetchIssue so it never invokes a real `gh`.
func TestQuickstartSubmitAndWatchIssueReachesRequest(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestSubmitRepo(t, workspace, `verify_command: "make verify"
preflight_profile: brownfield
`)
	dataDir := t.TempDir()

	orig := fakeForgeOf(dp).fetchIssueFn
	fakeForgeOf(dp).fetchIssueFn = func(context.Context, string) (string, string, int, error) {
		return "Reading-time endpoint", "See the spec for details.", 42, nil
	}
	defer func() { fakeForgeOf(dp).fetchIssueFn = orig }()

	var buf bytes.Buffer
	err := quickstartSubmitAndWatch(dp, &buf, workspace, dataDir, "", "https://github.com/acme/widgets/issues/42", "", "", false, "", false, time.Millisecond, time.Millisecond, "", false)
	if err != nil {
		t.Fatalf("quickstartSubmitAndWatch: %v", err)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("len(requests) = %d, want 1", len(requests))
	}
	if requests[0].Source.Kind != request.SourceIssue {
		t.Errorf("Source.Kind = %q, want %q", requests[0].Source.Kind, request.SourceIssue)
	}
	if requests[0].Source.IssueRef != "acme/widgets#42" {
		t.Errorf("Source.IssueRef = %q, want %q", requests[0].Source.IssueRef, "acme/widgets#42")
	}
	text, err := os.ReadFile(request.TextPath(dataDir, requests[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "Reading-time endpoint") {
		t.Errorf("request.md does not contain the issue's title: %q", string(text))
	}
}

// ---- -non-interactive missing-flag errors -------------------------------

func TestQuickstartBuildConfigNonInteractiveRequiresRoute(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{NonInteractive: true}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-route is required") {
		t.Errorf("err = %v, want a -route-is-required error", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveOpenAIRequiresModelHost(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{NonInteractive: true, Route: "openai"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-model-host is required") {
		t.Errorf("err = %v, want a -model-host-is-required error", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveOpenAIRequiresModelID(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{NonInteractive: true, Route: "openai", ModelHost: "http://127.0.0.1:1"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-model-id is required") {
		t.Errorf("err = %v, want a -model-id-is-required error", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveOpenAIRequiresContextWindow(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{NonInteractive: true, Route: "openai", ModelHost: "http://127.0.0.1:1", ModelID: "local-model"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-context-window is required") {
		t.Errorf("err = %v, want a -context-window-is-required error", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveAnthropicRequiresCredential(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	opts := &quickstartOptions{NonInteractive: true, Route: "anthropic"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-credential is required") {
		t.Errorf("err = %v, want a -credential-is-required error", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveCopilotRequiresModelIDAndCredential(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	// HOME sandboxed to an empty temp dir so quickstartDetectCopilotLogin
	// never discovers a real pi/pifork login on the machine running this
	// test (and never makes a real network call to list its models) --
	// see quickstartDetectCopilotLogin's own doc comment.
	t.Setenv("HOME", t.TempDir())
	opts := &quickstartOptions{NonInteractive: true, Route: "copilot"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-model-id is required") {
		t.Errorf("err = %v, want a -model-id-is-required error", err)
	}

	// -context-window is required too, before credential is even checked
	// -- worker's own startup preflight requires it for any configured
	// worker model id, copilot included, regardless of the route's own
	// earlier claim otherwise (a Codex review on this PR, round 3).
	opts = &quickstartOptions{NonInteractive: true, Route: "copilot", ModelID: "gpt-x"}
	_, _, err = quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-context-window is required") {
		t.Errorf("err = %v, want a -context-window-is-required error", err)
	}

	opts = &quickstartOptions{NonInteractive: true, Route: "copilot", ModelID: "gpt-x", ContextWindow: 131072, ContextWindowExplicit: true}
	_, _, err = quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-credential is required") {
		t.Errorf("err = %v, want a -credential-is-required error", err)
	}
}

// TestQuickstartBuildConfigCopilotSetsContextWindow is the regression test
// for the Codex finding (P1, round 3) that a copilot config never wrote
// relay_worker_model_extra_json.contextWindow at all -- worker's
// doctorCheckContextWindowConfigured runs for any non-empty
// relay_worker_model_id, copilot included, so every copilot quickstart
// run failed at the daemon's own startup preflight.
func TestQuickstartBuildConfigCopilotSetsContextWindow(t *testing.T) {
	dp := newTestDeps(t)
	// See TestQuickstartBuildConfigNonInteractiveCopilotRequiresModelIDAndCredential's
	// own comment on why HOME must be sandboxed for any -route copilot test.
	t.Setenv("HOME", t.TempDir())
	// The listing now runs whenever a token is available, even with
	// -context-window given explicitly (a round-1 review finding), so
	// this must stub it like every other -route copilot test rather than
	// let quickstartBuildConfig attempt a real network call. "gpt-x"
	// isn't in this listing, so the worker-API check just warns
	// (unverified) -- unrelated to what this test actually checks.
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return nil, nil
	})
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "copilot",
		ModelID:               "gpt-x",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
		Credential:            "gho_test",
		CredentialProvided:    true,
	}
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if len(credentialEnv) != 1 || credentialEnv[0] != "GITHUB_COPILOT_TOKEN=gho_test" {
		t.Errorf("credentialEnv = %v", credentialEnv)
	}
	if model, ok := cfg.Models["gpt-x"]; !ok || model.ContextWindow != 131072 {
		t.Errorf("Models[gpt-x].ContextWindow = %v, want 131072", model.ContextWindow)
	}
}

// TestQuickstartBuildConfigDefaultsCodeReviewPolicyToRequired covers M2-D's
// quickstart change: a freshly built session config turns the standalone
// AI code-review pass on (worker's own bare -code-review-policy flag
// default stays "off", for an operator running with no config at all --
// see that flag's own doc comment), so a new factory gets the
// review-corrective round's code_review half working out of the box
// rather than needing a manual opt-in.
func TestQuickstartBuildConfigDefaultsCodeReviewPolicyToRequired(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if cfg.CodeReviewPolicy == nil || *cfg.CodeReviewPolicy != codereview.PolicyRequired {
		t.Errorf("CodeReviewPolicy = %v, want %q", cfg.CodeReviewPolicy, codereview.PolicyRequired)
	}
}

// TestQuickstartPickCopilotModelIDOffersResponsesOnlyModel is the
// regression test for the picker's own half of the Responses-route fix:
// a /responses-only model (e.g. gpt-5.6-luna) must appear as a pickable
// option, not be hidden as unusable, now that quickstart can write
// models.<name>.api: openai-responses for it.
func TestQuickstartPickCopilotModelIDOffersResponsesOnlyModel(t *testing.T) {
	var out bytes.Buffer
	listed := []meter.CopilotModel{
		{ID: "gpt-5.6-luna", SupportedEndpoints: []string{"/responses"}},
		{ID: "claude-model", SupportedEndpoints: []string{"/v1/messages"}},
	}
	got, err := quickstartPickCopilotModelID(newQuickstartPrompter(strings.NewReader("1\n")), &out, listed)
	if err != nil {
		t.Fatalf("quickstartPickCopilotModelID: %v", err)
	}
	if got != "gpt-5.6-luna" {
		t.Errorf("picked id = %q, want gpt-5.6-luna offered as the first (only usable) option", got)
	}
	if !strings.Contains(out.String(), "1 listed model(s) hidden") {
		t.Errorf("output = %q, want it to say the /v1/messages-only model was hidden", out.String())
	}
}

// TestQuickstartBuildConfigCopilotResponsesOnlyModelSetsWorkerAPI is the
// regression test for the Responses-route fix: picking a Copilot model
// that serves only /responses (e.g. gpt-5.6-luna) used to be refused
// outright by meter.CopilotModelUsable's old chat/completions-only rule.
// quickstart must instead write api: openai-responses on the model so the
// relay switches to CopilotResponsesPath (applyGitHubCopilotRelayDefaults).
func TestQuickstartBuildConfigCopilotResponsesOnlyModelSetsWorkerAPI(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{
			{ID: "gpt-5.6-luna", ContextWindow: 200000, SupportedEndpoints: []string{"/responses"}},
		}, nil
	})
	opts := &quickstartOptions{
		NonInteractive:     true,
		Route:              "copilot",
		ModelID:            "gpt-5.6-luna",
		Credential:         "gho_test",
		CredentialProvided: true,
	}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	model, ok := cfg.Models["gpt-5.6-luna"]
	if !ok || model.API != meter.RequestFormatOpenAIResponses {
		t.Fatalf("Models[gpt-5.6-luna].API = %v, want %q", model.API, meter.RequestFormatOpenAIResponses)
	}
	if model.ContextWindow != 200000 {
		t.Errorf("contextWindow = %v, want 200000 (from the listing)", model.ContextWindow)
	}
}

// TestQuickstartBuildConfigCopilotResponsesOnlyModelWithExplicitContextWindowStillLists
// is the regression test for a round-2 review finding: the listing must
// run (and the worker API get verified) even when -context-window is
// already given explicitly, not only when the listing is needed to fill
// it in.
func TestQuickstartBuildConfigCopilotResponsesOnlyModelWithExplicitContextWindowStillLists(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	listingCalled := false
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		listingCalled = true
		return []meter.CopilotModel{
			{ID: "gpt-5.6-luna", ContextWindow: 200000, SupportedEndpoints: []string{"/responses"}},
		}, nil
	})
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "copilot",
		ModelID:               "gpt-5.6-luna",
		ContextWindow:         272000,
		ContextWindowExplicit: true,
		Credential:            "gho_test",
		CredentialProvided:    true,
	}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if !listingCalled {
		t.Fatal("listGitHubCopilotModelsFn was never called, want the listing to run even with -context-window given explicitly")
	}
	model, ok := cfg.Models["gpt-5.6-luna"]
	if !ok || model.API != meter.RequestFormatOpenAIResponses {
		t.Fatalf("Models[gpt-5.6-luna].API = %v, want %q", model.API, meter.RequestFormatOpenAIResponses)
	}
	if model.ContextWindow != 272000 {
		t.Errorf("contextWindow = %v, want the explicit 272000, not the listing's own value", model.ContextWindow)
	}
}

// TestQuickstartBuildConfigCopilotWarnsWhenNoListingRan is the regression
// test for the "could not verify (no listing)" warning: with no token
// available from any source (-credential, GITHUB_COPILOT_TOKEN, or a
// discovered pi/pifork login), the listing never runs at all, so
// quickstart must warn that the worker API couldn't be verified instead
// of silently writing an unverified completions config -- even though
// this non-interactive case still ultimately fails on the later
// -credential-is-required check.
func TestQuickstartBuildConfigCopilotWarnsWhenNoListingRan(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	var out bytes.Buffer
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "copilot",
		ModelID:               "gpt-5.6-luna",
		ContextWindow:         200000,
		ContextWindowExplicit: true,
	}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-credential is required") {
		t.Fatalf("err = %v, want the eventual -credential-is-required failure", err)
	}
	if !strings.Contains(out.String(), "could not verify") || !strings.Contains(out.String(), "no Copilot model listing") {
		t.Errorf("output = %q, want the no-listing warning naming the model and \"no Copilot model listing\"", out.String())
	}
}

// TestQuickstartBuildConfigCopilotRefusesModelAbsentFromSuccessfulListing
// is the regression test for the "listing ran but id absent" outcome (a
// round-2 review finding): a model id typed non-interactively that the
// listing did run and succeed for, but genuinely does not contain, must
// be refused naming that the id isn't among the account's entitled
// models -- distinct from "could not verify" (no listing ran at all) or
// the listing itself failing.
func TestQuickstartBuildConfigCopilotRefusesModelAbsentFromSuccessfulListing(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{
			{ID: "gpt-4.1", ContextWindow: 128000, SupportedEndpoints: []string{"/chat/completions"}},
		}, nil
	})
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "copilot",
		ModelID:               "not-a-real-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
		Credential:            "gho_test",
		CredentialProvided:    true,
	}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not among this account's entitled Copilot models") {
		t.Fatalf("err = %v, want a refusal naming the id as not among the entitled models", err)
	}
	if !strings.Contains(err.Error(), "gpt-4.1") {
		t.Errorf("err = %v, want it to suggest the one entitled id gpt-4.1", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveOpenAISucceedsWithEveryFlagGiven(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1/",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, "/tmp/quickstart-data")
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if len(credentialEnv) != 0 {
		t.Errorf("credentialEnv = %v, want none (no credential requested)", credentialEnv)
	}
	if cfg.Routes["local"].Upstream != "http://127.0.0.1:1" {
		t.Errorf("Routes[local].Upstream = %v, want the trailing slash trimmed", cfg.Routes["local"].Upstream)
	}
	if model, ok := cfg.Models["local-model"]; !ok || model.ID != "local-model" {
		t.Errorf("Models[local-model].ID = %v", model.ID)
	}
	if !cfg.Routes["local"].AllowNoCredential {
		t.Errorf("Routes[local].AllowNoCredential = %v, want true (no credential requested)", cfg.Routes["local"].AllowNoCredential)
	}
	if cfg.ReleaseMaxFilesChanged == nil || *cfg.ReleaseMaxFilesChanged != quickstartDefaultReleaseMaxFilesChanged {
		t.Errorf("ReleaseMaxFilesChanged = %v, want %d", cfg.ReleaseMaxFilesChanged, quickstartDefaultReleaseMaxFilesChanged)
	}
	if cfg.ReleaseRollbackPlan == nil || *cfg.ReleaseRollbackPlan == "" {
		t.Errorf("ReleaseRollbackPlan = %v, want a non-empty default", cfg.ReleaseRollbackPlan)
	}
}

// TestQuickstartBuildConfigNonInteractiveChatGPTCodexDefaultsModelID is the
// regression test for a bug found during onboarding (2026-09-26):
// chatgpt-codex has no model-listing endpoint to discover from, so with
// no -model-id it must
// fall back to chatGPTCodexDefaultModelID under -non-interactive rather
// than failing (there is nothing else it could prompt for, and the
// interactive path already shows this same value as its prompt default).
func TestQuickstartBuildConfigNonInteractiveChatGPTCodexDefaultsModelID(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if model, ok := cfg.Models[chatGPTCodexDefaultModelID]; !ok || model.ID != chatGPTCodexDefaultModelID {
		t.Errorf("Models[%s].ID = %v, want %q", chatGPTCodexDefaultModelID, model.ID, chatGPTCodexDefaultModelID)
	}
}

func TestQuickstartBuildConfigRejectsSlashInModelID(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1",
		ModelID:               "vendor/model",
		ContextWindow:         1000,
		ContextWindowExplicit: true,
	}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "must not contain a slash") {
		t.Errorf("err = %v, want a slash-rejection error", err)
	}
}

// ---- doctor-check-already-passing skip-straight-through path -----------

func TestQuickstartEnsureImagesSkipsWhenAlreadyPassing(t *testing.T) {
	dp := newTestDeps(t)
	dockerBinary, err := filepath.Abs("testdata/fake_docker_quickstart_ok.sh")
	if err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{SandboxDocker: dockerBinary}
	var out bytes.Buffer
	// No built-in default image any more (every image is built from
	// source) -- an "existing config" supplies all three so the fake
	// docker script's own "always present" answers are actually reached,
	// matching a real already-configured machine.
	sandboxImage := "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64)
	registryProxyImage := "localhost:5050/factoryd-registry-proxy@sha256:" + strings.Repeat("c", 64)
	existing := &sessionconfig.Config{SandboxImage: &sandboxImage, RegistryProxyImage: &registryProxyImage}
	if err := quickstartEnsureImages(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, "", existing); err != nil {
		t.Fatalf("quickstartEnsureImages: %v (output: %s)", err, out.String())
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Errorf("output contains a FAIL line, want every check to pass: %s", out.String())
	}
	if len(opts.builtImageRefs) != 0 {
		t.Errorf("builtImageRefs = %v, want none set when -fix never ran", opts.builtImageRefs)
	}
}

// writeDockerPresentFor writes a fake `docker` reporting the daemon
// reachable, compose present, and exactly refs present (`docker image
// inspect <ref>` exits 0 only for one of refs, and `docker pull` always
// fails) -- everything else absent. Used to drive
// quickstartSeedImagesFromDefaultConfig's own presence probe deterministically.
func writeDockerPresentFor(t *testing.T, refs ...string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo fake-docker/0.0.0; exit 0 ;;\n" +
		"  compose) if [ \"$2\" = version ]; then echo 2.99.0; exit 0; fi; exit 1 ;;\n" +
		"  image)\n" +
		"    if [ \"$2\" = inspect ]; then\n" +
		"      case \"$3\" in\n"
	for _, ref := range refs {
		script += "        \"" + ref + "\") exit 0 ;;\n"
	}
	script += "        *) exit 1 ;;\n" +
		"      esac\n" +
		"    fi\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return path
}

// ---- borrowed images must persist into the written config, not just ---
// ---- the doctor-check inputs -------------------------------------------

// TestQuickstartEnsureImagesBorrowedImagesPersistIntoFreshConfig is the
// regression test for the HIGH finding from adversarial review of the
// borrowed-images fix (2026-09-26): quickstartSeedImagesFromDefaultConfig
// fed borrowed
// image refs into the local doctor-check inputs only, so a fresh -config
// quickstart run that borrowed sandbox_image from the default
// config (because `make install` had already built and recorded it
// there) reported doctor green, printed "Reusing ...", and then wrote a
// config with no image pinned -- a real worker against it then
// failed the very "no sandbox image configured" check this borrowing
// exists to get past. Drives the real fresh-config path
// (quickstartEnsureImages then quickstartEnsureConfig, existing == nil)
// against a stubbed default config holding a present image and asserts the
// config actually written to disk carries it.
func TestQuickstartEnsureImagesBorrowedImagesPersistIntoFreshConfig(t *testing.T) {
	dp := newTestDeps(t)
	defaultPath := isolateSessionConfig(t)
	sandboxRef := "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64)
	writeSessionConfig(t, defaultPath, "sandbox_image: "+sandboxRef+"\n")
	docker := writeDockerPresentFor(t, sandboxRef)

	configPath := filepath.Join(t.TempDir(), "proj.yml")
	opts := &quickstartOptions{SandboxDocker: docker}
	p := newQuickstartPrompter(strings.NewReader(""))
	var out bytes.Buffer
	if err := quickstartEnsureImages(dp, opts, p, &out, configPath, nil); err != nil {
		t.Fatalf("quickstartEnsureImages: %v (output: %s)", err, out.String())
	}
	if got := opts.builtImageRefs["-sandbox-image"]; got != sandboxRef {
		t.Fatalf("builtImageRefs[-sandbox-image] = %q, want borrowed %q", got, sandboxRef)
	}

	opts.NonInteractive = true
	opts.Route = "openai"
	opts.ModelHost = "http://127.0.0.1:1"
	opts.ModelID = "local-model"
	opts.ContextWindow = 131072
	opts.ContextWindowExplicit = true
	if _, _, _, err := quickstartEnsureConfig(dp, opts, p, &out, configPath, nil); err != nil {
		t.Fatalf("quickstartEnsureConfig: %v (output: %s)", err, out.String())
	}
	written, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if written.SandboxImage == nil || *written.SandboxImage != sandboxRef {
		t.Errorf("written sandbox_image = %v, want borrowed %q", written.SandboxImage, sandboxRef)
	}
}

// ---- quickstartOfferStaleRebuild ----------------------------------------

// stubDoctorMakeImageWithConfigRewrite replaces the docker.makeImage seam
// for one test: instead of actually shelling out to `make`, it writes
// newSandboxImage into the session config at configPath (mirroring what a
// real `make local-images` -> `configure-images -config <configPath>`
// call would do) and records the (repoRoot, target, vars) it was
// dispatched with.
func stubDoctorMakeImageWithConfigRewrite(dp *deps, t *testing.T, configPath, newSandboxImage string) (calls *[][]string) {
	t.Helper()
	calls = new([][]string)
	prev := fakeDockerOf(dp).makeImageFn
	fakeDockerOf(dp).makeImageFn = func(repoRoot, target string, vars ...string) (string, error) {
		*calls = append(*calls, append([]string{repoRoot, target}, vars...))
		image := newSandboxImage
		writeSessionConfig(t, configPath, "sandbox_image: "+image+"\n")
		return "", nil
	}
	t.Cleanup(func() { fakeDockerOf(dp).makeImageFn = prev })
	return calls
}

// staleImageCheck builds the doctorCheck shape doctorCheckImageStale
// reports for a stale image against sourceRoot -- see that function's own
// "up to date with" name convention.
func staleImageCheck(sourceRoot string) doctorCheck {
	return doctorCheck{
		Name:     "sandbox image up to date with " + sourceRoot,
		Err:      errors.New("stale: built from source that has since changed"),
		Advisory: true,
	}
}

// TestQuickstartOfferStaleRebuildUsesImageSourceRootNotExecutableHeuristic
// is the regression test for a real finding (adversarial review of the
// ghcr-removal change): the stale-rebuild path used to resolve its
// checkout the same way as the missing-image path (flag, env, then the
// running binary's parent directory) -- which finds nothing after a
// normal `go install`, and even if it found something, staleness was
// measured against a specific, recorded image_source_root, not whatever
// checkout the executable happens to sit under. Proves the rebuild runs
// in existing's own image_source_root and forwards FACTORYD_CONFIG.
func TestQuickstartOfferStaleRebuildUsesImageSourceRootNotExecutableHeuristic(t *testing.T) {
	dp := newTestDeps(t)
	sourceRoot := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yml")
	oldImage := "worker@sha256:" + strings.Repeat("a", 64)
	newImage := "worker@sha256:" + strings.Repeat("b", 64)
	writeSessionConfig(t, configPath, "sandbox_image: "+oldImage+"\n")
	existing := &sessionconfig.Config{SandboxImage: &oldImage, ImageSourceRoot: &sourceRoot}

	calls := stubDoctorMakeImageWithConfigRewrite(dp, t, configPath, newImage)
	opts := &quickstartOptions{BuildImages: true}
	var out bytes.Buffer
	err := quickstartOfferStaleRebuild(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing, []doctorCheck{staleImageCheck(sourceRoot)}, oldImage, sourceRoot)
	if err != nil {
		t.Fatalf("quickstartOfferStaleRebuild: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("doctorMakeImage calls = %v, want exactly 1", *calls)
	}
	call := (*calls)[0]
	if call[0] != sourceRoot {
		t.Errorf("repoRoot = %q, want the recorded image_source_root %q, not an executable-heuristic guess", call[0], sourceRoot)
	}
	if call[1] != "local-images" {
		t.Errorf("target = %q, want local-images", call[1])
	}
	wantVar := "FACTORYD_CONFIG=" + configPath
	if !slices.Contains(call[2:], wantVar) {
		t.Errorf("vars = %v, want %q so configure-images writes to this quickstart's own config", call[2:], wantVar)
	}
	// The restart signal: quickstartEnsureConfig's own imagesUpdated logic
	// watches opts.builtImageRefs for exactly this key.
	if got := opts.builtImageRefs["-sandbox-image"]; got != newImage {
		t.Errorf("builtImageRefs[-sandbox-image] = %q, want the freshly rebuilt %q (the restart signal a running worker daemon needs)", got, newImage)
	}
}

// TestQuickstartOfferStaleRebuildExplicitRepoRootWinsOverImageSourceRoot
// proves an explicit -repo-root still overrides the recorded
// image_source_root -- an operator who names a checkout explicitly knows
// better than whatever staleness happened to be measured against.
func TestQuickstartOfferStaleRebuildExplicitRepoRootWinsOverImageSourceRoot(t *testing.T) {
	dp := newTestDeps(t)
	sourceRoot := t.TempDir()
	explicitRoot := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yml")
	oldImage := "worker@sha256:" + strings.Repeat("a", 64)
	writeSessionConfig(t, configPath, "sandbox_image: "+oldImage+"\n")
	existing := &sessionconfig.Config{SandboxImage: &oldImage, ImageSourceRoot: &sourceRoot}

	calls := stubDoctorMakeImageWithConfigRewrite(dp, t, configPath, oldImage)
	opts := &quickstartOptions{BuildImages: true, RepoRootFlag: explicitRoot}
	var out bytes.Buffer
	if err := quickstartOfferStaleRebuild(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing, []doctorCheck{staleImageCheck(sourceRoot)}, oldImage, sourceRoot); err != nil {
		t.Fatalf("quickstartOfferStaleRebuild: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0][0] != explicitRoot {
		t.Errorf("doctorMakeImage calls = %v, want repoRoot %q (the explicit -repo-root)", *calls, explicitRoot)
	}
}

// TestQuickstartOfferStaleRebuildPiforkSandboxImageSkipsLocalImages is the
// regression test for a real finding from adversarial review, 2026-09-25
// (Round 2 of the ghcr-removal change): a stale pifork sandbox_image used
// to trigger the same `make local-images` rebuild as a stale plain worker
// image, which doesn't build pifork at all and would silently replace it
// with a plain worker image. It must instead print the real
// `make pifork-image` guidance and run nothing.
func TestQuickstartOfferStaleRebuildPiforkSandboxImageSkipsLocalImages(t *testing.T) {
	dp := newTestDeps(t)
	sourceRoot := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yml")
	sandboxImage := "pifork-worker@sha256:" + strings.Repeat("a", 64)
	writeSessionConfig(t, configPath, "sandbox_image: "+sandboxImage+"\n")
	existing := &sessionconfig.Config{SandboxImage: &sandboxImage, ImageSourceRoot: &sourceRoot}
	docker := writeFakeDockerLabels(t, "pifork", "some-hash")

	calls := stubDoctorMakeImageWithConfigRewrite(dp, t, configPath, sandboxImage)
	opts := &quickstartOptions{BuildImages: true, SandboxDocker: docker}
	var out bytes.Buffer
	err := quickstartOfferStaleRebuild(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing, []doctorCheck{staleImageCheck(sourceRoot)}, sandboxImage, sourceRoot)
	if err != nil {
		t.Fatalf("quickstartOfferStaleRebuild: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("doctorMakeImage calls = %v, want none -- `make local-images` must never rebuild a pifork sandbox_image", *calls)
	}
	if !strings.Contains(out.String(), "make pifork-image") {
		t.Errorf("output = %q, want it to name `make pifork-image`", out.String())
	}
}

// TestQuickstartEnsureImagesNonInteractiveFailsClosedOnPullFailure pins
// -repo-root to a real (if empty) value so resolveDoctorRepoRoot succeeds
// and the test actually reaches the -non-interactive gate this test is
// named for, rather than exiting earlier via the "could not be located"
// path -- which the original version of this test did unnoticed (running
// under `go test`, the test binary's own parent directory has no
// Makefile and $FACTORYD_REPO_ROOT is unset), since its assertion
// accepted either message and so would have passed even with the
// -non-interactive gate deleted entirely (a Codex review on this PR,
// round 3).
func TestQuickstartEnsureImagesNonInteractiveFailsClosedOnPullFailure(t *testing.T) {
	dp := newTestDeps(t)
	dockerBinary, err := filepath.Abs("testdata/fake_docker.sh") // does not answer image/compose calls -> every pull check fails
	if err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{SandboxDocker: dockerBinary, NonInteractive: true, RepoRootFlag: t.TempDir()}
	var out bytes.Buffer
	err = quickstartEnsureImages(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, "", nil)
	if err == nil {
		t.Fatal("expected an error when images fail to pull under -non-interactive")
	}
	if !strings.Contains(err.Error(), "-build-images") {
		t.Errorf("err = %v, want it to specifically mention -build-images (the -non-interactive gate)", err)
	}
	if strings.Contains(err.Error(), "could not be located") {
		t.Errorf("err = %v, want the -non-interactive gate to be reached, not the repo-root-not-found path", err)
	}
}

// ---- quickstartShellQuote / quickstartDataDirArg -----------------------

// TestQuickstartShellQuoteEscapesSpacesAndMetacharacters is the
// regression test for a low-severity adversarial-review finding:
// quickstartDataDirArg used to paste the raw data dir straight into a
// printed shell command, so a path containing a space (e.g. macOS's own
// "~/Library/Application Support/factoryd/data") or a shell metacharacter
// would be split or reinterpreted by a real shell if the operator copy-
// pasted the command as printed.
func TestQuickstartShellQuoteEscapesSpacesAndMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path, no quoting needed", "/Users/op/data", "/Users/op/data"},
		{"space", "/Users/op/Application Support/data", `'/Users/op/Application Support/data'`},
		{"dollar sign", "/Users/op/$HOME/data", `'/Users/op/$HOME/data'`},
		{"single quote embedded", "/Users/op's/data", `'/Users/op'\''s/data'`},
		{"backtick", "/Users/op/`whoami`/data", "'/Users/op/`whoami`/data'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quickstartShellQuote(tc.in); got != tc.want {
				t.Errorf("quickstartShellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestQuickstartDataDirArgQuotesPathWithSpace proves the printed
// -data-dir flag itself is safe to copy-paste when the resolved data dir
// contains a space.
func TestQuickstartDataDirArgQuotesPathWithSpace(t *testing.T) {
	got := quickstartDataDirArg("/Users/op/Library/Application Support/factoryd/data")
	want := ` -data-dir '/Users/op/Library/Application Support/factoryd/data'`
	if got != want {
		t.Errorf("quickstartDataDirArg(...) = %q, want %q", got, want)
	}
}

// ---- request state-machine reporting (spec_review/halted/quarantined) --

func TestQuickstartReportRequestStateSpecReview(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", State: request.StateSpecReview}
	var out bytes.Buffer
	if err := quickstartReportRequestState(dataDir, r, &out); err != nil {
		t.Fatalf("quickstartReportRequestState: %v", err)
	}
	got := out.String()
	// The printed command must carry -data-dir whenever quickstart's own
	// data dir isn't necessarily approve's default -- otherwise the exact
	// command printed can fail with "no such file" against the wrong
	// directory (seen on a live walk).
	wantApprove := "factoryd approve -data-dir " + dataDir + " req-1"
	if !strings.Contains(got, wantApprove) {
		t.Errorf("output = %q, want the exact approve command %q", got, wantApprove)
	}
	if !strings.Contains(got, request.SpecPath(dataDir, "req-1")) {
		t.Errorf("output = %q, want the spec path", got)
	}
}

func TestQuickstartReportRequestStatePlanReview(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-2", State: request.StatePlanReview}
	var out bytes.Buffer
	if err := quickstartReportRequestState(dataDir, r, &out); err != nil {
		t.Fatalf("quickstartReportRequestState: %v", err)
	}
	wantApprove := "factoryd approve -data-dir " + dataDir + " req-2"
	if !strings.Contains(out.String(), wantApprove) {
		t.Errorf("output = %q, want the exact approve command %q", out.String(), wantApprove)
	}
}

func TestQuickstartReportRequestStateHalted(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-3", State: request.StateHalted, Error: "relay upstream unreachable"}
	var out bytes.Buffer
	err := quickstartReportRequestState(dataDir, r, &out)
	if err == nil {
		t.Fatal("expected a non-nil error for a halted request")
	}
	if !strings.Contains(err.Error(), "relay upstream unreachable") {
		t.Errorf("err = %v, want the halt reason", err)
	}
	got := out.String()
	wantRetry := "factoryd retry -data-dir " + dataDir + " req-3"
	if !strings.Contains(got, "relay upstream unreachable") || !strings.Contains(got, wantRetry) {
		t.Errorf("output = %q, want the reason and the exact retry command %q", got, wantRetry)
	}
}

func TestQuickstartReportRequestStateQuarantined(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-4", State: request.StateQuarantined, Error: "gate failure: lint"}
	var out bytes.Buffer
	err := quickstartReportRequestState(dataDir, r, &out)
	if err == nil {
		t.Fatal("expected a non-nil error for a quarantined request")
	}
	if !strings.Contains(err.Error(), "gate failure: lint") {
		t.Errorf("err = %v, want the quarantine reason", err)
	}
	got := out.String()
	wantRetry := "factoryd retry -data-dir " + dataDir + " req-4"
	if !strings.Contains(got, "gate failure: lint") || !strings.Contains(got, wantRetry) {
		t.Errorf("output = %q, want the reason and the exact retry command %q", got, wantRetry)
	}
}

func TestQuickstartReportRequestStateDoneAndCancelled(t *testing.T) {
	dataDir := t.TempDir()
	if err := quickstartReportRequestState(dataDir, &request.Request{ID: "req-5", State: request.StateDone}, &bytes.Buffer{}); err != nil {
		t.Errorf("done: err = %v, want nil (success exit)", err)
	}
	if err := quickstartReportRequestState(dataDir, &request.Request{ID: "req-6", State: request.StateCancelled}, &bytes.Buffer{}); err == nil {
		t.Error("cancelled: want a non-nil error")
	}
}

func TestQuickstartReportRequestStateUnknownIsInformationalNotFatal(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-7", State: request.StateBuilding}
	var out bytes.Buffer
	if err := quickstartReportRequestState(dataDir, r, &out); err != nil {
		t.Errorf("err = %v, want nil for a busy, non-gate state", err)
	}
	if !strings.Contains(out.String(), "factoryd status") {
		t.Errorf("output = %q, want a pointer to `factoryd status`", out.String())
	}
}

// ---- quickstartWatchRequest polls until leaving submitted/spec_drafting -

func TestQuickstartWatchRequestPollsUntilSpecReview(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	id := "req-poll"
	save := func(state request.State) {
		r := request.New(id, "/tmp/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
		r.State = state
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	save(request.StateSubmitted)

	origSleep := fakeHostOf(dp).sleepFn
	defer func() { fakeHostOf(dp).sleepFn = origSleep }()
	calls := 0
	fakeHostOf(dp).sleepFn = func(time.Duration) {
		calls++
		if calls == 1 {
			save(request.StateSpecDrafting)
		} else {
			save(request.StateSpecReview)
		}
	}

	var out bytes.Buffer
	err := quickstartWatchRequest(dp, dataDir, id, &out, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatalf("quickstartWatchRequest: %v", err)
	}
	if calls < 2 {
		t.Errorf("sleep called %d times, want at least 2 (one per non-terminal poll)", calls)
	}
	if !strings.Contains(out.String(), "factoryd approve -data-dir "+dataDir+" "+id) {
		t.Errorf("output = %q, want the approve command once spec_review is reached", out.String())
	}
}

func TestQuickstartWatchRequestTimesOutWithoutFailing(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	id := "req-timeout"
	r := request.New(id, "/tmp/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSubmitted
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	origSleep := fakeHostOf(dp).sleepFn
	defer func() { fakeHostOf(dp).sleepFn = origSleep }()
	fakeHostOf(dp).sleepFn = func(time.Duration) {}

	var out bytes.Buffer
	err := quickstartWatchRequest(dp, dataDir, id, &out, time.Nanosecond, -time.Second)
	if err != nil {
		t.Fatalf("quickstartWatchRequest: %v, want nil (timeout while still in-flight is not a failure)", err)
	}
	if !strings.Contains(out.String(), "factoryd status") {
		t.Errorf("output = %q, want a pointer to `factoryd status`", out.String())
	}
}

// ---- quickstartImagePullFailures / quickstartBuiltImageRefs -------------

func TestQuickstartImagePullFailuresFiltersToPullChecksOnly(t *testing.T) {
	checks := []doctorCheck{
		{Name: "docker daemon reachable"},
		{Name: "sandbox image present (foo@sha256:x)", Err: errors.New("boom")},
		{Name: "docker compose v2 available (>= 2.20)", Err: errors.New("too old")},
		{Name: "monorepo module root", Err: errors.New("guess"), Advisory: true},
	}
	got := quickstartImagePullFailures(checks)
	if len(got) != 1 || got[0].Name != checks[1].Name {
		t.Errorf("got %v, want exactly the one image-and-failed check", got)
	}
}

func TestQuickstartBuiltImageRefsParsesUseField(t *testing.T) {
	checks := []doctorCheck{
		{Name: "sandbox image present", Use: "-sandbox-image localhost:5050/foo@sha256:abc"},
		{Name: "no use field"},
	}
	refs := quickstartBuiltImageRefs(checks)
	if refs["-sandbox-image"] != "localhost:5050/foo@sha256:abc" {
		t.Errorf("refs[-sandbox-image] = %q", refs["-sandbox-image"])
	}
	if len(refs) != 1 {
		t.Errorf("refs = %v, want exactly one entry", refs)
	}
}

// ---- config reuse ---------------------------------------------------------

// quickstartTestRolesYAML makes a fixture a reusable config: quickstart
// reuses an existing config only when it names roles.execution (an
// images-only config is completed instead, see
// TestQuickstartCompletesImagesOnlyConfig).
const quickstartTestRolesYAML = "routes:\n  local: {upstream: https://model.example.invalid, allow_no_credential: true}\nmodels:\n  m: {id: m1, routes: [local]}\nroles:\n  execution: {model: m}\n"

func TestQuickstartEnsureConfigReusesExistingValidConfig(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte("data_dir: "+filepath.Join(dir, "data")+"\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{ConfigPath: configPath}
	var out bytes.Buffer
	gotPath, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	if gotPath != configPath {
		t.Errorf("path = %q, want %q", gotPath, configPath)
	}
	if existing == nil {
		t.Fatal("existing = nil, want the loaded config")
	}
	gotDataDir, credentialEnv, rewritten, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, gotPath, existing)
	if err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	// This fixture config carries no release_* keys, so the
	// release-defaults backfill (quickstartBackfillReleaseDefaults) fires
	// and rewrites it -- see
	// TestQuickstartEnsureConfigReuseBackfillsMissingReleaseDefaults for
	// the dedicated test of that behavior.
	if !rewritten {
		t.Error("rewritten = false, want true: the release-policy backfill should have fired")
	}
	if gotDataDir != filepath.Join(dir, "data") {
		t.Errorf("dataDir = %q, want the config's own data_dir", gotDataDir)
	}
	if len(credentialEnv) != 0 {
		t.Errorf("credentialEnv = %v, want none when reusing an existing config", credentialEnv)
	}
	if !strings.Contains(out.String(), "Reusing existing session config") {
		t.Errorf("output = %q, want a reuse message", out.String())
	}
}

// TestQuickstartEnsureConfigReuseBackfillsMissingReleaseDefaults is the
// regression test for the deny-all release policy gap (see CLAIMS.md): a
// reused session config missing release_max_files_changed/
// release_max_insertions/release_rollback_plan used to stay that way
// forever, silently denying every PR (internal/release.MergePolicyCheck: 0
// files allowed, empty rollback plan). quickstartEnsureConfig must now
// backfill exactly the missing keys, leave any key the operator already
// set untouched, and
// report which keys it added.
func TestQuickstartEnsureConfigReuseBackfillsMissingReleaseDefaults(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	// release_max_files_changed is already set (a deliberately tighter
	// operator choice); the other two are missing.
	if err := os.WriteFile(configPath, []byte("data_dir: "+filepath.Join(dir, "data")+"\nrelease_max_files_changed: 3\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{ConfigPath: configPath}
	var out bytes.Buffer
	_, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	_, _, rewritten, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing)
	if err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	if !rewritten {
		t.Error("rewritten = false, want true")
	}
	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("reload rewritten config: %v", err)
	}
	if reloaded.ReleaseMaxFilesChanged == nil || *reloaded.ReleaseMaxFilesChanged != 3 {
		t.Errorf("ReleaseMaxFilesChanged = %v, want the operator's own value (3) preserved", reloaded.ReleaseMaxFilesChanged)
	}
	if reloaded.ReleaseMaxInsertions == nil || *reloaded.ReleaseMaxInsertions != quickstartDefaultReleaseMaxInsertions {
		t.Errorf("ReleaseMaxInsertions = %v, want the backfilled default %d", reloaded.ReleaseMaxInsertions, quickstartDefaultReleaseMaxInsertions)
	}
	if reloaded.ReleaseRollbackPlan == nil || *reloaded.ReleaseRollbackPlan != quickstartDefaultReleaseRollbackPlan {
		t.Errorf("ReleaseRollbackPlan = %v, want the backfilled default %q", reloaded.ReleaseRollbackPlan, quickstartDefaultReleaseRollbackPlan)
	}
	if !strings.Contains(out.String(), "release_max_insertions") || !strings.Contains(out.String(), "release_rollback_plan") {
		t.Errorf("output = %q, want it to name the backfilled keys", out.String())
	}
	if strings.Contains(out.String(), "release_max_files_changed") {
		t.Errorf("output = %q, must not claim it added release_max_files_changed (already set)", out.String())
	}
	// The appended comment must name the command that actually wrote it --
	// quickstart's own reuse path, here, not `factoryd doctor -fix`.
	finalBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(finalBytes), "# added by factoryd quickstart ") {
		t.Errorf("%s = %q, want the appended comment to name factoryd quickstart as the writer", configPath, finalBytes)
	}
}

// TestQuickstartEnsureConfigReuseResolvesAndPersistsDefaultDataDir is
// a regression test: a reused session config with no data_dir key used to
// fall through to opts.DataDir's own flag default ("data"), resolved
// against the shell's current working directory -- running `factoryd
// quickstart` from $HOME created ~/data.
// quickstartEnsureConfig must instead resolve sessionconfig.DefaultDataDirFor (the
// same default the brand-new-config path already uses) and persist it
// into the config file, so a later command (approve/watch/worker/
// serve, all of which resolve -data-dir from session config too) agrees
// on the same directory.
func TestQuickstartEnsureConfigReuseResolvesAndPersistsDefaultDataDir(t *testing.T) {
	dp := newTestDeps(t)
	// A cwd deliberately NOT related to the config's own directory, so a
	// regression back to the old "data" (cwd-relative) fallback would
	// resolve somewhere entirely different from configDir/data and this
	// test would catch it.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	unrelatedCwd := t.TempDir()
	if err := os.Chdir(unrelatedCwd); err != nil {
		t.Fatal(err)
	}

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yml")
	// No data_dir key at all -- release_* keys present so this test
	// isolates the data_dir behavior from the release-defaults backfill's
	// own rewrite.
	if err := os.WriteFile(configPath, []byte("release_max_files_changed: 25\nrelease_max_insertions: 1000\nrelease_rollback_plan: \"git revert\"\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{ConfigPath: configPath, DataDir: "data"}
	var out bytes.Buffer
	_, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	gotDataDir, _, rewritten, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing)
	if err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	wantDataDir := filepath.Join(configDir, "data")
	if gotDataDir != wantDataDir {
		t.Errorf("dataDir = %q, want <config-dir>/data for a config outside the profiles dir (%q), not the shell's cwd", gotDataDir, wantDataDir)
	}
	if !rewritten {
		t.Error("rewritten = false, want true: data_dir was just persisted into the config")
	}

	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("reload rewritten config: %v", err)
	}
	if reloaded.DataDir == nil || *reloaded.DataDir != wantDataDir {
		t.Errorf("persisted DataDir = %v, want %q -- a later command must read the same directory from this file", reloaded.DataDir, wantDataDir)
	}
	if !strings.Contains(out.String(), wantDataDir) {
		t.Errorf("output = %q, want it to report the resolved data_dir", out.String())
	}
}

// TestQuickstartEnsureConfigReuseNullOrEmptyDataDirDoesNotCorruptYAML is
// the regression test for another adversarial-review finding: a
// reused config with an ALREADY-PRESENT `data_dir:` key -- null (no
// value) or an explicit empty string -- used to trigger the same "persist
// a resolved data_dir" path as a config with no key at all, but the old
// code only ever APPENDED a fresh "data_dir: <path>" line, producing a
// second, duplicate top-level data_dir key. gopkg.in/yaml.v3 refuses to
// parse a mapping with a duplicate key at all ("mapping key \"data_dir\"
// already defined"), which broke every later command against that config,
// not just quickstart. The fix reuses doctorRepointDataDir, which finds
// and replaces an existing data_dir: line instead of blindly appending.
func TestQuickstartEnsureConfigReuseNullOrEmptyDataDirDoesNotCorruptYAML(t *testing.T) {
	dp := newTestDeps(t)
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"null data_dir", "data_dir:\nrelease_max_files_changed: 25\nrelease_max_insertions: 1000\nrelease_rollback_plan: \"git revert\"\n"},
		{"empty-string data_dir", "data_dir: \"\"\nrelease_max_files_changed: 25\nrelease_max_insertions: 1000\nrelease_rollback_plan: \"git revert\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			configPath := filepath.Join(configDir, "config.yml")
			if err := os.WriteFile(configPath, []byte(tc.content+quickstartTestRolesYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := &quickstartOptions{ConfigPath: configPath, DataDir: "data"}
			var out bytes.Buffer
			_, existing, err := quickstartResolveExistingConfig(opts)
			if err != nil {
				t.Fatalf("quickstartResolveExistingConfig: %v", err)
			}
			gotDataDir, _, _, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing)
			if err != nil {
				t.Fatalf("quickstartEnsureConfig: %v", err)
			}
			wantDataDir := filepath.Join(configDir, "data")
			if gotDataDir != wantDataDir {
				t.Errorf("dataDir = %q, want %q", gotDataDir, wantDataDir)
			}

			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "data_dir:") != 1 {
				t.Fatalf("%s has %d data_dir: lines, want exactly 1 (a duplicate key makes YAML refuse to parse the whole file):\n%s", configPath, strings.Count(string(data), "data_dir:"), data)
			}
			reloaded, err := sessionconfig.Load(configPath)
			if err != nil {
				t.Fatalf("reload rewritten config: %v -- a duplicate data_dir: key breaks every later command against this file", err)
			}
			if reloaded.DataDir == nil || *reloaded.DataDir != wantDataDir {
				t.Errorf("persisted DataDir = %v, want %q", reloaded.DataDir, wantDataDir)
			}
		})
	}
}

// quickstartChdir changes the process cwd to dir for the duration of t,
// restoring the original cwd on cleanup, and returns dir resolved through
// os.Getwd() (not dir itself): on macOS t.TempDir() returns a
// /var/folders/... path that's actually a symlink to
// /private/var/folders/..., and filepath.Abs(...) inside
// quickstartEnsureConfig resolves against the process's real (already
// symlink-resolved) cwd -- comparing against the unresolved dir would
// spuriously fail on that path prefix alone.
func quickstartChdir(t *testing.T, dir string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	resolved, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestQuickstartEnsureConfigReuseNoticesButNeverAdoptsCwdDataRecords is
// the regression test for another adversarial-review finding: an
// earlier version of this fix auto-adopted <cwd>/data as data_dir when
// it already held records, but the check it used
// (filepath.Abs(opts.DataDir), opts.DataDir defaulting to "") actually
// probed the cwd ITSELF, never <cwd>/data -- and on a fixture that forced
// the probe to look in the right place, would have silently persisted a
// cwd-derived path (potentially inside the operator's own workspace) with
// no explicit flag at all. quickstart must now always persist
// sessionconfig.DefaultDataDirFor and only ever print a notice about <cwd>/data.
func TestQuickstartEnsureConfigReuseNoticesButNeverAdoptsCwdDataRecords(t *testing.T) {
	dp := newTestDeps(t)
	workDir := quickstartChdir(t, t.TempDir())

	// <cwd>/data already has real records.
	cwdDataDir := filepath.Join(workDir, "data")
	if err := os.MkdirAll(filepath.Join(cwdDataDir, "requests"), 0o750); err != nil {
		t.Fatal(err)
	}

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yml")
	if err := os.WriteFile(configPath, []byte("release_max_files_changed: 25\nrelease_max_insertions: 1000\nrelease_rollback_plan: \"git revert\"\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// No DataDir override: opts.DataDir stays at its own real flag default
	// ("") -- the earlier, broken fixture injected DataDir: "data" here,
	// which is exactly what let the broken cwd-itself probe accidentally
	// pass.
	opts := &quickstartOptions{ConfigPath: configPath}
	var out bytes.Buffer
	_, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	gotDataDir, _, _, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing)
	if err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	wantDataDir := filepath.Join(configDir, "data")
	if gotDataDir != wantDataDir {
		t.Errorf("dataDir = %q, want <config-dir>/data for a config outside the profiles dir (%q) -- a cwd-derived path must never be adopted", gotDataDir, wantDataDir)
	}
	if !strings.Contains(out.String(), "earlier records found at "+cwdDataDir) {
		t.Errorf("output = %q, want a notice naming %s", out.String(), cwdDataDir)
	}
	if !strings.Contains(out.String(), "-data-dir "+cwdDataDir) {
		t.Errorf("output = %q, want the notice to name the -data-dir flag to recover them", out.String())
	}
	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("reload rewritten config: %v", err)
	}
	if reloaded.DataDir == nil || *reloaded.DataDir != wantDataDir {
		t.Errorf("persisted DataDir = %v, want %q", reloaded.DataDir, wantDataDir)
	}
}

// TestQuickstartEnsureConfigReuseIgnoresTopLevelCwdRecordDirs is the
// second half of that same finding: a project root that happens to have
// its own top-level runs/ or queue/ directory (common in ML repos, unrelated to
// factoryd) must never be mistaken for a factoryd data directory and
// adopted -- quickstartDataDirHasRecords is only ever consulted against
// <cwd>/data specifically, never the cwd itself.
func TestQuickstartEnsureConfigReuseIgnoresTopLevelCwdRecordDirs(t *testing.T) {
	dp := newTestDeps(t)
	workDir := quickstartChdir(t, t.TempDir())

	// A top-level runs/ directory AT the cwd itself (not <cwd>/data) --
	// must be completely ignored.
	if err := os.MkdirAll(filepath.Join(workDir, "runs"), 0o750); err != nil {
		t.Fatal(err)
	}

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yml")
	if err := os.WriteFile(configPath, []byte("release_max_files_changed: 25\nrelease_max_insertions: 1000\nrelease_rollback_plan: \"git revert\"\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{ConfigPath: configPath}
	var out bytes.Buffer
	_, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	gotDataDir, _, _, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing)
	if err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	wantDataDir := filepath.Join(configDir, "data")
	if gotDataDir != wantDataDir {
		t.Errorf("dataDir = %q, want <config-dir>/data for a config outside the profiles dir (%q)", gotDataDir, wantDataDir)
	}
	if strings.Contains(out.String(), "earlier records found") {
		t.Errorf("output = %q, want no notice -- a top-level cwd/runs dir is not <cwd>/data and must be ignored", out.String())
	}
	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("reload rewritten config: %v", err)
	}
	if reloaded.DataDir == nil || *reloaded.DataDir != wantDataDir {
		t.Errorf("persisted DataDir = %v, want %q", reloaded.DataDir, wantDataDir)
	}
}

// TestQuickstartEnsureConfigReuseBackfillPreservesExistingFileBytes is the
// regression test for the review finding that quickstartWriteConfig's
// yaml.Marshal-the-whole-Config approach, used for the backfill path too,
// silently wiped an operator's own comments and formatting on every
// quickstart run that merely noticed a missing release_* key: reusing an
// old config was supposed to be non-destructive, but any comment or
// deliberate key order in the file on disk was destroyed the moment
// quickstartBackfillReleaseDefaults found even one key to add, since the
// fix at the time re-marshaled cfg from scratch rather than editing the
// file's own text. This proves every original byte of the file is
// preserved (only new lines appended) and that the three release_* keys
// still round-trip via sessionconfig.Load with their default values.
func TestQuickstartEnsureConfigReuseBackfillPreservesExistingFileBytes(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	original := "# my own notes on this config, please keep them\n" +
		"data_dir: " + filepath.Join(dir, "data") + "\n" +
		"# a deliberately unusual key order\n" +
		"sandbox_image: worker@sha256:" + strings.Repeat("a", 64) + "\n" +
		quickstartTestRolesYAML
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{ConfigPath: configPath}
	var out bytes.Buffer
	_, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	_, _, rewritten, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing)
	if err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	if !rewritten {
		t.Error("rewritten = false, want true: the release-policy backfill should have fired")
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read backfilled config: %v", err)
	}
	if !strings.HasPrefix(string(got), original) {
		t.Errorf("backfilled config does not start with the original file's exact bytes -- want every original byte untouched, only new lines appended.\noriginal:\n%s\ngot:\n%s", original, got)
	}

	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("reload backfilled config: %v", err)
	}
	if reloaded.ReleaseMaxFilesChanged == nil || *reloaded.ReleaseMaxFilesChanged != quickstartDefaultReleaseMaxFilesChanged {
		t.Errorf("ReleaseMaxFilesChanged = %v, want the backfilled default %d", reloaded.ReleaseMaxFilesChanged, quickstartDefaultReleaseMaxFilesChanged)
	}
	if reloaded.ReleaseMaxInsertions == nil || *reloaded.ReleaseMaxInsertions != quickstartDefaultReleaseMaxInsertions {
		t.Errorf("ReleaseMaxInsertions = %v, want the backfilled default %d", reloaded.ReleaseMaxInsertions, quickstartDefaultReleaseMaxInsertions)
	}
	if reloaded.ReleaseRollbackPlan == nil || *reloaded.ReleaseRollbackPlan != quickstartDefaultReleaseRollbackPlan {
		t.Errorf("ReleaseRollbackPlan = %v, want the backfilled default %q", reloaded.ReleaseRollbackPlan, quickstartDefaultReleaseRollbackPlan)
	}
}

// TestQuickstartBackfillReleaseDefaultsNoopWhenAllSet confirms the
// backfill never overwrites an already-complete release policy and
// reports nothing added.
func TestQuickstartBackfillReleaseDefaultsNoopWhenAllSet(t *testing.T) {
	files, insertions, plan := 7, 200, "roll back by hand"
	cfg := &sessionconfig.Config{
		ReleaseMaxFilesChanged: &files,
		ReleaseMaxInsertions:   &insertions,
		ReleaseRollbackPlan:    &plan,
	}
	added := quickstartBackfillReleaseDefaults(cfg)
	if len(added) != 0 {
		t.Errorf("added = %v, want none", added)
	}
	if *cfg.ReleaseMaxFilesChanged != 7 || *cfg.ReleaseMaxInsertions != 200 || *cfg.ReleaseRollbackPlan != "roll back by hand" {
		t.Error("backfill must not touch already-set values")
	}
}

// TestQuickstartEnsureConfigReuseWritesFreshlyBuiltImageRefs is the
// regression test for the self-review finding that reusing an existing
// config silently discarded opts.builtImageRefs: a config written before
// quickstartEnsureImages locally built an image after it was found
// missing must be rewritten with that image's ref, not reused verbatim
// with no image configured.
func TestQuickstartEnsureConfigReuseWritesFreshlyBuiltImageRefs(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(configPath, []byte("data_dir: "+filepath.Join(dir, "data")+"\nsandbox_image: canonical@sha256:old\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{
		ConfigPath:     configPath,
		builtImageRefs: map[string]string{"-sandbox-image": "localhost:5050/buildgate-worker@sha256:new"},
	}
	var out bytes.Buffer
	_, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	if _, _, _, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing); err != nil {
		t.Fatalf("quickstartEnsureConfig: %v", err)
	}
	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("reload rewritten config: %v", err)
	}
	if reloaded.SandboxImage == nil || *reloaded.SandboxImage != "localhost:5050/buildgate-worker@sha256:new" {
		t.Errorf("SandboxImage = %v, want the freshly built ref", reloaded.SandboxImage)
	}
	if !strings.Contains(out.String(), "Updated "+configPath) {
		t.Errorf("output = %q, want a message about updating the reused config", out.String())
	}
}

// ---- verify-command / preflight-profile explicitness --------------------

// TestQuickstartEnsureRepoReadyExplicitFlagStaysExplicit is the regression
// test for the self-review finding that verifyCommandExplicit/
// preflightProfileExplicit were computed from "is the resolved value
// non-empty" (always true, since both are given non-empty defaults or
// detected values) instead of "did the operator actually pass the flag" --
// which meant a repo's own committed .factory.yml could never win, even
// when the operator left both flags at their defaults, because
// submitRequest's applyProjectConfigDefaults always saw them as explicit.
func TestQuickstartEnsureRepoReadyExplicitFlagStaysExplicit(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	opts := &quickstartOptions{
		VerifyCommand:            "make my-verify",
		VerifyCommandExplicit:    true,
		PreflightProfile:         "brownfield",
		PreflightProfileExplicit: false,
		NonInteractive:           true,
	}
	verifyCommand, verifyExplicit, preflightProfile, preflightExplicit, err := quickstartEnsureRepoReady(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, dir)
	if err != nil {
		t.Fatalf("quickstartEnsureRepoReady: %v", err)
	}
	if verifyCommand != "make my-verify" || !verifyExplicit {
		t.Errorf("verifyCommand = %q, verifyExplicit = %v, want (%q, true)", verifyCommand, verifyExplicit, "make my-verify")
	}
	if preflightProfile != "brownfield" || preflightExplicit {
		t.Errorf("preflightProfile = %q, preflightExplicit = %v, want (%q, false) since -preflight-profile was left at its default", preflightProfile, preflightExplicit, "brownfield")
	}
}

// TestQuickstartEnsureRepoReadyPrefersFactoryYmlOverDetection confirms
// applyProjectConfigDefaults' own "explicit flag wins, otherwise
// .factory.yml" precedence is honored even in what quickstart itself
// displays: a repo with both a committed .factory.yml verify_command and a
// Makefile target must show (and, since preflightExplicit/verifyExplicit
// stay false, actually submit with) the .factory.yml value, not the
// Makefile-detected one.
func TestQuickstartEnsureRepoReadyPrefersFactoryYmlOverDetection(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\techo makefile-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{NonInteractive: true, PreflightProfile: "brownfield"}
	var out bytes.Buffer
	verifyCommand, verifyExplicit, _, _, err := quickstartEnsureRepoReady(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, dir)
	if err != nil {
		t.Fatalf("quickstartEnsureRepoReady: %v", err)
	}
	if verifyCommand != "make ci-verify" {
		t.Errorf("verifyCommand = %q, want the .factory.yml value \"make ci-verify\" over the Makefile-detected \"make test\"", verifyCommand)
	}
	if verifyExplicit {
		t.Error("verifyExplicit = true, want false: no -verify-command flag was passed")
	}
	if !strings.Contains(out.String(), ".factory.yml") {
		t.Errorf("output = %q, want it to say the value came from .factory.yml", out.String())
	}
}

// TestQuickstartEnsureRepoReadyAutoDetectsBrownfieldRepo is plan 2.2's own
// done criterion: an existing git repo with commits and none of the three
// onboarding artifacts (spec/spec.md, spec/contract.md, ARCHITECTURE.md)
// needs zero flags/prompts under -non-interactive to proceed, and prints
// what quickstart auto-detected. The prompter is given an empty reader
// (no queued answers) so a regression that still asked the old y/N
// "scaffold onboarding docs now?" question would surface as a read-input
// error, not just a silently-wrong default.
func TestQuickstartEnsureRepoReadyAutoDetectsBrownfieldRepo(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	// Resolved up front (as resolveQuickstartRepoRoot's real toplevel()
	// call would leave it) so the .factory.yml path this test later hands
	// to projectconfig.Load -- which itself resolves the git toplevel --
	// matches byte-for-byte on a host where TMPDIR is a symlink (e.g.
	// macOS's /var -> /private/var).
	var err error
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\techo makefile-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", "Makefile")
	runGit("commit", "-q", "-m", "initial commit")

	opts := &quickstartOptions{NonInteractive: true, PreflightProfile: "brownfield"}
	var out bytes.Buffer
	verifyCommand, _, preflightProfile, _, err := quickstartEnsureRepoReady(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, dir)
	if err != nil {
		t.Fatalf("quickstartEnsureRepoReady: %v", err)
	}
	if verifyCommand != "make test" {
		t.Errorf("verifyCommand = %q, want the Makefile-detected \"make test\"", verifyCommand)
	}
	if preflightProfile != "brownfield" {
		t.Errorf("preflightProfile = %q, want \"brownfield\"", preflightProfile)
	}
	if !strings.Contains(out.String(), "brownfield") || !strings.Contains(out.String(), "-scaffold") {
		t.Errorf("output = %q, want it to name the auto-detected brownfield profile and the -scaffold override", out.String())
	}
	factoryYML, err := os.ReadFile(filepath.Join(dir, ".factory.yml"))
	if err != nil {
		t.Fatalf("expected .factory.yml to be written: %v", err)
	}
	if !strings.Contains(string(factoryYML), `verify_command: "make test"`) || !strings.Contains(string(factoryYML), "preflight_profile: brownfield") {
		t.Errorf(".factory.yml = %q, want the detected verify_command and preflight_profile", factoryYML)
	}
	if want := 1; strings.Count(out.String(), "Commit .factory.yml") != want {
		t.Errorf("output = %q, want the commit reminder exactly once", out.String())
	}

	// Regression: quickstart's own reminder above must suppress
	// projectconfig's identical "exists in the worktree but is not
	// committed" warning on the very next Load (what submitRequest does
	// moments later in the real flow) -- otherwise the operator sees the
	// same information twice.
	var logOut bytes.Buffer
	prevOutput, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logOut)
	log.SetFlags(0)
	_, _, loadErr := projectconfig.Load(dir)
	log.SetOutput(prevOutput)
	log.SetFlags(prevFlags)
	if loadErr != nil {
		t.Fatalf("projectconfig.Load: %v", loadErr)
	}
	if strings.Contains(logOut.String(), "is not committed") {
		t.Errorf("projectconfig.Load logged %q after quickstart's own reminder, want it suppressed", logOut.String())
	}
}

// TestQuickstartEnsureRepoReadyGreenfieldRepoStillPromptsToScaffold confirms
// the brownfield auto-detection is scoped to repos with real history: a
// freshly `git init`'d repo with no commits yet still gets the
// interactive y/N scaffold prompt (declining here), since there is no
// existing codebase yet for "brownfield" to describe.
func TestQuickstartEnsureRepoReadyGreenfieldRepoStillPromptsToScaffold(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	opts := &quickstartOptions{NonInteractive: false, PreflightProfile: "brownfield", VerifyCommand: "make test", VerifyCommandExplicit: true}
	var out bytes.Buffer
	_, _, _, _, err := quickstartEnsureRepoReady(dp, opts, newQuickstartPrompter(strings.NewReader("n\n")), &out, dir)
	if err != nil {
		t.Fatalf("quickstartEnsureRepoReady: %v", err)
	}
	if !strings.Contains(out.String(), "scaffold onboarding docs") {
		t.Errorf("output = %q, want the interactive scaffold prompt for a no-commits repo", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".factory.yml")); !os.IsNotExist(err) {
		t.Errorf(".factory.yml should not be auto-written for a no-commits repo, stat err = %v", err)
	}
}

// TestQuickstartEnsureRepoReadyOffersScaffoldForPartiallyOnboardedRepo is
// the regression test for a real bug found via Codex review of PR #173:
// a repo with a hand-written ARCHITECTURE.md but no spec/ yet has
// existingArtifacts non-empty, so the brownfield auto-detect branch above
// is skipped -- but the interactive fallback used to check only
// os.Stat("ARCHITECTURE.md"), which exists here, so the scaffold prompt
// was never even offered and spec/spec.md, spec/contract.md were never
// created, despite this PR's own doOnboard change (scaffold only what's
// missing) being built exactly for this case. Confirms the prompt now
// fires (checking existingArtifacts's count, not one hardcoded filename)
// and, on a yes, only the missing two files are created.
func TestQuickstartEnsureRepoReadyOffersScaffoldForPartiallyOnboardedRepo(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	architecturePath := filepath.Join(dir, "ARCHITECTURE.md")
	original := "# Architecture\n\nHand-written, pre-existing.\n"
	if err := os.WriteFile(architecturePath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\techo makefile-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", "ARCHITECTURE.md", "Makefile")
	runGit("commit", "-q", "-m", "initial commit")

	opts := &quickstartOptions{NonInteractive: false, PreflightProfile: "brownfield", VerifyCommand: "make test", VerifyCommandExplicit: true}
	var out bytes.Buffer
	_, _, _, _, err := quickstartEnsureRepoReady(dp, opts, newQuickstartPrompter(strings.NewReader("y\n")), &out, dir)
	if err != nil {
		t.Fatalf("quickstartEnsureRepoReady: %v", err)
	}
	if !strings.Contains(out.String(), "Onboarding docs missing") {
		t.Errorf("output = %q, want the scaffold prompt to fire for a repo missing spec/", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "spec", "spec.md")); err != nil {
		t.Errorf("expected spec/spec.md to be scaffolded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "spec", "contract.md")); err != nil {
		t.Errorf("expected spec/contract.md to be scaffolded: %v", err)
	}
	gotArchitecture, err := os.ReadFile(architecturePath)
	if err != nil {
		t.Fatalf("read ARCHITECTURE.md: %v", err)
	}
	if string(gotArchitecture) != original {
		t.Errorf("ARCHITECTURE.md = %q, want it left byte-identical to the original", gotArchitecture)
	}
}

// ---- worker readiness probe ------------------------------------------

// TestQuickstartWorkerLockHeldTrueWhileActuallyLocked confirms the
// probe reports true while a real acquireWorkerLock-style flock is
// held, and that the probe itself does not release someone else's lock.
func TestQuickstartWorkerLockHeldTrueWhileActuallyLocked(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	release, err := acquireWorkerLock(dp, dataDir)
	if err != nil {
		t.Fatalf("acquireWorkerLock: %v", err)
	}
	defer release()

	held, err := hostcontrol.QuickstartWorkerLockHeld(dataDir)
	if err != nil {
		t.Fatalf("quickstartWorkerLockHeld: %v", err)
	}
	if !held {
		t.Error("held = false, want true: acquireWorkerLock is still holding the lock")
	}

	// The probe must not have released the real lock out from under its
	// holder: a second real acquisition attempt must still fail.
	if _, err := acquireWorkerLock(dp, dataDir); err == nil {
		t.Error("a second acquireWorkerLock succeeded; the probe must have released the held lock")
	}
}

// ---- Codex review findings on PR #161 -----------------------------------

// TestQuickstartPullDoctorInputsUsesExistingConfigImages is the
// regression test for the Codex finding that quickstart's pull-focused
// preflight always checked the three canonical images, even when an
// already-valid session config it is about to reuse names locally built
// or private ones instead -- rejecting an offline operator whose actual
// worker would never touch a remote registry at all.
func TestQuickstartPullDoctorInputsUsesExistingConfigImages(t *testing.T) {
	sandboxRef := "localhost:5050/sandbox@sha256:aaa"
	registryRef := "localhost:5050/registry-proxy@sha256:ccc"
	existing := &sessionconfig.Config{
		SandboxImage:       &sandboxRef,
		RegistryProxyImage: &registryRef,
	}
	in := quickstartPullDoctorInputs("docker", existing)
	if in.sandboxImage != sandboxRef {
		t.Errorf("sandboxImage = %q, want %q", in.sandboxImage, sandboxRef)
	}
	if in.registryProxyImage != registryRef {
		t.Errorf("registryProxyImage = %q, want %q", in.registryProxyImage, registryRef)
	}
}

// TestQuickstartPullDoctorInputsDefaultsWithNoExistingConfig confirms a
// fresh (no existing config) run checks against no image at all -- there
// is no built-in default to fall back to, so every image check fails
// until one is configured or built (existing being nil must not silently
// invent an image name).
func TestQuickstartPullDoctorInputsDefaultsWithNoExistingConfig(t *testing.T) {
	in := quickstartPullDoctorInputs("docker", nil)
	if in.sandboxImage != "" {
		t.Errorf("sandboxImage = %q, want empty (no built-in default)", in.sandboxImage)
	}
	if in.registryProxyImage != "" {
		t.Errorf("registryProxyImage = %q, want empty (no built-in default)", in.registryProxyImage)
	}
	if in.releaseMaxFilesChanged != quickstartDefaultReleaseMaxFilesChanged || in.releaseMaxInsertions != quickstartDefaultReleaseMaxInsertions || in.releaseRollbackPlan != quickstartDefaultReleaseRollbackPlan {
		t.Errorf("release policy = (%d, %d, %q), want the usable quickstart defaults (a fresh config gets these) -- a zero-value release policy makes doctorCheckReleasePolicy always warn", in.releaseMaxFilesChanged, in.releaseMaxInsertions, in.releaseRollbackPlan)
	}
}

// TestQuickstartPullDoctorInputsUsesExistingConfigReleasePolicy is a
// regression test: a live walk saw quickstart's own preflight warn "the
// effective release policy denies every PR unconditionally" even though
// the config it was about to use already had usable release_* keys --
// doctorChecksFor
// always appends doctorCheckReleasePolicy, and quickstartPullDoctorInputs
// used to leave releaseMaxFilesChanged/releaseMaxInsertions/
// releaseRollbackPlan at doctorInputs' own zero value regardless of what
// existing (already loaded) actually set, so the check evaluated a
// hardcoded 0/0/"" instead of the config quickstart was really about to
// use.
func TestQuickstartPullDoctorInputsUsesExistingConfigReleasePolicy(t *testing.T) {
	maxFiles := 10
	maxInsertions := 500
	rollbackPlan := "git revert"
	existing := &sessionconfig.Config{
		ReleaseMaxFilesChanged: &maxFiles,
		ReleaseMaxInsertions:   &maxInsertions,
		ReleaseRollbackPlan:    &rollbackPlan,
	}
	in := quickstartPullDoctorInputs("docker", existing)
	if in.releaseMaxFilesChanged != maxFiles {
		t.Errorf("releaseMaxFilesChanged = %d, want the existing config's %d", in.releaseMaxFilesChanged, maxFiles)
	}
	if in.releaseMaxInsertions != maxInsertions {
		t.Errorf("releaseMaxInsertions = %d, want the existing config's %d", in.releaseMaxInsertions, maxInsertions)
	}
	if in.releaseRollbackPlan != rollbackPlan {
		t.Errorf("releaseRollbackPlan = %q, want the existing config's %q", in.releaseRollbackPlan, rollbackPlan)
	}
	// The false-alarm check itself: with these values populated,
	// doctorCheckReleasePolicy must not warn.
	check := doctorCheckReleasePolicy(in.releaseMaxFilesChanged, in.releaseMaxInsertions, in.releaseRollbackPlan)
	if check.Err != nil {
		t.Errorf("doctorCheckReleasePolicy = %v, want no error -- the existing config already has usable release defaults", check.Err)
	}
}

// TestQuickstartResolveExistingConfigPrefersLegacyPathWhenXDGAbsent is the
// regression test for the Codex finding that quickstart, with -config
// left unset, always resolved to DefaultPaths()[0] (the XDG path) even
// when only the legacy ~/.factory/config.yml existed -- so an operator
// with a working legacy config got prompted for a fresh route and had a
// second, divergent config written next to the one that already worked.
func TestQuickstartResolveExistingConfigPrefersLegacyPathWhenXDGAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	legacyPath := filepath.Join(home, ".factory", "config.yml")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("routes:\n  local:\n    upstream: https://legacy.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := &quickstartOptions{}
	gotPath, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	if gotPath != legacyPath {
		t.Errorf("path = %q, want the legacy path %q", gotPath, legacyPath)
	}
	if existing == nil || existing.Routes["local"].Upstream != "https://legacy.example" {
		t.Errorf("existing = %+v, want the legacy-path config loaded", existing)
	}
}

// TestQuickstartCredentialEnvForReusedConfigPreservesExplicitCredential is
// the regression test for the Codex finding that an operator's explicit
// -credential was silently discarded when quickstart reused an existing
// config, leaving a subsequently spawned worker with no credential in
// its environment at all.
func TestQuickstartCredentialEnvForReusedConfigPreservesExplicitCredential(t *testing.T) {
	opts := &quickstartOptions{Credential: "secret-value", CredentialProvided: true}

	env := quickstartCredentialEnvForReusedConfig(opts, &sessionconfig.Config{})
	if len(env) != 1 || env[0] != "ANTHROPIC_API_KEY=secret-value" {
		t.Errorf("env = %v, want [ANTHROPIC_API_KEY=secret-value] for a non-Copilot config", env)
	}

	copilotCfg := &sessionconfig.Config{
		Routes: map[string]sessionconfig.Route{
			"copilot": {CredentialMode: meter.CredentialModeGitHubCopilot},
		},
		Models: map[string]sessionconfig.Model{
			"copilot-model": {ID: "copilot-model", Routes: []string{"copilot"}},
		},
		Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "copilot-model"}},
	}
	env = quickstartCredentialEnvForReusedConfig(opts, copilotCfg)
	if len(env) != 1 || env[0] != "GITHUB_COPILOT_TOKEN=secret-value" {
		t.Errorf("env = %v, want [GITHUB_COPILOT_TOKEN=secret-value] for a Copilot config", env)
	}

	if env := quickstartCredentialEnvForReusedConfig(&quickstartOptions{}, &sessionconfig.Config{}); env != nil {
		t.Errorf("env = %v, want nil when no -credential was provided", env)
	}
}

// TestQuickstartServiceDrainsSelectedQueueComparesArguments is the
// regression test for the Codex finding that quickstart trusted any
// "loaded, running" launchd worker service regardless of which
// -config/-data-dir it actually launches with, so selecting a different
// config/data directory than an already-running service used left the
// just-submitted request's queue never serviced at all.
func TestQuickstartServiceDrainsSelectedQueueComparesArguments(t *testing.T) {
	dir := t.TempDir()
	plistPath := filepath.Join(dir, "dev.factoryd.worker.plist")
	configPath := filepath.Join(dir, "config.yml")
	dataDir := filepath.Join(dir, "data")
	plist := buildWorkerPlist(workerPlistConfig{
		BinaryPath: "/usr/local/bin/factoryd",
		ConfigPath: configPath,
		DataDir:    dataDir,
		HomeDir:    dir,
	})
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatal(err)
	}

	matches, err := hostcontrol.QuickstartServiceDrainsSelectedQueue(plistPath, configPath, dataDir)
	if err != nil {
		t.Fatalf("quickstartServiceDrainsSelectedQueue: %v", err)
	}
	if !matches {
		t.Error("matches = false, want true when -config/-data-dir match exactly")
	}

	matches, err = hostcontrol.QuickstartServiceDrainsSelectedQueue(plistPath, filepath.Join(dir, "other-config.yml"), dataDir)
	if err != nil {
		t.Fatalf("quickstartServiceDrainsSelectedQueue: %v", err)
	}
	if matches {
		t.Error("matches = true, want false when -config differs")
	}

	matches, err = hostcontrol.QuickstartServiceDrainsSelectedQueue(plistPath, configPath, filepath.Join(dir, "other-data"))
	if err != nil {
		t.Fatalf("quickstartServiceDrainsSelectedQueue: %v", err)
	}
	if matches {
		t.Error("matches = true, want false when -data-dir differs")
	}
}

// TestQuickstartEnsureDaemonRestartsAliveChildWhenConfigRewritten is the
// regression test for the Codex finding that a still-alive spawned child
// from a previous `factoryd quickstart` was left running untouched even
// when this invocation just rewrote the session config it was started
// with -- worker resolves its own settings once at startup, so the
// running request would silently keep using the previous model,
// resource limits, and release policy.
func TestQuickstartEnsureDaemonRestartsAliveChildWhenConfigRewritten(t *testing.T) {
	dp := newTestDeps(t)
	// Isolate from this developer's own real ~/Library/LaunchAgents plist
	// and launchctl: quickstartEnsureDaemon's darwin branch runs
	// unconditionally, and without this a real installed service (or
	// simply a real `launchctl` binary reachable on PATH) could make this
	// test's outcome depend on this machine's own state rather than the
	// code under test (a Codex review on this PR, round 3).
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreLaunchctlBinary := dp.host.launchctlBinary()
	fakeLaunchctl := filepath.Join(home, "launchctl")
	if err := os.WriteFile(fakeLaunchctl, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHostOf(dp).launchctlBinaryFn = func() string { return fakeLaunchctl }
	defer func() { fakeHostOf(dp).launchctlBinaryFn = func() string { return restoreLaunchctlBinary } }()

	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "quickstart-queue-run.pid")

	// A real, still-alive process this test controls, standing in for a
	// previous quickstart's spawned worker child. Reaped in the
	// background as soon as it exits (mirroring host.spawnWorker's
	// own reaping goroutine) -- without this, this test's own process
	// (not a genuinely separate quickstart invocation, unlike the real
	// scenario) is sleeper's parent, so a SIGTERM'd-but-unreaped sleeper
	// would sit as a zombie that syscall.Kill(pid, 0) still reports alive,
	// making quickstartStopAlivePID's poll below time out for a reason
	// that has nothing to do with the code under test.
	sleeper := exec.Command("sleep", "300")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	go func() { _, _ = sleeper.Process.Wait() }()
	defer func() { _ = sleeper.Process.Kill() }()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	// The stand-in process is a real "sleep", not a "factoryd" binary, so
	// host.pidLooksLikeWorker's own `ps` check must be stubbed for
	// quickstartReadAlivePID to treat it as ours at all.
	restoreLooksLike := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(pid int) bool { return true }
	var spawned bool
	restoreSpawn := fakeHostOf(dp).spawnWorkerFn
	fakeHostOf(dp).spawnWorkerFn = func(w io.Writer, binaryPath, configPath, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
		spawned = true
		return nil
	}
	defer func() {
		fakeHostOf(dp).spawnWorkerFn = restoreSpawn
		fakeHostOf(dp).pidLooksLikeWorkerFn = restoreLooksLike
	}()

	var out bytes.Buffer
	if err := hostcontrol.QuickstartEnsureDaemon(dp, &out, "factoryd", filepath.Join(dataDir, "config.yml"), dataDir, nil, true, "localhost:7233"); err != nil {
		t.Fatalf("quickstartEnsureDaemon: %v", err)
	}
	if !spawned {
		t.Error("quickstartSpawnWorker was not called; want the stale child stopped and a fresh one spawned")
	}
	if syscall.Kill(sleeper.Process.Pid, 0) == nil {
		t.Error("the stand-in process is still alive; want it stopped (SIGTERM) before respawning")
	}
	if !strings.Contains(out.String(), "restarting it") {
		t.Errorf("output = %q, want a message explaining the restart", out.String())
	}
}

// TestQuickstartEnsureDaemonSkipsAliveChildWhenConfigNotRewritten confirms
// the existing "already running -- skip" behavior is unchanged when
// restartNeeded is false.
func TestQuickstartEnsureDaemonSkipsAliveChildWhenConfigNotRewritten(t *testing.T) {
	dp := newTestDeps(t)
	// Same isolation as TestQuickstartEnsureDaemonRestartsAliveChildWhenConfigRewritten.
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreLaunchctlBinary := dp.host.launchctlBinary()
	fakeLaunchctl := filepath.Join(home, "launchctl")
	if err := os.WriteFile(fakeLaunchctl, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHostOf(dp).launchctlBinaryFn = func() string { return fakeLaunchctl }
	defer func() { fakeHostOf(dp).launchctlBinaryFn = func() string { return restoreLaunchctlBinary } }()

	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "quickstart-queue-run.pid")
	sleeper := exec.Command("sleep", "300")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	defer func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() }()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreLooksLike := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(pid int) bool { return true }
	restoreSpawn := fakeHostOf(dp).spawnWorkerFn
	fakeHostOf(dp).spawnWorkerFn = func(w io.Writer, binaryPath, configPath, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
		t.Error("quickstartSpawnWorker must not be called when the config wasn't rewritten")
		return nil
	}
	defer func() {
		fakeHostOf(dp).spawnWorkerFn = restoreSpawn
		fakeHostOf(dp).pidLooksLikeWorkerFn = restoreLooksLike
	}()

	var out bytes.Buffer
	if err := hostcontrol.QuickstartEnsureDaemon(dp, &out, "factoryd", filepath.Join(dataDir, "config.yml"), dataDir, nil, false, ""); err != nil {
		t.Fatalf("quickstartEnsureDaemon: %v", err)
	}
	if syscall.Kill(sleeper.Process.Pid, 0) != nil {
		t.Error("the stand-in process was stopped; want it left alone when the config wasn't rewritten")
	}
}

// TestQuickstartEnsureDaemonWithoutTemporalIsAnErrorAndSpawnsNothing proves
// the worker is the only driver: an empty Temporal address is refused with the
// fix named, never answered by spawning anything else.
func TestQuickstartEnsureDaemonWithoutTemporalIsAnErrorAndSpawnsNothing(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreLaunchctlBinary := dp.host.launchctlBinary()
	fakeLaunchctl := filepath.Join(home, "launchctl")
	if err := os.WriteFile(fakeLaunchctl, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHostOf(dp).launchctlBinaryFn = func() string { return fakeLaunchctl }
	restoreSpawn := fakeHostOf(dp).spawnWorkerFn
	fakeHostOf(dp).spawnWorkerFn = func(io.Writer, string, string, string, []string, string, string) error {
		t.Error("quickstartSpawnWorker must not be called without a Temporal address")
		return nil
	}
	defer func() {
		fakeHostOf(dp).launchctlBinaryFn = func() string { return restoreLaunchctlBinary }
		fakeHostOf(dp).spawnWorkerFn = restoreSpawn
	}()
	dataDir := t.TempDir()
	err := hostcontrol.QuickstartEnsureDaemon(dp, io.Discard, "factoryd", filepath.Join(dataDir, "config.yml"), dataDir, nil, false, "")
	if err == nil || !strings.Contains(err.Error(), "the worker needs Temporal") || !strings.Contains(err.Error(), "factoryd doctor -fix") {
		t.Fatalf("quickstartEnsureDaemon with no Temporal = %v, want the worker-needs-Temporal error naming `factoryd doctor -fix`", err)
	}
}

// TestQuickstartPrompterAskSecretFallsBackWhenNotATerminal confirms
// askSecret behaves exactly like ask when rawIn isn't a real terminal
// (the case for every non-interactive test, and for a piped stdin) --
// the no-echo path only activates against an actual *os.File terminal.
func TestQuickstartPrompterAskSecretFallsBackWhenNotATerminal(t *testing.T) {
	p := newQuickstartPrompter(strings.NewReader("my-secret\n"))
	var out bytes.Buffer
	got, err := p.askSecret(&out, "Credential: ")
	if err != nil {
		t.Fatalf("askSecret: %v", err)
	}
	if got != "my-secret" {
		t.Errorf("got = %q, want %q", got, "my-secret")
	}
}

// ---- fable review findings (round 3) on PR #161 --------------------------

// TestQuickstartWriteConfigRoundTripsWithoutClobberingDefaults is the
// regression test for the P1 finding that quickstartWriteConfig's plain
// yaml.Marshal of a *sessionconfig.Config emitted `[]`/`{}`/`null` for
// every field quickstart itself never set, since Config carried no
// `,omitempty` tags. `[]`/`{}` both round-trip through sessionconfig.Load
// into a non-nil-but-empty value, indistinguishable from an operator
// deliberately emptying a field -- so ApplySettings honored the marshaled
// `compose_services_allowed_registries: []` as a real override of the
// non-empty `docker.io/library/` default, meaning every quickstart-
// written config rejected every compose service in any target repo with
// one. Fixed by adding `,omitempty` to every Config field
// (internal/sessionconfig/sessionconfig.go) rather than only working
// around it here, since quickstartWriteConfig is the only place in this
// codebase that yaml.Marshal's a *Config at all.
func TestQuickstartWriteConfigRoundTripsWithoutClobberingDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	cfg := &sessionconfig.Config{
		Routes: map[string]sessionconfig.Route{
			"local": {Upstream: "http://127.0.0.1:8080"},
		},
		Models: map[string]sessionconfig.Model{
			"local-model": {ID: "local-model", Routes: []string{"local"}},
		},
	}
	if err := quickstartWriteConfig(configPath, cfg); err != nil {
		t.Fatalf("quickstartWriteConfig: %v", err)
	}

	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("sessionconfig.Load: %v", err)
	}
	if reloaded.ComposeServicesAllowedRegistries != nil {
		t.Errorf("ComposeServicesAllowedRegistries = %v, want nil (absent), not an explicit empty override", reloaded.ComposeServicesAllowedRegistries)
	}

	settings, err := reloaded.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	want := sessionconfig.DefaultSettings().ComposeServicesAllowedRegistries
	if len(settings.ComposeServicesAllowedRegistries) != len(want) {
		t.Errorf("ComposeServicesAllowedRegistries = %v, want the default %v to survive the round-trip", settings.ComposeServicesAllowedRegistries, want)
	}
}

// TestQuickstartBuildConfigOpenAIAlwaysPromptsForCredentialInteractively is
// the regression test for the P2 finding that an inherited
// ANTHROPIC_API_KEY silently skipped the "Does this endpoint require a
// credential?" confirmation entirely, so a real Anthropic key could be
// configured for (and later sent to) an unrelated OpenAI-compatible host
// without the operator ever being asked.
func TestQuickstartBuildConfigOpenAIAlwaysPromptsForCredentialInteractively(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("ANTHROPIC_API_KEY", "leaked-from-shell")
	opts := &quickstartOptions{
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1/",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	var out bytes.Buffer
	// Answering "n" here must be honored -- if the env var silently
	// decided needsCredential instead, this would come back credentialed
	// regardless of the answer given.
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("n\n")), &out, "/tmp/quickstart-data")
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if !strings.Contains(out.String(), "Does this endpoint require a credential?") {
		t.Errorf("output = %q, want the confirmation prompt to have been shown despite ANTHROPIC_API_KEY being set", out.String())
	}
	if len(credentialEnv) != 0 {
		t.Errorf("credentialEnv = %v, want none: the operator answered no", credentialEnv)
	}
	if !cfg.Routes["local"].AllowNoCredential {
		t.Errorf("Routes[local].AllowNoCredential = %v, want true", cfg.Routes["local"].AllowNoCredential)
	}
}

// TestQuickstartFindInFlightRequestFindsNonTerminalRequestForRepo is the
// regression test for the P2 finding that a rerun of `factoryd quickstart`
// against a repo with a request already in flight submitted a duplicate --
// release.RejectProjectCollision only rejects a DIFFERENT repository root
// claiming the same project name, not the same repo resubmitting.
func TestQuickstartFindInFlightRequestFindsNonTerminalRequestForRepo(t *testing.T) {
	dataDir := t.TempDir()
	repoRoot := "/repo/a"

	found, err := quickstartFindInFlightRequest(dataDir, repoRoot)
	if err != nil {
		t.Fatalf("quickstartFindInFlightRequest: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %v, want nil when no requests exist yet", found)
	}

	mustSaveRequest := func(id, workspace string, state request.State) {
		t.Helper()
		if err := request.SaveText(dataDir, id, "goal text"); err != nil {
			t.Fatal(err)
		}
		r := request.New(id, workspace, filepath.Base(workspace), request.Source{Kind: request.SourceText}, time.Now())
		r.State = state
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}

	// A terminal request against the same repo must not count as in flight.
	mustSaveRequest("req-done", repoRoot, request.StateDone)
	found, err = quickstartFindInFlightRequest(dataDir, repoRoot)
	if err != nil {
		t.Fatalf("quickstartFindInFlightRequest: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %v, want nil: the only request against this repo is terminal (done)", found)
	}

	// A non-terminal request against a DIFFERENT repo must not count either.
	mustSaveRequest("req-other-repo", "/repo/b", request.StateSpecReview)
	found, err = quickstartFindInFlightRequest(dataDir, repoRoot)
	if err != nil {
		t.Fatalf("quickstartFindInFlightRequest: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %v, want nil: the only non-terminal request is against a different repo", found)
	}

	// A non-terminal request against THIS repo must be found.
	mustSaveRequest("req-in-flight", repoRoot, request.StateSpecReview)
	found, err = quickstartFindInFlightRequest(dataDir, repoRoot)
	if err != nil {
		t.Fatalf("quickstartFindInFlightRequest: %v", err)
	}
	if found == nil || found.ID != "req-in-flight" {
		t.Fatalf("found = %v, want the in-flight request against this repo", found)
	}
}

// ---- Codex review round 2 findings on PR #161 ---------------------------

// TestQuickstartBuildConfigDoesNotForceRegistryProxyWithoutSandboxImage is
// the regression test for the Codex finding (P1) that quickstart's
// generated config unconditionally set registry_proxy: true while leaving
// sandbox_image unset on the normal successful-pull path -- worker_config.go's
// own startup check refuses -registry-proxy without -sandbox-image
// whenever the proxy setting is "deliberate" (an explicit config key
// counts), so every quickstart-written config would have made the very
// next worker spawn refuse to start.
func TestQuickstartBuildConfigDoesNotForceRegistryProxyWithoutSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1/",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, "/tmp/quickstart-data")
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if cfg.SandboxImage != nil {
		t.Fatalf("SandboxImage = %v, want unset (no -fix build happened on this path)", *cfg.SandboxImage)
	}
	if cfg.RegistryProxy != nil {
		t.Errorf("RegistryProxy = %v, want unset so worker's own non-deliberate default-on behavior applies instead of tripping its -sandbox-image requirement", *cfg.RegistryProxy)
	}
}

// TestQuickstartEnsureConfigReuseSignalsRestartNeeded is the regression
// test for the Codex findings (P2 x2) that reusing a config with a freshly
// supplied -credential, or with its own image refs just updated from
// opts.builtImageRefs, both left restartNeeded false -- so an
// already-running daemon kept using the stale credential/image
// indefinitely despite quickstart appearing to have picked up the change.
func TestQuickstartEnsureConfigReuseSignalsRestartNeeded(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	// Release policy keys are pre-set so the release-defaults backfill
	// (quickstartBackfillReleaseDefaults) is a no-op here and doesn't mask
	// what this test is actually about -- see
	// TestQuickstartEnsureConfigReuseBackfillsMissingReleaseDefaults for
	// that behavior's own dedicated coverage.
	if err := os.WriteFile(configPath, []byte("data_dir: "+filepath.Join(dir, "data")+"\nrelease_max_files_changed: 25\nrelease_max_insertions: 1000\nrelease_rollback_plan: \"git revert the merge commit on main\"\n"+quickstartTestRolesYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("nothing changed", func(t *testing.T) {
		opts := &quickstartOptions{ConfigPath: configPath}
		_, existing, err := quickstartResolveExistingConfig(opts)
		if err != nil {
			t.Fatalf("quickstartResolveExistingConfig: %v", err)
		}
		_, _, restartNeeded, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, configPath, existing)
		if err != nil {
			t.Fatalf("quickstartEnsureConfig: %v", err)
		}
		if restartNeeded {
			t.Error("restartNeeded = true, want false: nothing about the reused config changed")
		}
	})

	t.Run("explicit credential supplied", func(t *testing.T) {
		opts := &quickstartOptions{ConfigPath: configPath, Credential: "new-secret", CredentialProvided: true}
		_, existing, err := quickstartResolveExistingConfig(opts)
		if err != nil {
			t.Fatalf("quickstartResolveExistingConfig: %v", err)
		}
		_, credentialEnv, restartNeeded, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, configPath, existing)
		if err != nil {
			t.Fatalf("quickstartEnsureConfig: %v", err)
		}
		if !restartNeeded {
			t.Error("restartNeeded = false, want true: an explicit -credential was supplied alongside the reused config")
		}
		if len(credentialEnv) != 1 || credentialEnv[0] != "ANTHROPIC_API_KEY=new-secret" {
			t.Errorf("credentialEnv = %v", credentialEnv)
		}
	})

	t.Run("built image refs applied", func(t *testing.T) {
		opts := &quickstartOptions{ConfigPath: configPath, builtImageRefs: map[string]string{"-sandbox-image": "localhost:5050/x@sha256:new"}}
		_, existing, err := quickstartResolveExistingConfig(opts)
		if err != nil {
			t.Fatalf("quickstartResolveExistingConfig: %v", err)
		}
		_, _, restartNeeded, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, configPath, existing)
		if err != nil {
			t.Fatalf("quickstartEnsureConfig: %v", err)
		}
		if !restartNeeded {
			t.Error("restartNeeded = false, want true: opts.builtImageRefs just updated the reused config's own image refs")
		}
	})
}

// TestQuickstartResolveExistingConfigDuringReconfigureUsesExistingDefaultPath
// is the regression test for the Codex finding (P2) that -reconfigure with
// -config left unset always picked DefaultPaths()[0] (the XDG path)
// regardless of an existing legacy ~/.factory/config.yml, writing a second,
// divergent config instead of rewriting the one actually in use.
func TestQuickstartResolveExistingConfigDuringReconfigureUsesExistingDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	legacyPath := filepath.Join(home, ".factory", "config.yml")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("relay_worker_model_id: legacy-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := &quickstartOptions{Reconfigure: true}
	gotPath, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	if gotPath != legacyPath {
		t.Errorf("path = %q, want the legacy path %q rewritten in place", gotPath, legacyPath)
	}
	if existing != nil {
		t.Errorf("existing = %+v, want nil: -reconfigure means write fresh, not merge into the old content", existing)
	}
}

// TestQuickstartReconfigureKeepsOnlyRecordedImages: -reconfigure rewrites the
// config but keeps what `make install` recorded (image refs, image_source_root),
// and nothing the operator chose -- otherwise the rewrite's image check fails
// with "no sandbox image configured" on a freshly installed machine.
func TestQuickstartReconfigureKeepsOnlyRecordedImages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	path := filepath.Join(home, ".config", "factoryd", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64)
	body := "sandbox_image: " + sandbox + "\nimage_source_root: /src/buildgate\ncode_review_policy: required\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, existing, err := quickstartResolveExistingConfig(&quickstartOptions{Reconfigure: true})
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	if existing == nil || existing.SandboxImage == nil || *existing.SandboxImage != sandbox {
		t.Fatalf("existing = %+v, want the recorded sandbox_image kept", existing)
	}
	if existing.ImageSourceRoot == nil || *existing.ImageSourceRoot != "/src/buildgate" {
		t.Errorf("image_source_root not kept: %+v", existing.ImageSourceRoot)
	}
	if existing.CodeReviewPolicy != nil {
		t.Errorf("code_review_policy = %v, want it dropped: -reconfigure writes fresh", *existing.CodeReviewPolicy)
	}
	if quickstartConfigHasExecutionRole(existing) {
		t.Error("kept config has an execution role; quickstartEnsureConfig would reuse it instead of rewriting")
	}
}

// TestQuickstartResolveExistingConfigDuringReconfigureFallsBackWhenNothingExists
// confirms -reconfigure with nothing at any default path still falls back
// to DefaultPaths()[0], unchanged from before this fix.
func TestQuickstartResolveExistingConfigDuringReconfigureFallsBackWhenNothingExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	opts := &quickstartOptions{Reconfigure: true}
	gotPath, existing, err := quickstartResolveExistingConfig(opts)
	if err != nil {
		t.Fatalf("quickstartResolveExistingConfig: %v", err)
	}
	want := filepath.Join(home, ".config", "factoryd", "config.yml")
	if gotPath != want {
		t.Errorf("path = %q, want the default XDG path %q", gotPath, want)
	}
	if existing != nil {
		t.Error("existing = non-nil, want nil when nothing exists yet")
	}
}

// TestQuickstartPullDoctorInputsSkipsRegistryProxyImageWhenDisabled is the
// regression test for the Codex finding (P2) that an existing config's own
// registry_proxy: false was ignored, so an offline operator whose worker
// would never launch a registry proxy at all was still rejected over that
// image never being present.
func TestQuickstartPullDoctorInputsSkipsRegistryProxyImageWhenDisabled(t *testing.T) {
	off := false
	existing := &sessionconfig.Config{RegistryProxy: &off}
	in := quickstartPullDoctorInputs("docker", existing)
	if in.registryProxyImage != "" {
		t.Errorf("registryProxyImage = %q, want empty: the config explicitly disables the registry proxy", in.registryProxyImage)
	}
}

// TestQuickstartEnsureDaemonRestartsMatchingLaunchdServiceWhenRestartNeeded
// is the regression test for the Codex finding (P1) that a launchd service
// draining the exact -config/-data-dir this invocation resolved was still
// just skipped even when restartNeeded is true, leaving it running against
// the stale settings quickstart just rewrote.
func TestQuickstartEnsureDaemonRestartsMatchingLaunchdServiceWhenRestartNeeded(t *testing.T) {
	dp := newTestDeps(t)
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only; quickstartEnsureDaemon's own darwin check short-circuits on this GOOS")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "data")
	configPath := filepath.Join(home, "config.yml")

	binary := filepath.Join(home, "factoryd")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := buildWorkerPlist(workerPlistConfig{
		BinaryPath: binary,
		ConfigPath: configPath,
		DataDir:    dataDir,
		HomeDir:    home,
	})
	plistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		t.Fatalf("workerPlistPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatal(err)
	}

	fakeLaunchctl := filepath.Join(home, "launchctl")
	if err := os.WriteFile(fakeLaunchctl, []byte("#!/bin/sh\necho 'state = running'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	restoreBinary := dp.host.launchctlBinary()
	fakeHostOf(dp).launchctlBinaryFn = func() string { return fakeLaunchctl }
	defer func() { fakeHostOf(dp).launchctlBinaryFn = func() string { return restoreBinary } }()

	var kickstartArgs []string
	restoreRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) {
		kickstartArgs = args
		return []byte("ok"), nil
	}
	defer func() { fakeHostOf(dp).launchctlFn = restoreRun }()

	var out bytes.Buffer
	if err := hostcontrol.QuickstartEnsureDaemon(dp, &out, binary, configPath, dataDir, nil, true, ""); err != nil {
		t.Fatalf("quickstartEnsureDaemon: %v", err)
	}
	if len(kickstartArgs) < 1 || kickstartArgs[0] != "kickstart" {
		t.Errorf("kickstartArgs = %v, want a launchctl kickstart call", kickstartArgs)
	}
	if !strings.Contains(out.String(), "restarted it") {
		t.Errorf("output = %q, want a message about restarting the service", out.String())
	}
}

// TestQuickstartEnsureDaemonSkipsMatchingLaunchdServiceWhenRestartNotNeeded
// confirms the plain "already managed by launchd -- skipping" behavior is
// unchanged (no kickstart call) when restartNeeded is false.
func TestQuickstartEnsureDaemonSkipsMatchingLaunchdServiceWhenRestartNotNeeded(t *testing.T) {
	dp := newTestDeps(t)
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only; quickstartEnsureDaemon's own darwin check short-circuits on this GOOS")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "data")
	configPath := filepath.Join(home, "config.yml")

	binary := filepath.Join(home, "factoryd")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := buildWorkerPlist(workerPlistConfig{
		BinaryPath: binary,
		ConfigPath: configPath,
		DataDir:    dataDir,
		HomeDir:    home,
	})
	plistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		t.Fatalf("workerPlistPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatal(err)
	}

	fakeLaunchctl := filepath.Join(home, "launchctl")
	if err := os.WriteFile(fakeLaunchctl, []byte("#!/bin/sh\necho 'state = running'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	restoreBinary := dp.host.launchctlBinary()
	fakeHostOf(dp).launchctlBinaryFn = func() string { return fakeLaunchctl }
	defer func() { fakeHostOf(dp).launchctlBinaryFn = func() string { return restoreBinary } }()

	restoreRun := fakeHostOf(dp).launchctlFn
	fakeHostOf(dp).launchctlFn = func(args ...string) ([]byte, error) {
		t.Error("serviceLaunchctlRun must not be called (no kickstart) when restartNeeded is false")
		return []byte("ok"), nil
	}
	defer func() { fakeHostOf(dp).launchctlFn = restoreRun }()

	var out bytes.Buffer
	if err := hostcontrol.QuickstartEnsureDaemon(dp, &out, binary, configPath, dataDir, nil, false, ""); err != nil {
		t.Fatalf("quickstartEnsureDaemon: %v", err)
	}
	if !strings.Contains(out.String(), "skipping") {
		t.Errorf("output = %q, want the plain skip message", out.String())
	}
}

// quickstart refuses a repository without a committed AGENTS.md right after it
// resolves the repository: it prints nothing more, so no image, config,
// daemon or submit step ran. An AGENTS.md only in the working tree is not one.
func TestQuickstartRefusesARepositoryWithoutAgentsFileBeforeAnySetup(t *testing.T) {
	repo := commitFiles(t, map[string]string{"add.py": "def add(a, b):\n    return a + b\n"})
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# AGENTS.md\n\n- Test: `python3 -m unittest`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var out bytes.Buffer
	err := runQuickstart(newTestDeps(t), &quickstartOptions{NonInteractive: true, Goal: "Add multiply"}, repo, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "has no AGENTS.md committed at its root") || !strings.Contains(err.Error(), "setup, test, build and lint commands") {
		t.Fatalf("runQuickstart = %v, want the AGENTS.md refusal and its fix", err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "Repository: ") {
		t.Fatalf("quickstart printed more than the repository line before refusing:\n%s", out.String())
	}
}
