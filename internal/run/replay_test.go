package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func validReplay() Replay {
	return Replay{
		RequestID:     "req-1",
		TicketIndex:   2,
		OfRunID:       "req-1-002-review1",
		SourceRunIDs:  []string{"req-1-002", "req-1-002-review1"},
		UpstreamSHA:   sha('1'),
		SourceHeadSHA: sha('3'),
		OntoSHA:       sha('a'),
		HeadSHA:       sha('c'),
		Commits: []ReplayedCommit{
			{Original: sha('2'), Replayed: sha('b'), OriginalPatchID: sha('d'), ReplayedPatchID: sha('d')},
			{Original: sha('3'), Replayed: sha('c'), OriginalPatchID: sha('e'), ReplayedPatchID: sha('e')},
		},
	}
}

func TestReplayValidate(t *testing.T) {
	cases := map[string]func(*Replay){
		"no source runs":         func(r *Replay) { r.SourceRunIDs = nil },
		"of run not a source":    func(r *Replay) { r.SourceRunIDs = []string{"req-1-002"} },
		"short onto":             func(r *Replay) { r.OntoSHA = "abc123" },
		"no commits":             func(r *Replay) { r.Commits = nil },
		"last replayed not head": func(r *Replay) { r.HeadSHA = sha('f') },
		"last original not head": func(r *Replay) { r.SourceHeadSHA = sha('f') },
		"one patch id missing":   func(r *Replay) { r.Commits[0].ReplayedPatchID = "" },
		"uppercase commit":       func(r *Replay) { r.Commits[0].Original = strings.ToUpper(sha('a')) },
		"empty of_run_id":        func(r *Replay) { r.OfRunID = "" },
		"empty source run id":    func(r *Replay) { r.SourceRunIDs = append(r.SourceRunIDs, "") },
		"no request":             func(r *Replay) { r.RequestID = "" },
		"no ticket":              func(r *Replay) { r.TicketIndex = 0 },
	}
	ok := validReplay()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid replay refused: %v", err)
	}
	empty := validReplay()
	empty.Commits[0].OriginalPatchID, empty.Commits[0].ReplayedPatchID = "", ""
	if err := empty.Validate(); err != nil {
		t.Errorf("an empty commit's pair (no patch ids) refused: %v", err)
	}
	for name, mutate := range cases {
		r := validReplay()
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

func TestReadReplayFileRefusesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	body := `{"request_id":"req-1","ticket_index":2,"of_run_id":"r","source_run_ids":["r"],"upstream_sha":"` + sha('1') + `","source_head_sha":"` + sha('2') + `","onto_sha":"` + sha('a') + `","head_sha":"` + sha('b') + `","commits":[{"original":"` + sha('2') + `","replayed":"` + sha('b') + `","original_patch_id":"` + sha('d') + `","replayed_patch_id":"` + sha('d') + `"}]}`
	if err := os.WriteFile(good, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadReplayFile(good); err != nil || r.HeadSHA != sha('b') {
		t.Fatalf("ReadReplayFile = %+v, %v", r, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(strings.Replace(body, `"head_sha"`, `"heads_sha"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReplayFile(bad); err == nil {
		t.Fatal("ReadReplayFile accepted a misspelled field")
	}
}

// TestApplyOverrideRefusesAReplayRun: a replayed result is never accepted by
// hand, whatever the release policy says about overrides.
func TestApplyOverrideRefusesAReplayRun(t *testing.T) {
	replay := validReplay()
	r := &Run{ID: "req-1-002-restack1", State: StateQuarantined, Replay: &replay}
	err := r.ApplyOverride("alice", "looks fine", StateAccepted, func() string { return "2026-10-25T00:00:00Z" })
	if err == nil || r.State != StateQuarantined || len(r.Overrides) != 0 {
		t.Fatalf("override of a replay run: err=%v state=%s overrides=%d, want refused and unchanged", err, r.State, len(r.Overrides))
	}
}
