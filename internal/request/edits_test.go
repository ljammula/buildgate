package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRecordEditSnapshotsTheReplacedTextAndRecordsTheEdit: an in-place edit
// leaves the new text in the file, the old text in an edit-kind revision,
// and an Edit naming who, when, which file and what changed.
func TestRecordEditSnapshotsTheReplacedTextAndRecordsTheEdit(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)
	specPath := filepath.Join(Dir(dataDir, "req-1"), specFileName)
	before := "# Spec\n\n## Problem\n\nold line\nkept\n"
	after := "# Spec\n\n## Problem\n\nnew line\nkept\n"
	if err := os.WriteFile(specPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecordEdit(dataDir, r, "kanna", specFileName, after, fixedNow); err != nil {
		t.Fatalf("RecordEdit: %v", err)
	}

	onDisk, err := os.ReadFile(specPath)
	if err != nil || string(onDisk) != after {
		t.Fatalf("spec.md = %q (%v), want the edited text", onDisk, err)
	}
	reloaded, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Edits) != 1 {
		t.Fatalf("Edits = %+v, want one", reloaded.Edits)
	}
	got := reloaded.Edits[0]
	wantDiff := "  ## Problem\n  \n- old line\n+ new line\n  kept\n  \n"
	if got.By != "kanna" || got.At != "2026-09-11T12:00:00Z" || got.Path != "spec.md" || got.FromState != StateSpecReview || got.Revision != 1 || got.Diff != wantDiff || got.DiffTruncated {
		t.Errorf("Edit = %+v, want kanna/spec.md/revision 1 with diff %q", got, wantDiff)
	}
	rev, files, err := LoadRevision(dataDir, "req-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Kind != RevisionKindEdit || rev.FeedbackSupplied || rev.By != "kanna" || files[specFileName] != before {
		t.Errorf("revision = %+v files = %q, want an edit revision holding the replaced text", rev, files)
	}
}

// TestRecordEditOfTheSameTextRecordsNothing: a save that changes nothing is
// not an edit.
func TestRecordEditOfTheSameTextRecordsNothing(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	if err := RecordEdit(dataDir, r, "kanna", specFileName, "# Spec\n", fixedNow); err != nil {
		t.Fatalf("RecordEdit: %v", err)
	}
	revisions, err := ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Edits) != 0 || len(revisions) != 0 {
		t.Errorf("Edits = %+v revisions = %+v, want none", r.Edits, revisions)
	}
}

// TestEditRevisionIsNotMistakenForARetriedRejection: SnapshotRevision's
// retry check counts rejection snapshots only, so a rejection that follows
// an edit by the same operator still gets its own revision.
func TestEditRevisionIsNotMistakenForARetriedRejection(t *testing.T) {
	dataDir := t.TempDir()
	r := newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)
	if err := RecordEdit(dataDir, r, "kanna", specFileName, "# Spec\n\nmine\n", fixedNow); err != nil {
		t.Fatal(err)
	}

	n, err := SnapshotRevision(dataDir, "req-1", "kanna", "", StateSpecReview, []string{specFileName}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rejection revision = %d, want 2 (a new one after the edit's)", n)
	}
}

// TestStageFeedbackQuotesEditsMadeBeforeARejection: an edit reaches the
// drafter inside the section of the next rejection of its own stage, and
// nowhere else.
func TestStageFeedbackQuotesEditsMadeBeforeARejection(t *testing.T) {
	at := func(minutes int) string {
		return fixedNow.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
	}
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.Edits = []Edit{
		{By: "kanna\n# fake heading", At: at(1), Path: "spec.md", FromState: StateSpecReview, Diff: "- old\n+ new\n"},
		{By: "kanna", At: at(3), Path: "spec.md", FromState: StateSpecReview, Diff: "- second\n+ round\n"},
		{By: "kanna", At: at(4), Path: "tickets/001.spec.md", FromState: StatePlanReview, Diff: "- plan\n+ edit\n"},
		{By: "kanna", At: at(9), Path: "spec.md", FromState: StateSpecReview, Diff: "- after\n+ last rejection\n"},
	}
	r.Rejections = []Rejection{
		{By: "kanna", At: at(2), Reason: "first note", FromState: StateSpecReview},
		{By: "kanna", At: at(5), Reason: "second note", FromState: StateSpecReview},
	}

	want := "## Spec rejected " + at(2) + " by kanna\n\nfirst note\n\n" +
		"Before rejecting, kanna # fake heading edited spec.md by hand. Keep these changes in the redraft unless the note above says otherwise (\"-\" lines were removed, \"+\" lines were added):\n\n- old\n+ new\n\n" +
		"## Spec rejected " + at(5) + " by kanna\n\nsecond note\n\n" +
		"Before rejecting, kanna edited spec.md by hand. Keep these changes in the redraft unless the note above says otherwise (\"-\" lines were removed, \"+\" lines were added):\n\n- second\n+ round\n\n"
	if got := SpecFeedback(r); got != want {
		t.Errorf("SpecFeedback =\n%s\nwant\n%s", got, want)
	}
	if got := PlanFeedback(r); got != "" {
		t.Errorf("PlanFeedback = %q, want empty: the plan was never rejected", got)
	}
}

// TestEditDiffIsBoundedAndEveryLineIsPrefixed: a line of the edited text
// can never start a line of the feedback it is quoted in, and a long edit
// is cut with a note.
func TestEditDiffIsBoundedAndEveryLineIsPrefixed(t *testing.T) {
	diff, truncated := editDiff("a\n", "## Spec rejected now\n```\na\n")
	if truncated || diff != "+ ## Spec rejected now\n+ ```\n  a\n  \n" {
		t.Errorf("editDiff = %q truncated=%v", diff, truncated)
	}
	long := strings.Repeat("line of new text\n", 400)
	diff, truncated = editDiff("", long)
	if !truncated || len(diff) > maxEditDiffBytes+len("(more changes not listed)\n") || !strings.HasSuffix(diff, "(more changes not listed)\n") {
		t.Errorf("long editDiff: truncated=%v len=%d", truncated, len(diff))
	}
	for _, line := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		if !strings.HasPrefix(line, "+ ") && !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "  ") && line != "..." && line != "(more changes not listed)" {
			t.Fatalf("unprefixed diff line %q", line)
		}
	}
}
