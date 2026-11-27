package main

import (
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
)

// TestFactoryYMLContentLayout pins the byte layout quickstart and
// `doctor -fix` write: the verify_command line first (when non-empty), then
// the commented gates example.
func TestFactoryYMLContentLayout(t *testing.T) {
	t.Parallel()
	const commentedGateDefaults = "# gates:\n" +
		"#   - id: lint\n" +
		"#     command: \"golangci-lint run ./...\"\n"
	if got, want := factoryYMLContent("go test ./..."), "verify_command: \"go test ./...\"\n"+commentedGateDefaults; got != want {
		t.Errorf("factoryYMLContent = %q, want %q", got, want)
	}
	if got, want := factoryYMLContent(""), commentedGateDefaults; got != want {
		t.Errorf("factoryYMLContent with no verify command = %q, want %q", got, want)
	}
}

// TestFactoryYMLCommitReminderNamesTheFileAndTheCommittedRead pins the
// one thing this line has to convey: writing .factory.yml is not enough,
// because projectconfig.Load reads the committed HEAD revision and
// ignores an uncommitted worktree copy outright.
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
