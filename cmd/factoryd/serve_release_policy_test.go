package main

import (
	"testing"

	"buildgate/internal/sessionconfig"
)

// TestResolveServeReleasePolicyPrefersSessionConfigWhenFlagsUnset is the
// regression test for `factoryd serve`'s own -release-* flags: before
// this fix they defaulted to their flag.FlagSet zero value (0
// files/insertions allowed, "" rollback plan) and that zero value always
// won over the on-disk session config, so every API-started run was
// denied release regardless of what config.yml actually configured.
func TestResolveServeReleasePolicyPrefersSessionConfigWhenFlagsUnset(t *testing.T) {
	t.Parallel()
	flags, sf := newServeFlags()
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	base := sessionconfig.DefaultSettings()
	base.ReleaseMaxFilesChanged = 25
	base.ReleaseMaxInsertions = 1000
	base.ReleaseRollbackPlan = "git revert the merge commit on main"
	base.ReleaseProtectedPaths = "safety-contract.md"
	base.ReleaseAllowOverrides = true

	got := resolveServeReleasePolicy(flags, base, sf)
	if got.MaxFilesChanged != 25 {
		t.Errorf("MaxFilesChanged = %d, want the session config's 25 (no -release-max-files-changed flag given)", got.MaxFilesChanged)
	}
	if got.MaxInsertions != 1000 {
		t.Errorf("MaxInsertions = %d, want the session config's 1000", got.MaxInsertions)
	}
	if got.RollbackPlan != "git revert the merge commit on main" {
		t.Errorf("RollbackPlan = %q, want the session config's value", got.RollbackPlan)
	}
	if len(got.ProtectedPaths) != 1 || got.ProtectedPaths[0] != "safety-contract.md" {
		t.Errorf("ProtectedPaths = %v, want [safety-contract.md]", got.ProtectedPaths)
	}
	if !got.AllowOverrides {
		t.Error("AllowOverrides = false, want true from the session config")
	}
}

// TestResolveServeReleasePolicyExplicitFlagWinsOverSessionConfig proves
// an operator-supplied -release-* flag on `serve` still overrides the
// session config, including an explicit value that happens to look like
// a zero value -- the same "explicit flag > config file" precedence
// worker's own applySessionConfig documents.
func TestResolveServeReleasePolicyExplicitFlagWinsOverSessionConfig(t *testing.T) {
	t.Parallel()
	flags, sf := newServeFlags()
	if err := flags.Parse([]string{
		"-release-max-files-changed", "3",
		"-release-rollback-plan", "roll back by hand",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	base := sessionconfig.DefaultSettings()
	base.ReleaseMaxFilesChanged = 25
	base.ReleaseMaxInsertions = 1000
	base.ReleaseRollbackPlan = "git revert the merge commit on main"

	got := resolveServeReleasePolicy(flags, base, sf)
	if got.MaxFilesChanged != 3 {
		t.Errorf("MaxFilesChanged = %d, want the explicit flag value 3", got.MaxFilesChanged)
	}
	if got.RollbackPlan != "roll back by hand" {
		t.Errorf("RollbackPlan = %q, want the explicit flag value", got.RollbackPlan)
	}
	// release-max-insertions was not passed on the command line, so it
	// must still come from the session config, not the flag default (0).
	if got.MaxInsertions != 1000 {
		t.Errorf("MaxInsertions = %d, want the session config's 1000 (flag not explicitly set)", got.MaxInsertions)
	}
}

// TestResolveServeReleasePolicyNoSessionConfigKeepsFlagDefaults proves a
// daemon with no session config release keys at all (sessionconfig.
// DefaultSettings' own zero values) and no -release-* flags produces the
// same always-deny policy as before this fix -- this change only lets a
// REAL config value win, it does not invent a new default of its own.
func TestResolveServeReleasePolicyNoSessionConfigKeepsFlagDefaults(t *testing.T) {
	t.Parallel()
	flags, sf := newServeFlags()
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	got := resolveServeReleasePolicy(flags, sessionconfig.DefaultSettings(), sf)
	if got.MaxFilesChanged != 0 || got.MaxInsertions != 0 || got.RollbackPlan != "" {
		t.Errorf("got = %+v, want the zero-value always-deny policy when neither flags nor session config set anything", got)
	}
}
