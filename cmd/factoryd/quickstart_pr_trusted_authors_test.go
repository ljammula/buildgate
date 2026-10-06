package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// withQuickstartGHAPIUserLogin overrides forge.userLogin for the
// duration of the test, restoring the package-wide stub (see
// integration_test.go's runTests) afterward. It swaps a package global, so
// a test calling it must not be t.Parallel: two such tests running at once
// read each other's stub (a -race failure in cmd/factoryd's shard 4, seen
// 2026-09-29). Sequential top-level tests never overlap parallel ones.
func withQuickstartGHAPIUserLogin(dp *deps, t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := fakeForgeOf(dp).userLoginFn
	fakeForgeOf(dp).userLoginFn = fn
	t.Cleanup(func() { fakeForgeOf(dp).userLoginFn = orig })
}

// TestQuickstartBuildConfigDefaultsPRTrustedAuthors is G17b: an operator
// who never sets -pr-trusted-authors gets it defaulted to their own gh
// login, and told so.
func TestQuickstartBuildConfigDefaultsPRTrustedAuthors(t *testing.T) {
	dp := newTestDeps(t)
	withQuickstartGHAPIUserLogin(dp, t, func() (string, error) { return "octocat", nil })

	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1/",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	var out bytes.Buffer
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, "/tmp/quickstart-data")
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if len(cfg.PRTrustedAuthors) != 1 || cfg.PRTrustedAuthors[0] != "octocat" {
		t.Errorf("PRTrustedAuthors = %v, want [octocat]", cfg.PRTrustedAuthors)
	}
	if !strings.Contains(out.String(), "octocat") {
		t.Errorf("output = %q, want it to mention the defaulted login", out.String())
	}
}

// TestQuickstartBuildConfigSkipsPRTrustedAuthorsWhenGHUnauthenticated
// proves the skip is silent (no error, no PRTrustedAuthors set) when gh
// isn't installed/authenticated -- quickstart must never fail onboarding
// over this convenience default.
func TestQuickstartBuildConfigSkipsPRTrustedAuthorsWhenGHUnauthenticated(t *testing.T) {
	dp := newTestDeps(t)
	withQuickstartGHAPIUserLogin(dp, t, func() (string, error) { return "", errors.New("gh: not authenticated") })

	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:1/",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	var out bytes.Buffer
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, "/tmp/quickstart-data")
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if cfg.PRTrustedAuthors != nil {
		t.Errorf("PRTrustedAuthors = %v, want nil when gh is unauthenticated", cfg.PRTrustedAuthors)
	}
	if strings.Contains(out.String(), "pr-trusted-authors") {
		t.Errorf("output = %q, want no mention of defaulting -pr-trusted-authors when gh failed", out.String())
	}
}
