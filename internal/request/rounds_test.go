package request

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func roundsRequest() *Request {
	r := New("req-9", "/repos/app", "app", Source{Kind: SourceText}, time.Now())
	r.Tickets = []Ticket{{Index: 1}, {Index: 2}}
	r.TicketCount = 2
	r.TicketIndex = 1
	return r
}

func TestRoundTagRecordedOnlyForACommit(t *testing.T) {
	r := roundsRequest()
	cases := []struct {
		name string
		tag  RoundTag
		want bool
	}{
		{"no result commit", RoundTag{RunID: "a", SHA: "", BaseSHA: "b1"}, false},
		{"result equals its base", RoundTag{RunID: "a", SHA: "b1", BaseSHA: "b1"}, false},
		{"a commit of its own", RoundTag{RunID: "a", SHA: "c1", BaseSHA: "b1"}, true},
	}
	for _, c := range cases {
		got, ok := r.RoundTagFor(1, c.tag)
		if ok != c.want {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.want)
		}
		if ok && (got.N != 1 || got.Ref != "refs/buildgate/rounds/req-9/t1/r1") {
			t.Errorf("%s: tag = %+v, want round 1 under refs/buildgate/rounds/req-9/t1/", c.name, got)
		}
	}
	tag, _ := r.RoundTagFor(1, RoundTag{RunID: "a", SHA: "c1", BaseSHA: "b1"})
	r.AppendRoundTag(1, tag)
	if _, ok := r.RoundTagFor(1, RoundTag{RunID: "a", SHA: "c1", BaseSHA: "b1"}); ok {
		t.Error("the same run is recorded twice, want it once")
	}
	second, ok := r.RoundTagFor(1, RoundTag{RunID: "b", SHA: "c2", BaseSHA: "c1"})
	if !ok || second.N != 2 {
		t.Errorf("next round = %+v ok %v, want round 2", second, ok)
	}
	other, ok := r.RoundTagFor(2, RoundTag{RunID: "c", SHA: "d1", BaseSHA: "b1"})
	if !ok || other.N != 1 || other.Ref != "refs/buildgate/rounds/req-9/t2/r1" {
		t.Errorf("ticket 2's first round = %+v ok %v, want r1 under t2: rounds are numbered per ticket", other, ok)
	}
	if _, ok := r.RoundTagFor(5, RoundTag{RunID: "d", SHA: "e", BaseSHA: "f"}); ok {
		t.Error("a round of a ticket the request does not have was recorded")
	}
}

func TestRetryFromRoundSetsTheChoiceAndRefusesWhatCannotBeRebuilt(t *testing.T) {
	dataDir := t.TempDir()
	r := roundsRequest()
	r.State = StateQuarantined
	r.Tickets[0].RoundTags = []RoundTag{{N: 1, RunID: "a", SHA: "c1"}, {N: 2, RunID: "b", SHA: "c2"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := RetryFromRound(dataDir, r.ID, "alice", "", 3, time.Now(), nil); !errors.Is(err, ErrIllegalTransition) || !strings.Contains(err.Error(), "rounds: 1, 2") {
		t.Fatalf("round 3 = %v, want a refusal naming rounds 1, 2", err)
	}
	if _, err := RetryFromRound(dataDir, r.ID, "alice", "", 0, time.Now(), nil); err == nil {
		t.Fatal("round 0 was accepted")
	}
	got, err := RetryFromRound(dataDir, r.ID, "alice", "", 2, time.Now(), nil)
	if err != nil {
		t.Fatalf("RetryFromRound(2): %v", err)
	}
	if got.State != StateBuilding || got.RetryFromRound != 2 || got.RetryFromScratch {
		t.Errorf("after = state %q round %d scratch %v, want building from round 2", got.State, got.RetryFromRound, got.RetryFromScratch)
	}
	if err := got.Halt("lost", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got.RetryFromRound != 0 {
		t.Errorf("RetryFromRound = %d after the next transition, want it dropped", got.RetryFromRound)
	}
}

func TestRetryFromRoundRefusesARequestThatRebuildsNothing(t *testing.T) {
	dataDir := t.TempDir()
	r := roundsRequest()
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.Tickets[0].RoundTags = []RoundTag{{N: 1, RunID: "a", SHA: "c1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := RetryFromRound(dataDir, r.ID, "alice", "", 1, time.Now(), nil); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("RetryFromRound on an accepted-no-PR halt = %v, want a refusal", err)
	}
}

func TestMergeReadinessOnlyDraftBlocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    *MergeReadiness
		want bool
	}{
		{"nil", nil, false},
		{"ready", &MergeReadiness{Ready: true}, false},
		{"only the draft", &MergeReadiness{Draft: true, Blockers: []string{"it is still a draft"}}, true},
		{"draft and failing checks", &MergeReadiness{Draft: true, Blockers: []string{"it is still a draft", "its checks are pending or failing"}}, false},
		{"another blocker, not a draft", &MergeReadiness{Blockers: []string{"it is stacked"}}, false},
	} {
		if got := tc.m.OnlyDraftBlocks(); got != tc.want {
			t.Errorf("%s: OnlyDraftBlocks = %v, want %v", tc.name, got, tc.want)
		}
	}
}
