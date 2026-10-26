package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// writeReplayFile writes a valid replay record onto onto, ending at head, and
// returns its path.
func writeReplayFile(t *testing.T, dir, onto, head string) string {
	t.Helper()
	original := strings.Repeat("3", 40)
	record := run.Replay{
		RequestID: "req-1", TicketIndex: 2,
		OfRunID: "req-1-002", SourceRunIDs: []string{"req-1-002"},
		UpstreamSHA: strings.Repeat("1", 40), SourceHeadSHA: original, OntoSHA: onto, HeadSHA: head,
		Commits: []run.ReplayedCommit{{Original: original, Replayed: head, OriginalPatchID: strings.Repeat("d", 40), ReplayedPatchID: strings.Repeat("d", 40)}},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "replay.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// -replay reaches RunWorkflowInput, carrying the part of the record the
// workflow checks; an ordinary run carries none.
func TestReplayReachesTheWorkflow(t *testing.T) {
	replay := &run.Replay{OfRunID: "a", SourceRunIDs: []string{"a", "b"}, OntoSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40)}
	got := (runOptions{Slice: temporalSliceOptions{Replay: replayInputOf(replay)}}).workflowInput().Replay
	want := &workflow.ReplayInput{OfRunID: "a", SourceRunIDs: []string{"a", "b"}, OntoSHA: replay.OntoSHA, HeadSHA: replay.HeadSHA}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunWorkflowInput.Replay = %+v, want %+v", got, want)
	}
	if (runOptions{}).workflowInput().Replay != nil {
		t.Fatal("an ordinary run carried a replay")
	}
}

// -replay is refused beside every input that starts, chains or opens a build,
// without -on-branch or -diff-base, with a -diff-base that is not the record's
// onto_sha, and with a file that is not a valid record.
func TestReplayFlagRefusals(t *testing.T) {
	dir := t.TempDir()
	onto, head := strings.Repeat("a", 40), strings.Repeat("c", 40)
	path := writeReplayFile(t, dir, onto, head)
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"of_run_id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := []string{"-replay", path, "-on-branch", "buildgate/req-1/t2-restack1", "-diff-base", onto}
	check := func(extra ...string) error {
		t.Helper()
		flags, rf := newRunFlags()
		if err := flags.Parse(append(append([]string{}, ok...), extra...)); err != nil {
			t.Fatal(err)
		}
		_, err := validateReplayFlags(*rf.replay, *rf.onBranch, *rf.diffBase, replayConflicts(rf))
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("a well-formed replay run refused: %v", err)
	}
	for _, extra := range [][]string{
		{"-earlier-attempt", "x.md"},
		{"-resume-worktree-of", "lost"},
		{"-prior-run", "req-1-001"},
		{"-baseline-check"},
		{"-open-pull-request"},
		{"-pr-base", "main"},
		{"-repository", "acme"},
		{"-run-id", "served-1"},
		{"-on-branch", ""},
		{"-diff-base", ""},
		{"-diff-base", head},
		{"-replay", bad},
	} {
		if err := check(extra...); err == nil {
			t.Errorf("-replay with %v accepted", extra)
		}
	}
	if replay, err := validateReplayFlags("", "", "", nil); replay != nil || err != nil {
		t.Errorf("no -replay: %+v, %v; want nothing", replay, err)
	}
}
