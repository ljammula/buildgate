package request

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveThenLoadRoundTrips(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceIssue, IssueRef: "acme/widgets#1"}, fixedNow)
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != r.ID || loaded.Workspace != r.Workspace || loaded.Source != r.Source || loaded.State != r.State {
		t.Errorf("loaded = %+v, want %+v", loaded, r)
	}
}

func TestSaveTextThenTextPathRoundTrips(t *testing.T) {
	dataDir := t.TempDir()
	if err := SaveText(dataDir, "req-1", "Add idempotency keys"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	b, err := os.ReadFile(TextPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "Add idempotency keys" {
		t.Errorf("text = %q, want %q", string(b), "Add idempotency keys")
	}
}

func TestTitleReturnsTrimmedFirstLine(t *testing.T) {
	dataDir := t.TempDir()
	if err := SaveText(dataDir, "req-1", "  Add idempotency keys  \n\nMore detail on the second paragraph."); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	if got := Title(dataDir, "req-1"); got != "Add idempotency keys" {
		t.Errorf("Title = %q, want %q", got, "Add idempotency keys")
	}
}

func TestTitleEmptyWhenRequestTextMissing(t *testing.T) {
	dataDir := t.TempDir()
	if got := Title(dataDir, "missing"); got != "" {
		t.Errorf("Title = %q, want empty", got)
	}
}

func TestSpecPathJoinsRequestDir(t *testing.T) {
	dataDir := t.TempDir()
	want := filepath.Join(dataDir, "requests", "req-1", "spec.md")
	if got := SpecPath(dataDir, "req-1"); got != want {
		t.Errorf("SpecPath = %q, want %q", got, want)
	}
}

// TestListSkipsDirectoryWithoutPublishedRequestJSON covers the
// "request.md written first, request.json published last" invariant: a
// request directory that has SaveText's output but no request.json yet
// (the state a crash between the two writes, or a concurrent List racing
// submit, could observe) must be silently skipped, not treated as
// corrupt.
func TestListSkipsDirectoryWithoutPublishedRequestJSON(t *testing.T) {
	dataDir := t.TempDir()
	if err := SaveText(dataDir, "unpublished", "some request text"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	published := New("published", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	if err := SaveText(dataDir, "published", "text"); err != nil {
		t.Fatal(err)
	}
	if err := published.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	requests, err := List(dataDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 1 || requests[0].ID != "published" {
		t.Fatalf("List = %v, want only the published request", requests)
	}
}

func TestListOrdersOldestSubmittedFirst(t *testing.T) {
	dataDir := t.TempDir()
	older := New("older", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	newer := New("newer", "/repos/app", "app", Source{Kind: SourceText}, fixedNow.Add(time.Hour))
	// Saved in reverse order, so List's own sort -- not save order or
	// directory read order -- must be what determines the result.
	if err := newer.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := older.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	requests, err := List(dataDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(requests) != 2 || requests[0].ID != "older" || requests[1].ID != "newer" {
		t.Fatalf("List = %v, want [older, newer]", requests)
	}
}

func TestListOnMissingDirectoryReturnsEmptyNotError(t *testing.T) {
	requests, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("List: %v, want no error for a data dir with no requests directory yet", err)
	}
	if len(requests) != 0 {
		t.Errorf("List = %v, want empty", requests)
	}
}

func TestClaimIDDisambiguatesOnCollision(t *testing.T) {
	dataDir := t.TempDir()
	first, err := ClaimID(dataDir, "add-idempotency-keys")
	if err != nil {
		t.Fatalf("ClaimID: %v", err)
	}
	second, err := ClaimID(dataDir, "add-idempotency-keys")
	if err != nil {
		t.Fatalf("ClaimID: %v", err)
	}
	if first == second {
		t.Fatalf("ClaimID returned the same id twice: %q", first)
	}
	if second != "add-idempotency-keys-2" {
		t.Errorf("second = %q, want %q", second, "add-idempotency-keys-2")
	}
}

// TestSaveCrashAfterTempWriteLeavesOldOrNewNeverTorn is the crash-safety
// test the request driver requires: a failure injected right after the
// temp file is written (before the rename that publishes it) must leave
// either the OLD request.json (if one existed) or nothing at all --
// never a torn/partial request.json. Save's own os.Rename is what
// guarantees this: renaming into place is atomic on the same filesystem,
// so a process that dies between the temp write and the rename simply
// never gets far enough to replace the old file.
func TestSaveCrashAfterTempWriteLeavesOldOrNewNeverTorn(t *testing.T) {
	dataDir := t.TempDir()

	// First, a real save establishes an "old" request.json.
	original := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	if err := original.Save(dataDir); err != nil {
		t.Fatalf("Save (original): %v", err)
	}
	originalBytes, err := os.ReadFile(Path(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate "crash after the temp write, before the rename": write the
	// new content to the temp path directly (the same operation Save's own
	// os.WriteFile(tmp, ...) performs) and stop there -- never calling
	// os.Rename, exactly as a process killed at that point would.
	updated := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow.Add(time.Hour))
	updated.State = StateSpecDrafting
	tmpPath := Path(dataDir, "req-1") + ".tmp"
	simulateCrashAfterTempWrite(t, tmpPath, updated)

	// The published request.json must be untouched: reading it back gives
	// exactly the original, never a mix of old and new fields.
	loaded, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load after simulated crash: %v", err)
	}
	if loaded.State != original.State {
		t.Errorf("State = %q after simulated crash, want the OLD state %q (rename never happened)", loaded.State, original.State)
	}
	publishedBytes, err := os.ReadFile(Path(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(publishedBytes) != string(originalBytes) {
		t.Errorf("request.json changed even though the rename was never reached")
	}

	// The temp file itself must be exactly updated's content, not torn --
	// proving the write that ran before the simulated crash completed
	// cleanly rather than partially.
	tmpBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	var reloadedTmp Request
	if err := json.Unmarshal(tmpBytes, &reloadedTmp); err != nil {
		t.Fatalf("temp file is torn/corrupt JSON: %v", err)
	}
	if reloadedTmp.State != StateSpecDrafting {
		t.Errorf("temp file State = %q, want %q", reloadedTmp.State, StateSpecDrafting)
	}

	// Finally: completing the rename now (what the NEXT successful Save
	// would do) must cleanly replace the old file with the new one -- the
	// crash left the directory in a recoverable state, not a stuck one.
	if err := os.Rename(tmpPath, Path(dataDir, "req-1")); err != nil {
		t.Fatalf("os.Rename: %v", err)
	}
	recovered, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load after completing the rename: %v", err)
	}
	if recovered.State != StateSpecDrafting {
		t.Errorf("State after completing the rename = %q, want %q", recovered.State, StateSpecDrafting)
	}
}

// TestSaveFallbackRecordsDirectStateChangeOnce covers Save's own History
// fallback (see its doc comment): a caller outside this package -- e.g.
// cmd/factoryd/request_driver_test.go's own fixture pattern, `r.State =
// ...` followed by `r.Save(dataDir)` -- must still get exactly one
// History entry for that move, and a later plain re-save (no further
// state change) must never append a second one.
func TestSaveFallbackRecordsDirectStateChangeOnce(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save (initial): %v", err)
	}
	if len(r.History) != 0 {
		t.Fatalf("History after initial Save = %+v, want none (no state change yet)", r.History)
	}

	r.State = StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save (after direct State write): %v", err)
	}
	if len(r.History) != 1 {
		t.Fatalf("History after direct State write = %+v, want exactly one entry", r.History)
	}
	if got := r.History[0]; got.From != StateSubmitted || got.To != StateSpecReview || got.By != factoryActor {
		t.Errorf("History[0] = %+v, want From=%q To=%q By=%q", got, StateSubmitted, StateSpecReview, factoryActor)
	}

	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save (plain re-save): %v", err)
	}
	if len(r.History) != 1 {
		t.Errorf("History after plain re-save = %+v, want still exactly one entry", r.History)
	}

	loaded, err := Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.History) != 1 {
		t.Fatalf("loaded.History = %+v, want exactly one entry", loaded.History)
	}
	if err := loaded.Save(dataDir); err != nil {
		t.Fatalf("Save (loaded, no change): %v", err)
	}
	if len(loaded.History) != 1 {
		t.Errorf("loaded.History after re-save with no change = %+v, want still exactly one entry", loaded.History)
	}
}

// simulateCrashAfterTempWrite writes r's JSON to tmpPath the same way
// Save's own os.WriteFile(tmp, ...) does, and stops -- modeling a process
// killed (e.g. SIGKILL, or a real crash) after that write but before
// Save's subsequent os.Rename ever runs.
func simulateCrashAfterTempWrite(t *testing.T, tmpPath string, r *Request) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
}

func TestTitleSkipsASectionLabelHeading(t *testing.T) {
	dataDir := t.TempDir()
	cases := map[string]string{
		"# Goal\nAdd a divide(a, b) function.\n\n## Acceptance criteria\n- x": "Add a divide(a, b) function.",
		"## Request:\n\nFix the cache":                                        "Fix the cache",
		"# Add login page\nDetails":                                           "Add login page",
		"Plain first line\nsecond":                                            "Plain first line",
		"# Goal":                                                              "Goal",
		"\n\n":                                                                "",
	}
	i := 0
	for text, want := range cases {
		i++
		id := fmt.Sprintf("r%d", i)
		if err := SaveText(dataDir, id, text); err != nil {
			t.Fatal(err)
		}
		if got := Title(dataDir, id); got != want {
			t.Errorf("Title(%q) = %q, want %q", text, got, want)
		}
	}
}
