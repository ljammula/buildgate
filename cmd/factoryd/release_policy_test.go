package main

import (
	"reflect"
	"testing"
)

// TestReleasePolicyFromFlagsDefaultsRequiredGatesToCanonicalAndFullSuite
// is the regression test for the 2026-09-05 Opus review finding S1: the
// production release.MergePolicy built from -release-* flags must
// require canonical_verify and full_suite_verify, not merely accept
// whatever subset happened to run. Without this, a run accepted on
// canonical_verify alone records a release decision indistinguishable in
// shape from a fully-gated one.
//
// Deliberately NOT policy.AllGateChecks (an earlier version of this
// test/default): found via a real local `codex review` pass on this same
// PR -- diff_scope/required_files_changed/required_content_present run
// only if the ticket declared the corresponding key, which isn't
// recorded anywhere on run.Run, so requiring them unconditionally denied
// release for every legitimately undeclared-scope ticket. See
// MergePolicy.RequiredGates' own doc comment for the full reasoning.
func TestReleasePolicyFromFlagsDefaultsRequiredGatesToCanonicalAndFullSuite(t *testing.T) {
	t.Parallel()
	got := releasePolicyFromFlags("", 0, 0, "", false, false, false, false)
	want := []string{"canonical_verify", "full_suite_verify"}
	if !reflect.DeepEqual(got.RequiredGates, want) {
		t.Errorf("releasePolicyFromFlags(...).RequiredGates = %v, want %v", got.RequiredGates, want)
	}
}

// TestReleasePolicyFromFlagsPropagatesAllowSkippedProjectCheck is the
// regression test for a real GitHub Codex App review finding on this PR:
// -skip-project-check is a documented, legitimate escape hatch, but
// releasePolicyFromFlags (and every -release-* flag set built on it) had
// no corresponding input, so release.MergePolicy.AllowSkippedProjectCheck
// was always false outside a direct Go caller -- an operator could
// enable the CLI escape hatch but had no way to configure the matching
// release-policy exception through any supported factoryd surface.
func TestReleasePolicyFromFlagsPropagatesAllowSkippedProjectCheck(t *testing.T) {
	t.Parallel()
	if got := releasePolicyFromFlags("", 0, 0, "", false, false, false, false); got.AllowSkippedProjectCheck {
		t.Errorf("AllowSkippedProjectCheck = true with the flag unset, want false")
	}
	if got := releasePolicyFromFlags("", 0, 0, "", false, false, false, true); !got.AllowSkippedProjectCheck {
		t.Errorf("AllowSkippedProjectCheck = false with the flag set, want true")
	}
}
