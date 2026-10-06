package main

import (
	"context"
	"strings"
	"testing"
)

// TestRunMainWithReadyRefusesLooserFactoryYMLTokenCeiling is the direct-
// run regression test for M3-E1: a workspace's committed .factory.yml may
// only TIGHTEN the session's relay token ceiling, never loosen it. With
// no session config present (isolateSessionConfig points HOME at an
// empty dir), the session's effective token ceiling is
// sessionconfig.DefaultSettings's own 5x-budget default (5,000,000): a
// .factory.yml declaring a higher token_ceiling must make
// runMainWithReady refuse the run -- before ever reaching a real ticket/
// spec/sandbox launch, which this fixture's own nonexistent -spec path
// would otherwise fail on for an unrelated reason.
func TestRunMainWithReadyRefusesLooserFactoryYMLTokenCeiling(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, "verify_command: \"true\"\ntoken_ceiling: 6000000\n")

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", workspace, "-spec", "/does/not/exist",
		// An explicit offline -build-app-script with no roles.execution, so
		// this run reaches applyProjectConfigDefaults with no route to
		// resolve first (modelRouteNeeded).
		"-build-app-script", "/does/not/exist/offline-build.sh",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady: err = nil, want a refusal for a .factory.yml token_ceiling looser than the session ceiling")
	}
	for _, want := range []string{"token_ceiling", "6000000", "5000000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}

// TestSubmitMainRefusesLooserFactoryYMLTokenCeiling is submitMain's own
// counterpart: `factoryd submit` against a workspace whose committed
// .factory.yml declares a token_ceiling looser than the resolved session
// config's effective ceiling must refuse the request, not accept it and
// silently launch at the tighter session ceiling later.
func TestSubmitMainRefusesLooserFactoryYMLTokenCeiling(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
token_ceiling: 6000000
`)
	dataDir := t.TempDir()

	err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Add a new func"})
	if err == nil {
		t.Fatal("submitMain: err = nil, want a refusal for a .factory.yml token_ceiling looser than the session ceiling")
	}
	for _, want := range []string{"token_ceiling", "6000000", "5000000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}
