package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func TestMatchIDNamesOneRecordOrNone(t *testing.T) {
	requests := []string{"add-login-20261101-081026", "add-login-page-20261101-091500", "fix-cache-20261102-101010"}
	runs := []string{
		"add-login-20261101-081026-001-20261101-081240-83085",
		"add-login-20261101-081026-001-review1-20261101-090000-83085",
		"fix-cache-20261102-101010-001-20261102-101500-83085",
		"smoke-ticket-20261103-111111-4242",
	}
	for _, tc := range []struct {
		name, query string
		requests    []string
		runs        []string
		want        string
		candidates  []string
	}{
		{name: "the full id of a request that is a prefix of another's", query: "add-login-20261101-081026", requests: requests, runs: runs, want: "add-login-20261101-081026"},
		{name: "a request's trailing timestamp, which its runs carry too", query: "081026", requests: requests, runs: runs, want: "add-login-20261101-081026"},
		{name: "a unique prefix", query: "fix-", requests: requests, runs: runs, want: "fix-cache-20261102-101010"},
		{name: "a run's launch time", query: "20261101-090000", requests: requests, runs: runs, want: "add-login-20261101-081026-001-review1-20261101-090000-83085"},
		{name: "a run of no request", query: "smoke", requests: requests, runs: runs, want: "smoke-ticket-20261103-111111-4242"},
		{name: "a prefix two requests share", query: "add-login", requests: requests, runs: runs, candidates: []string{
			"add-login-20261101-081026", "add-login-page-20261101-091500",
			"add-login-20261101-081026-001-20261101-081240-83085", "add-login-20261101-081026-001-review1-20261101-090000-83085",
		}},
		{name: "a part only two runs share", query: "081026-001", requests: requests, runs: runs, candidates: []string{
			"add-login-20261101-081026-001-20261101-081240-83085", "add-login-20261101-081026-001-review1-20261101-090000-83085",
		}},
		{name: "a process id runs of two requests share", query: "83085", requests: requests, runs: runs, candidates: []string{
			"add-login-20261101-081026-001-20261101-081240-83085", "add-login-20261101-081026-001-review1-20261101-090000-83085",
			"fix-cache-20261102-101010-001-20261102-101500-83085",
		}},
		{name: "nothing", query: "nope", requests: requests, runs: runs},
		{name: "requests only: a run's part names no request", query: "081240", requests: requests},
		{name: "runs only: a request's id that one run carries", query: "fix-cache-20261102-101010", runs: runs, want: "fix-cache-20261102-101010-001-20261102-101500-83085"},
		{name: "runs only: a request's id that two runs carry", query: "add-login-20261101-081026", runs: runs, candidates: []string{
			"add-login-20261101-081026-001-20261101-081240-83085", "add-login-20261101-081026-001-review1-20261101-090000-83085",
		}},
	} {
		got, candidates := matchID(tc.query, tc.requests, tc.runs, strings.Contains)
		if got != tc.want || !reflect.DeepEqual(candidates, tc.candidates) {
			t.Errorf("%s: matchID(%q) = %q, %v; want %q, %v", tc.name, tc.query, got, candidates, tc.want, tc.candidates)
		}
		if got != "" && len(candidates) > 0 {
			t.Errorf("%s: matchID(%q) returned an id and candidates", tc.name, tc.query)
		}
	}
}

// A gate decision lands only on the request or run the operator named. A
// part of an id that two records share is refused with both listed, and
// neither record changes: approve, reject, answer and override never pick.
func TestAGateDecisionOnAnAmbiguousIDIsRefusedAndChangesNothing(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	first, second := "add-login-20261101-081026", "add-login-page-20261101-091500"
	for _, id := range []string{first, second} {
		requestdrivertest.NewApprovableDriverRequest(t, dataDir, id, request.StateSpecReview)
	}
	runA, runB := "smoke-20261103-111111-4242", "smoke-20261103-121212-4242"
	for _, id := range []string{runA, runB} {
		if err := (&run.Run{ID: id, Ticket: "smoke", State: run.StateQuarantined}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	updatedBefore := map[string]string{}
	for _, id := range []string{first, second} {
		loaded, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		updatedBefore[id] = loaded.UpdatedAt
	}

	decisions := map[string]func() error{
		"approve": func() error { return approveMain(dp, []string{"-data-dir", dataDir, "add-login"}) },
		"reject":  func() error { return rejectMain(dp, []string{"-data-dir", dataDir, "-reason", "no", "add-login"}) },
		"answer": func() error {
			return answerMain(dp, []string{"-data-dir", dataDir, "-q", "Q1", "-hash", "0123456789ab", "-text", "yes", "add-login"})
		},
		"override": func() error {
			return overrideMain(dp, []string{"-data-dir", dataDir, "-run", "smoke-2026", "-by", "operator", "-reason", "checked", "-state", "accepted"})
		},
	}
	for verb, decide := range decisions {
		err := decide()
		if err == nil {
			t.Fatalf("%s on an ambiguous id succeeded", verb)
		}
		want := []string{first, second}
		if verb == "override" {
			want = []string{runA, runB}
		}
		for _, id := range want {
			if !strings.Contains(err.Error(), "\n  "+id) {
				t.Errorf("%s: err = %v, want candidate %s listed", verb, err, id)
			}
		}
		if !strings.Contains(err.Error(), "matches 2 ids") || !strings.Contains(err.Error(), "nothing was done") {
			t.Errorf("%s: err = %v, want a refusal that says nothing was done", verb, err)
		}
	}
	for _, id := range []string{first, second} {
		loaded, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State != request.StateSpecReview || loaded.UpdatedAt != updatedBefore[id] {
			t.Errorf("request %s is %s, updated %s, after refused decisions; want spec_review, not written since %s", id, loaded.State, loaded.UpdatedAt, updatedBefore[id])
		}
	}
	for _, id := range []string{runA, runB} {
		loaded, err := run.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State != run.StateQuarantined || len(loaded.Overrides) != 0 {
			t.Errorf("run %s is %s with %d overrides after a refused override", id, loaded.State, len(loaded.Overrides))
		}
	}

	// The part only one of them has names it.
	if err := approveMain(dp, []string{"-data-dir", dataDir, "091500"}); err != nil {
		t.Fatalf("approve by a unique part: %v", err)
	}
	for id, want := range map[string]request.State{first: request.StateSpecReview, second: request.StatePlanning} {
		loaded, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.State != want {
			t.Errorf("request %s is %s, want %s", id, loaded.State, want)
		}
	}
}

// An id nothing matches is refused by name, with the data dir, the profile
// and the nearest ids: never a file-open error.
func TestAnUnknownIDNamesThePlaceAndTheNearestIDs(t *testing.T) {
	f := newProfileFixture(t, "default")
	root := f.roots["default"]
	for _, id := range []string{"add-login-20261101-081026", "fix-cache-20261102-101010", "bump-deps-20261103-111111", "tidy-docs-20261104-121212"} {
		saveWaitingRequest(t, root, id, "A title", request.StateSpecReview, 0, nil)
	}
	var out bytes.Buffer
	err := inspectRun([]string{"add-logn-20261101"}, &out)
	if err == nil {
		t.Fatal("inspect of an unknown id succeeded")
	}
	msg := err.Error()
	for _, want := range []string{`no request or run matches "add-logn-20261101" in ` + root + ` (profile default). Nearest:`, "\n  add-login-20261101-081026"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "no such file") || strings.Contains(msg, "request.json") {
		t.Errorf("err = %q, want no file-open error", msg)
	}
	if n := strings.Count(msg, "\n  "); n > nearestIDsShown {
		t.Errorf("err lists %d ids, want at most %d:\n%s", n, nearestIDsShown, msg)
	}

	// -data-dir names the place itself: no profile is claimed for it.
	err = watchMain(newTestDeps(t), []string{"-data-dir", root, "-no-follow", "zzzz"})
	if err == nil || !strings.Contains(err.Error(), `no request or run matches "zzzz" in `+root+". ") || strings.Contains(err.Error(), "profile") && !strings.Contains(err.Error(), "other profiles") {
		t.Errorf("watch of an unknown id: err = %v", err)
	}
	for _, id := range []string{"../x", "a/b", ".."} {
		if err := inspectRun([]string{"-data-dir", root, id}, &out); err == nil || !strings.Contains(err.Error(), "is not a request or run id") {
			t.Errorf("inspect %q: err = %v, want it refused as no id", id, err)
		}
	}
}

// Every command that takes an id takes a unique part of it.
func TestCommandsTakeAUniquePartOfAnID(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	id := "add-login-20261101-081026"
	saveWaitingRequest(t, dataDir, id, "Add login", request.StateSpecReview, 0, nil)
	saveWaitingRequest(t, dataDir, "fix-cache-20261102-101010", "Fix cache", request.StateSpecReview, 0, nil)

	var out bytes.Buffer
	if err := inspectRun([]string{"-data-dir", dataDir, "081026"}, &out); err != nil || !strings.Contains(out.String(), "/requests/"+id+"/spec.md") {
		t.Errorf("inspect by trailing timestamp: err = %v, output %q", err, out.String())
	}
	out.Reset()
	if err := roundsRun([]string{"-data-dir", dataDir, "add-"}, &out); err != nil || !strings.Contains(out.String(), "request "+id+" has no recorded rounds") {
		t.Errorf("rounds by prefix: err = %v, output %q", err, out.String())
	}
	watched := captureStdout(t, func() {
		if err := watchMain(dp, []string{"-data-dir", dataDir, "-no-follow", "login"}); err != nil {
			t.Errorf("watch by a part: %v", err)
		}
	})
	if !strings.Contains(watched, "request "+id) {
		t.Errorf("watch by a part printed %q, want request %s", watched, id)
	}
	if err := cancelMain(dp, []string{"-data-dir", dataDir, "20261101-081026"}); err != nil {
		t.Fatalf("cancel by a part: %v", err)
	}
	if loaded, err := request.Load(dataDir, id); err != nil || loaded.State != request.StateCancelled {
		t.Errorf("after cancel by a part: state %v, err %v, want cancelled", loaded, err)
	}
	if other, err := request.Load(dataDir, "fix-cache-20261102-101010"); err != nil || other.State != request.StateSpecReview {
		t.Errorf("the other request changed: %v, err %v", other, err)
	}
}

func TestNearestIDsRanksBySharedRunAndCaps(t *testing.T) {
	known := []string{"add-login-20261101-081026", "add-logout-20261101-091500", "fix-cache-20261102-101010", "zz"}
	if got, want := nearestIDs("add-login-2027", known, 2), []string{"add-login-20261101-081026", "add-logout-20261101-091500"}; !reflect.DeepEqual(got, want) {
		t.Errorf("nearestIDs = %v, want %v", got, want)
	}
	if got := nearestIDs("qq", known, 3); len(got) != 0 {
		t.Errorf("nearestIDs of a query sharing nothing = %v, want none", got)
	}
}

// A decision takes the start or the end of an id, six characters or more:
// a part from the middle, or a part as short as "2", names nothing however
// few records there are, and nothing changes.
func TestADecisionTakesOnlyTheStartOrEndOfAnIDAndNoShortPart(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	id := "add-login-20261126-001500"
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, id, request.StateSpecReview)
	runID := "smoke-20261103-111111-4242"
	if err := (&run.Run{ID: runID, Ticket: "smoke", State: run.StateQuarantined}).Save(dataDir); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ part, want string }{
		{"2", "too short"},
		{"login", "too short"},
		{"001500"[:5], "too short"},
		{"login-2026", "not a part from the middle"},
		{"20261126", "not a part from the middle"},
	} {
		refusals := map[string]error{
			"approve":     approveMain(dp, []string{"-data-dir", dataDir, tc.part}),
			"reject":      rejectMain(dp, []string{"-data-dir", dataDir, "-reason", "no", tc.part}),
			"answer":      answerMain(dp, []string{"-data-dir", dataDir, "-q", "Q1", "-hash", "0123456789ab", "-text", "yes", tc.part}),
			"cancel":      cancelMain(dp, []string{"-data-dir", dataDir, tc.part}),
			"retry":       retryMain(dp, []string{"-data-dir", dataDir, tc.part}),
			"amend-scope": amendScopeMain(dp, []string{"-data-dir", dataDir, "-reason", "r", tc.part, "a.go"}),
		}
		for verb, err := range refusals {
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s %q: err = %v, want it refused: %s", verb, tc.part, err, tc.want)
			}
		}
	}
	for _, part := range []string{"2", "20261103"} {
		err := overrideMain(dp, []string{"-data-dir", dataDir, "-run", part, "-by", "operator", "-reason", "checked", "-state", "accepted"})
		if err == nil {
			t.Errorf("override -run %q succeeded", part)
		}
	}
	if loaded, err := request.Load(dataDir, id); err != nil || loaded.State != request.StateSpecReview {
		t.Fatalf("after refused decisions the request is %v (err %v), want spec_review", loaded, err)
	}
	if loaded, err := run.Load(dataDir, runID); err != nil || loaded.State != run.StateQuarantined {
		t.Fatalf("after refused overrides the run is %v (err %v), want quarantined", loaded, err)
	}

	// The same parts name the request for a command that only reads.
	var out bytes.Buffer
	if err := roundsRun([]string{"-data-dir", dataDir, "2"}, &out); err != nil || !strings.Contains(out.String(), "request "+id) {
		t.Errorf("rounds by a short part: err = %v, output %q", err, out.String())
	}
	// Its start or its end, six characters or more, names it for a decision.
	if err := approveMain(dp, []string{"-data-dir", dataDir, "001500"}); err != nil {
		t.Fatalf("approve by the id's end: %v", err)
	}
	if loaded, err := request.Load(dataDir, id); err != nil || loaded.State != request.StatePlanning {
		t.Errorf("after approve by the id's end the request is %v (err %v), want planning", loaded, err)
	}
}

// A whole id whose record is gone names nothing: a decision never moves to
// the request made in the same second ("<id>-2"), and a directory of that
// name with no record is the one named, so the load says it is missing.
func TestAWholeIDOfAMissingRecordNeverResolvesToItsSibling(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	gone, sibling := "foo-20261126-001500", "foo-20261126-001500-2"
	requestdrivertest.NewApprovableDriverRequest(t, dataDir, sibling, request.StateSpecReview)

	// No directory of the typed id at all.
	err := approveMain(dp, []string{"-data-dir", dataDir, gone})
	if err == nil || !strings.Contains(err.Error(), `no request matches "`+gone+`"`) || !strings.Contains(err.Error(), "whole id") {
		t.Errorf("approve of a whole id with no record: err = %v, want it refused as a whole id that names nothing", err)
	}
	if err := cancelMain(dp, []string{"-data-dir", dataDir, gone}); err == nil {
		t.Error("cancel of a whole id with no record succeeded")
	}

	// A directory of that id with no request.json, as a claimed id leaves.
	if _, err := request.ClaimID(dataDir, gone); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for verb, err := range map[string]error{
		"approve": approveMain(dp, []string{"-data-dir", dataDir, gone}),
		"reject":  rejectMain(dp, []string{"-data-dir", dataDir, "-reason", "no", gone}),
		"cancel":  cancelMain(dp, []string{"-data-dir", dataDir, gone}),
		"rounds":  roundsRun([]string{"-data-dir", dataDir, gone}, &out),
	} {
		if err == nil {
			t.Errorf("%s of an id whose directory holds no record succeeded", verb)
		}
	}
	if strings.Contains(out.String(), sibling) {
		t.Errorf("rounds of the recordless id printed the sibling: %q", out.String())
	}
	if loaded, err := request.Load(dataDir, sibling); err != nil || loaded.State != request.StateSpecReview {
		t.Errorf("the sibling is %v (err %v), want spec_review untouched", loaded, err)
	}
}
