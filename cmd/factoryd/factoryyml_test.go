package main

import (
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
)

// TestFactoryYMLContentSharedByInitAndOnboard is the regression guard for
// factoryYMLContent's move out of init.go into its own file: init.go's own
// TestInitWithWriteFactoryYMLWritesFile et al. already prove initMain's
// end-to-end behavior is unchanged, so this proves the extracted helper
// itself still produces the exact byte layout both callers depend on --
// verify_command line first (when non-empty), preflight_profile always
// last, one call site parameterizing the profile rather than each caller
// building its own string.
func TestFactoryYMLContentSharedByInitAndOnboard(t *testing.T) {
	t.Parallel()
	const commentedGateDefaults = "# lint_command: \"golangci-lint run ./...\"\n" +
		"# security_command: \"govulncheck ./...\"\n" +
		"# unit_test_command: \"go test ./...\"\n" +
		"# integration_test_command: \"go test -tags=integration ./...\"\n" +
		"# reference_oracle_command: \"\" # e.g. a script diffing candidate output against an independent oracle -- must NOT be agent-authored or editable via the ticket's Allowed-Files\n"
	if got, want := factoryYMLContent("go test ./...", "brownfield"), "verify_command: \"go test ./...\"\npreflight_profile: brownfield\n"+commentedGateDefaults; got != want {
		t.Errorf("factoryYMLContent = %q, want %q", got, want)
	}
	if got, want := factoryYMLContent("", "brownfield"), "preflight_profile: brownfield\n"+commentedGateDefaults; got != want {
		t.Errorf("factoryYMLContent with no verify command = %q, want %q", got, want)
	}
}

// TestFactoryYMLCommitReminderNamesTheFileAndTheCommittedRead pins the
// one thing this line has to convey: writing .factory.yml is not enough,
// because projectconfig.Load reads the committed HEAD revision and
// ignores an uncommitted worktree copy outright. Shared by initMain and
// onboardMain so neither can drift into saying something different.
func TestFactoryYMLCommitReminderNamesTheFileAndTheCommittedRead(t *testing.T) {
	t.Parallel()
	got := factoryYMLCommitReminder()
	if !strings.Contains(got, projectconfig.FileName) {
		t.Errorf("factoryYMLCommitReminder = %q, want it to name %s", got, projectconfig.FileName)
	}
	for _, want := range []string{"Commit", "committed", "ignored"} {
		if !strings.Contains(got, want) {
			t.Errorf("factoryYMLCommitReminder = %q, want it to contain %q", got, want)
		}
	}
}

// quickstart submits in the same invocation, so its note must not tell the
// operator to commit "before running anything".
func TestFactoryYMLQuickstartNoteDoesNotBlockTheCurrentRun(t *testing.T) {
	t.Parallel()
	got := factoryYMLQuickstartNote()
	for _, want := range []string{"Commit " + projectconfig.FileName, "committed HEAD", "This run already uses"} {
		if !strings.Contains(got, want) {
			t.Errorf("factoryYMLQuickstartNote = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "before running anything") {
		t.Errorf("factoryYMLQuickstartNote = %q, must not say to commit before running anything", got)
	}
}
