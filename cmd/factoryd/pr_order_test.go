package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"buildgate/internal/run"
)

func TestRunPROrderIsRecordedOnlyForARequestTicket(t *testing.T) {
	groups := [][]string{{"db/migrations/"}}
	for _, tc := range []struct {
		name          string
		requestTicket bool
		groups        [][]string
		group         int
		want          *run.PROrder
	}{
		{"single-ticket run ignores pr_order", false, groups, 0, nil},
		{"no groups listed", true, nil, 0, nil},
		{"request ticket", true, groups, 0, &run.PROrder{Groups: groups}},
		{"carved ticket", true, groups, 2, &run.PROrder{Groups: groups, TicketGroup: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &ticketRun{requestTicket: &tc.requestTicket, prOrder: tc.groups, rf: runFlags{orderGroup: &tc.group}}
			if got := tr.runPROrder(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("runPROrder = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEveryBuildOfACarvedTicketIsCarved(t *testing.T) {
	zero, two := 0, 2
	for _, tc := range []struct {
		name   string
		replay *run.Replay
		group  *int
		want   bool
	}{
		{"ordinary ticket", nil, &zero, false},
		{"replay of a carved commit", &run.Replay{Carve: &run.Carve{Group: 1}}, &zero, true},
		{"later round of a carved ticket", nil, &two, true},
		{"restack replay of a carved ticket", &run.Replay{}, &two, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &ticketRun{replay: tc.replay, rf: runFlags{orderGroup: tc.group}}
			if got := tr.carved(); got != tc.want {
				t.Fatalf("carved = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every build of a carved ticket records the opt-out for tests_red_on_base,
// whatever the ticket's own spec declares; an ordinary ticket records only its
// own.
func TestCarvedTicketRecordsTheTestsRequiredOptOut(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	two, zero := 2, 0
	for name, tc := range map[string]struct {
		group *int
		want  string
	}{
		"carved":   {&two, carvedTestsRequiredReason},
		"ordinary": {&zero, ""},
	} {
		tr := &ticketRun{specSnapshotPath: spec, rf: runFlags{orderGroup: &two}}
		tr.rf.orderGroup = tc.group
		if err := tr.parseTestsRequiredOpt(); err != nil {
			t.Fatalf("%s: parseTestsRequiredOpt: %v", name, err)
		}
		if tr.testsRequiredOptOut != tc.want {
			t.Errorf("%s: opt-out = %q, want %q", name, tr.testsRequiredOptOut, tc.want)
		}
	}
}
