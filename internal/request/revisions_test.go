package request

import "testing"

// TestListRevisionsEmptyWhenNeverRejected covers the "never rejected"
// case: an empty slice, not an error.
func TestListRevisionsEmptyWhenNeverRejected(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	revisions, err := ListRevisions(dataDir, "req-1")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revisions) != 0 {
		t.Errorf("ListRevisions = %+v, want empty", revisions)
	}
}

// TestLoadRevisionMissingIsError covers a revision index that was never
// recorded.
func TestLoadRevisionMissingIsError(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	if _, _, err := LoadRevision(dataDir, "req-1", 1); err == nil {
		t.Fatal("LoadRevision for a never-recorded revision: want an error, got nil")
	}
}

// TestSnapshotRevisionSkipsMissingFiles covers a relPath that does not
// exist yet: skipped, not an error, and not recorded in Files.
func TestSnapshotRevisionSkipsMissingFiles(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, false)

	n, err := SnapshotRevision(dataDir, "req-1", "bob", "reason", StateSpecReview, []string{specFileName, "tickets/999.spec.md"}, fixedNow)
	if err != nil {
		t.Fatalf("SnapshotRevision: %v", err)
	}
	rev, contents, err := LoadRevision(dataDir, "req-1", n)
	if err != nil {
		t.Fatalf("LoadRevision: %v", err)
	}
	if len(rev.Files) != 1 || rev.Files[0] != specFileName {
		t.Errorf("Files = %v, want only %q", rev.Files, specFileName)
	}
	if len(contents) != 1 {
		t.Errorf("contents = %+v, want one entry", contents)
	}
	if !rev.FeedbackSupplied {
		t.Errorf("FeedbackSupplied = false, want true for a spec_review rejection with feedback")
	}
}
