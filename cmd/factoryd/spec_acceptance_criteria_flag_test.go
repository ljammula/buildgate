package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunMainWithReadyAcceptsSpecAcceptanceCriteriaFlag is the "one test
// that parses any new argv through the real flag set" requirement:
// -spec-acceptance-criteria must be a recognized flag on runMainWithReady's
// own flag.FlagSet, not just on buildTicketRunArgs's own
// argv-construction side (see TestBuildTicketRunArgsIncludesSpecAcceptanceCriteriaOnlyWhenSet
// for that half). A nonexistent -workspace/-spec means this run fails
// well before ever invoking build_app.py; what this test actually proves
// is that failure is NOT "flag provided but not defined: -spec-acceptance-criteria" —
// which is exactly the class of bug a merely-buildAppArgs-side test can't catch.
func TestRunMainWithReadyAcceptsSpecAcceptanceCriteriaFlag(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	criteria := filepath.Join(dir, "criteria.md")
	if err := os.WriteFile(criteria, []byte("1. Foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", dir, "-spec", spec,
		"-spec-acceptance-criteria", criteria,
	}, nil)
	if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("runMainWithReady rejected -spec-acceptance-criteria as an unknown flag: %v", err)
	}
}

// TestRunMainWithReadyAcceptsConformityPolicyFlag mirrors
// TestRunMainWithReadyAcceptsSpecAcceptanceCriteriaFlag above for
// -conformity-policy: the per-criterion conformity requirement lives in
// this flag alone (see buildAppArgs' own doc comment on
// --conformity-policy).
func TestRunMainWithReadyAcceptsConformityPolicyFlag(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	criteria := filepath.Join(dir, "criteria.md")
	if err := os.WriteFile(criteria, []byte("1. Foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", dir, "-spec", spec,
		"-spec-acceptance-criteria", criteria,
		"-conformity-policy", "advisory",
	}, nil)
	if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("runMainWithReady rejected -conformity-policy as an unknown flag: %v", err)
	}
}
