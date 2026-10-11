package meter

import (
	pb "buildgate/internal/meter/middlewarepb"
	"path/filepath"
	"testing"
	"time"
)

// Every way a request can end leaves its own trace in the ledger, written by
// the meter from the response head, so the factory can tell an answered model
// from a refused or a silent one without reading anything the worker printed.
func TestTheLedgerSaysHowEachRequestEnded(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"token_ceiling": 170})
	ask := func(id string) {
		f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: id, body: []byte(jsonBody)})
	}

	ask("answered")
	f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "answered", contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":60,"output_tokens":40}}`)}})
	ask("limited")
	f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "limited", status: 429, retryAfter: "120"})
	ask("limited-by-date")
	f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "limited-by-date", status: 429, retryAfter: f.clock.Now().Add(10 * time.Minute).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")})
	ask("rejected")
	f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "rejected", status: 400, retryAfter: "not a number"})
	ask("failed")
	f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "failed", status: 502})
	// No response ever arrives; the reaper charges the estimate. Its usage
	// record must not read as an answer.
	ask("silent")
	f.clock.Advance(2 * time.Minute)
	if n := f.registry.Reap(); n != 1 {
		t.Fatalf("Reap = %d, want the silent request", n)
	}
	// Still waiting when the ledger is read.
	ask("pending")
	// 100 charged + 27 estimated (silent) + 27 reserved (pending) leaves no
	// room under 170 for two more: the second is refused at the ceiling.
	ask("fits")
	ask("over")

	records, err := ReadLedgerAll(filepath.Join(f.root, "run-1", "sbx-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got := SummarizeCalls(records)
	want := Calls{Answered: 1, RateLimited: 2, Rejected: 1, Failed: 1, Unanswered: 3, DeniedCeiling: 1, RetryAt: f.clock.Now().Add(-2 * time.Minute).Add(10 * time.Minute).UTC().Format(time.RFC3339)}
	if got != want {
		t.Errorf("SummarizeCalls = %+v\nwant           %+v", got, want)
	}
	if got.Recorded() != 9 || got.Legacy {
		t.Errorf("Recorded = %d, Legacy = %v; want 9 and a ledger that is not legacy", got.Recorded(), got.Legacy)
	}
	settled := map[string]LedgerRecord{}
	for _, rec := range records {
		if rec.Kind == LedgerKindComplete {
			settled[rec.RequestID] = rec
		}
	}
	for id, want := range map[string]LedgerRecord{
		"answered": {Settled: SettledExact, Status: 200},
		"limited":  {Settled: SettledReleased, Status: 429, RetryAfterSeconds: 120},
		"rejected": {Settled: SettledReleased, Status: 400},
		"silent":   {Settled: SettledEstimate},
	} {
		rec := settled[id]
		if rec.Settled != want.Settled || rec.Status != want.Status || rec.RetryAfterSeconds != want.RetryAfterSeconds {
			t.Errorf("complete record of %q = settled %q status %d retry-after %d, want %q %d %d", id, rec.Settled, rec.Status, rec.RetryAfterSeconds, want.Settled, want.Status, want.RetryAfterSeconds)
		}
	}
}

func TestTheWindowBudgetAndTheRequestRateLeaveADenyRecord(t *testing.T) {
	for kind, tc := range map[string]struct {
		overrides map[string]any
		want      Calls
	}{
		LedgerKindRateLimited:    {map[string]any{"requests_per_minute": 1}, Calls{Answered: 1, DeniedRate: 1}},
		LedgerKindBudgetExceeded: {map[string]any{"token_budget": 50, "token_window_seconds": 3600}, Calls{Answered: 1, DeniedBudget: 1}},
	} {
		f := newFixture(t)
		config := policyStruct(t, tc.overrides)
		f.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)},
			httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":60,"output_tokens":40}}`)}})
		if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetReasonCode() != kind {
			t.Fatalf("%s: second request = %q, want it refused with that code", kind, r.GetReasonCode())
		}
		records, err := ReadLedgerAll(filepath.Join(f.root, "run-1", "sbx-1.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if got := SummarizeCalls(records); got != tc.want || got.Denied() != 1 {
			t.Errorf("%s: calls = %+v, want %+v", kind, got, tc.want)
		}
	}
}

// The shape the ChatGPT usage limit left in a ledger written before the
// complete record said how a request ended (2026-10-11): admits, a usage
// record for each answered request, and bare completes for the refused ones.
func TestALedgerFromBeforeSettlementWasRecordedStillCounts(t *testing.T) {
	records := []LedgerRecord{
		{Kind: LedgerKindAdmit, RequestID: "a"},
		{InputTokens: 10, OutputTokens: 5},
		{Kind: LedgerKindComplete, RequestID: "a"},
		{Kind: LedgerKindAdmit, RequestID: "b"},
		{Kind: LedgerKindComplete, RequestID: "b"},
		{Kind: LedgerKindAdmit, RequestID: "c"},
		{Kind: LedgerKindComplete, RequestID: "c"},
	}
	if got, want := SummarizeCalls(records), (Calls{Answered: 1, Failed: 2, Legacy: true}); got != want {
		t.Errorf("SummarizeCalls = %+v, want %+v", got, want)
	}
	if got := SummarizeCalls(nil); got.Recorded() != 0 {
		t.Errorf("an empty ledger = %+v", got)
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	header := func(v string) []*pb.HttpHeader { return []*pb.HttpHeader{{Name: "Retry-After", Value: v}} }
	for value, want := range map[string]int64{
		"120":                           120,
		" 45 ":                          45,
		"0":                             0,
		"-5":                            0,
		"99999999":                      0,
		"soon":                          0,
		"Sun, 11 Oct 2026 12:05:00 GMT": 300,
		"Sun, 11 Oct 2026 11:00:00 GMT": 0,
	} {
		if got := retryAfterSeconds(header(value), now); got != want {
			t.Errorf("Retry-After %q = %d, want %d", value, got, want)
		}
	}
	if got := retryAfterSeconds(nil, now); got != 0 {
		t.Errorf("no header = %d", got)
	}
	two := []*pb.HttpHeader{{Name: "retry-after", Value: "soon"}, {Name: "RETRY-AFTER", Value: "30"}}
	if got := retryAfterSeconds(two, now); got != 30 {
		t.Errorf("an unreadable header before a readable one = %d, want 30", got)
	}
}

// A success head whose usage could not be read is not an answer the factory
// can vouch for: the worker may have received nothing.
func TestASuccessHeadWithUnreadUsageIsNotCountedAsAnswered(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, nil)
	f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "cut", body: []byte(jsonBody)})
	f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "cut", contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tok`)}, noFinal: true, sessionEnd: true})
	records, err := ReadLedgerAll(filepath.Join(f.root, "run-1", "sbx-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := SummarizeCalls(records), (Calls{AnsweredUnread: 1}); got != want {
		t.Errorf("SummarizeCalls = %+v, want %+v", got, want)
	}
}

// An id may be admitted again once its first request settled; the second,
// never answered, must still be counted.
func TestARequestIDAdmittedAgainIsCountedAgain(t *testing.T) {
	records := []LedgerRecord{
		{Kind: LedgerKindAdmit, RequestID: "a"},
		{Kind: LedgerKindComplete, RequestID: "a", Settled: SettledExact, Status: 200},
		{Kind: LedgerKindAdmit, RequestID: "a"},
	}
	if got, want := SummarizeCalls(records), (Calls{Answered: 1, Unanswered: 1}); got != want {
		t.Errorf("SummarizeCalls = %+v, want %+v", got, want)
	}
}
