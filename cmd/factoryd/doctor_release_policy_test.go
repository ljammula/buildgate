package main

import (
	"strings"
	"testing"
)

// TestDoctorCheckReleasePolicyWarnsWhenDenyingAll is the regression test
// for the doctor-visibility half of the deny-all release policy gap:
// before this check existed, a
// session config with no release_max_files_changed/release_max_insertions/
// release_rollback_plan set (MergePolicyCheck's own always-deny case) was
// invisible to `factoryd doctor` -- an operator only learned about it
// after a first accepted run silently produced no pull request.
func TestDoctorCheckReleasePolicyWarnsWhenDenyingAll(t *testing.T) {
	t.Parallel()
	c := doctorCheckReleasePolicy(0, 0, "")
	if c.Err == nil {
		t.Fatal("Err = nil, want a diagnosis for a deny-all release policy")
	}
	if !c.Advisory {
		t.Error("Advisory = false, want true: this must warn, not fail worker's shared preflight -- see this check's own doc comment")
	}
	if !strings.Contains(c.Fix, "release_max_files_changed") || !strings.Contains(c.Fix, "release_max_insertions") || !strings.Contains(c.Fix, "release_rollback_plan") {
		t.Errorf("Fix = %q, want it to name all three session-config keys to add", c.Fix)
	}
}

// TestDoctorCheckReleasePolicyPassesWithUsableDefaults confirms the
// quickstart/init-config release defaults (25 files / 1000 insertions
// / a non-empty rollback plan) satisfy this check.
func TestDoctorCheckReleasePolicyPassesWithUsableDefaults(t *testing.T) {
	t.Parallel()
	c := doctorCheckReleasePolicy(25, 1000, "git revert the merge commit on main")
	if c.Err != nil {
		t.Errorf("Err = %v, want nil for a usable release policy", c.Err)
	}
}

// TestDoctorChecksForIncludesReleasePolicyCheck proves the check is
// actually wired into the shared doctorChecksFor list `factoryd doctor`
// and worker's own startup preflight both use, not just a standalone
// function nothing calls.
func TestDoctorChecksForIncludesReleasePolicyCheck(t *testing.T) {
	t.Parallel()
	in := doctorInputs{sandboxImage: "example/image@sha256:" + strings.Repeat("a", 64)}
	checks := doctorChecksFor(t.Context(), in)
	found := false
	for _, c := range checks {
		if c.Name == "release policy allows a PR" {
			found = true
			if c.Err == nil {
				t.Error("expected the zero-value doctorInputs release fields to trigger the warning")
			}
		}
	}
	if !found {
		t.Fatal("doctorChecksFor did not include the release policy check")
	}
}
