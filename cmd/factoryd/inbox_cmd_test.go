package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
)

// saveWaitingRequest writes a request in state, entered `ago` before now.
func saveWaitingRequest(t *testing.T, dataDir, id, title string, state request.State, ago time.Duration, mutate func(*request.Request)) {
	t.Helper()
	now := time.Now()
	r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, now.Add(-48*time.Hour))
	if err := request.SaveText(dataDir, id, title+"\nmore detail\n"); err != nil {
		t.Fatal(err)
	}
	r.State = state
	if mutate != nil {
		mutate(r)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
	// Save may stamp its own timestamps on a state change; pin the one under test.
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	loaded.EnteredAt = now.Add(-ago).UTC().Format(time.RFC3339Nano)
	loaded.WaitingSince = ""
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

func TestInboxListsWaitingRequestsAcrossProfilesOldestFirst(t *testing.T) {
	f := newProfileFixture(t, "default", "work")
	t.Setenv(consolelink.EnvVar, "")
	def, work := f.roots["default"], f.roots["work"]

	saveWaitingRequest(t, def, "spec-a", "Add login page", request.StateSpecReview, 5*time.Hour, nil)
	saveWaitingRequest(t, work, "oracle-b", "Fix the cache", request.StateOracleReview, 30*time.Minute, nil)
	saveWaitingRequest(t, work, "plan-c", "Split the module", request.StatePlanReview, 26*time.Hour, nil)
	saveWaitingRequest(t, def, "pr-d", "Ship the export", request.StatePRReview, 2*time.Hour, func(r *request.Request) {
		r.Tickets = []request.Ticket{{Index: 1, PRURL: "https://github.com/o/r/pull/7"}}
	})
	saveWaitingRequest(t, work, "halt-e", "Rename config", request.StateHalted, 3*time.Hour, func(r *request.Request) {
		r.Error = "spec drafting failed:\nforged: line"
	})
	saveWaitingRequest(t, def, "quar-f", "Bump deps", request.StateQuarantined, 10*time.Minute, func(r *request.Request) {
		r.Error = "verify failed"
	})
	// Not waiting on the operator.
	saveWaitingRequest(t, def, "busy-g", "In flight", request.StateBuilding, 1*time.Hour, nil)
	saveWaitingRequest(t, work, "done-h", "Finished", request.StateDone, 1*time.Hour, nil)

	var out, warn bytes.Buffer
	if err := inboxRun(nil, &out, &warn); err != nil {
		t.Fatalf("inboxRun: %v", err)
	}
	text := out.String()

	var ids []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			ids = append(ids, strings.Fields(line)[3])
		}
	}
	if want := []string{"plan-c", "spec-a", "halt-e", "pr-d", "oracle-b", "quar-f"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want oldest first %v\n%s", ids, want, text)
	}
	for _, want := range []string{
		"1d2h  work  plan_review  plan-c  Split the module",
		"5h00m  default  spec_review  spec-a  Add login page",
		"  factoryd approve -config default spec-a\n",
		"  factoryd reject -config default -reason \"...\" spec-a\n",
		"  factoryd approve -config work plan-c\n",
		"  review and merge: https://github.com/o/r/pull/7\n",
		"  reason: spec drafting failed: forged: line\n",
		"  next: ",
		"reason: verify failed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "busy-g") || strings.Contains(text, "done-h") {
		t.Errorf("requests that do not wait on the operator are listed:\n%s", text)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warnings: %s", warn.String())
	}
}

func TestInboxShowsConsoleLinkWhenOneResolves(t *testing.T) {
	f := newProfileFixture(t, "default")
	t.Setenv(consolelink.EnvVar, "http://console.example:8090")
	saveWaitingRequest(t, f.roots["default"], "spec-a", "Add login", request.StateSpecReview, time.Hour, nil)
	var out bytes.Buffer
	if err := inboxRun(nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  console: http://console.example:8090/requests/spec-a\n") {
		t.Errorf("output lacks the console link:\n%s", out.String())
	}
}

func TestInboxEmpty(t *testing.T) {
	newProfileFixture(t, "default", "work")
	var out bytes.Buffer
	if err := inboxRun(nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Nothing is waiting on you.\n" {
		t.Errorf("output = %q", out.String())
	}
	out.Reset()
	if err := inboxRun([]string{"-json"}, &out, &bytes.Buffer{}); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("-json on an empty inbox = %q, %v; want []", out.String(), err)
	}
}

func TestInboxJSONIsTheSameDataAsAnArray(t *testing.T) {
	f := newProfileFixture(t, "default", "work")
	t.Setenv(consolelink.EnvVar, "")
	saveWaitingRequest(t, f.roots["work"], "spec-a", "Add login", request.StateSpecReview, 2*time.Hour, nil)
	saveWaitingRequest(t, f.roots["default"], "halt-b", "Rename", request.StateHalted, time.Hour, func(r *request.Request) { r.Error = "boom" })
	var out bytes.Buffer
	if err := inboxRun([]string{"-json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var got []inboxEntry
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if len(got) != 2 || got[0].ID != "spec-a" || got[1].ID != "halt-b" {
		t.Fatalf("entries = %+v", got)
	}
	if got[0].Profile != "work" || got[0].DataDir != f.roots["work"] || got[0].State != "spec_review" || got[0].Title != "Add login" ||
		got[0].Approve != "factoryd approve -config work spec-a" || got[0].AgeSeconds < 7100 {
		t.Errorf("spec entry = %+v", got[0])
	}
	if got[1].Reason != "boom" || got[1].Next == "" {
		t.Errorf("halted entry = %+v", got[1])
	}
}

func TestInboxCountsASharedDataDirOnce(t *testing.T) {
	f := newProfileFixture(t, "default")
	// A second profile on the same data dir.
	shared := f.roots["default"]
	writeSessionConfig(t, sessionconfig.ProfilePath("alias"), "data_dir: "+shared+"\n")
	saveWaitingRequest(t, shared, "spec-a", "Add login", request.StateSpecReview, time.Hour, nil)
	var out bytes.Buffer
	if err := inboxRun([]string{"-json"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var got []inboxEntry
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 1 {
		t.Fatalf("want one entry, got %d (%v)", len(got), err)
	}
}
