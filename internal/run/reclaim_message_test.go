package run

import (
	"strings"
	"testing"
)

// The refusal sentence, without its "buildgate: ", fits the 200 bytes a
// triage sentence may have for every path: a path that would not fit is
// replaced whole by where the run records it.
func TestReclaimNotCheckedMessageFitsAndNeverCutsThePath(t *testing.T) {
	commit := strings.Repeat("0123456789ab", 4)[:40]
	got := ReclaimNotCheckedMessage("repository not found", "/Users/op/code/app", commit)
	want := "buildgate: reclaim refused, repository not found: /Users/op/code/app must hold commit 0123456789ab with the run's .factory.yml; then start the run again, or factoryd override"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	for n := 0; n < 400; n += 7 {
		path := "/" + strings.Repeat("d", n)
		msg := strings.TrimPrefix(ReclaimNotCheckedMessage("file is not a regular file", path, commit), "buildgate: ")
		if len(msg) > 200 {
			t.Fatalf("path of %d bytes: sentence is %d bytes", n, len(msg))
		}
		if !strings.Contains(msg, path+" must hold") && !strings.Contains(msg, "the run's repository_root must hold") {
			t.Fatalf("path of %d bytes: %q", n, msg)
		}
		if !strings.HasSuffix(msg, "factoryd override") {
			t.Fatalf("path of %d bytes: %q does not end with the next step", n, msg)
		}
	}
	if got, want := ReclaimNotCheckedMessage("no commit on the run record", "/repo", ""), "buildgate: reclaim refused, no commit on the run record for its .factory.yml: start the run again, or factoryd override"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if !(GateResult{Check: "canonical_verify", ExitCode: -1, Command: []string{got}}).ReclaimNotChecked() {
		t.Error("ReclaimNotChecked = false for the message")
	}
}
