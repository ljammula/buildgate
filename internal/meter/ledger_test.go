package meter

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedgerAppendWritesTheDocumentedLineShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	ledger, err := OpenLedger(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	defer ledger.Close()
	ledger.Append(LedgerRecord{TS: "2026-10-04T00:00:00Z", InputTokens: 7, OutputTokens: 3, CostMicroUSD: 11})
	ledger.Append(LedgerRecord{TS: "2026-10-04T00:00:01Z", InputTokens: 1, OutputTokens: 2, CostMicroUSD: 0, ReasoningEffort: "high", CachedInputTokens: 9})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ts":"2026-10-04T00:00:00Z","input_tokens":7,"output_tokens":3,"cost_micro_usd":11}` + "\n" +
		`{"ts":"2026-10-04T00:00:01Z","input_tokens":1,"output_tokens":2,"cost_micro_usd":0,"reasoning_effort":"high","cached_input_tokens":9}` + "\n"
	if string(got) != want {
		t.Fatalf("ledger = %q, want %q", got, want)
	}
}

func TestLedgerAppendOnNilLedgerIsANoOp(t *testing.T) {
	var ledger *Ledger
	ledger.Append(LedgerRecord{InputTokens: 1})
}

func TestReadLedgerSkipsBlankAndCorruptLinesAndKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	content := `{"ts":"a","input_tokens":1,"output_tokens":2,"cost_micro_usd":3,"reasoning_effort":"low"}` + "\n\n" +
		`{"ts":"b","input_tokens":` + "\n" +
		strings.Repeat(" ", 3) + "\n" +
		`{"input_tokens":10,"output_tokens":20,"cost_micro_usd":30}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := ReadLedgerAll(path)
	if err != nil {
		t.Fatalf("ReadLedgerAll: %v", err)
	}
	want := []LedgerRecord{
		{TS: "a", InputTokens: 1, OutputTokens: 2, CostMicroUSD: 3, ReasoningEffort: "low"},
		{InputTokens: 10, OutputTokens: 20, CostMicroUSD: 30},
	}
	if len(records) != len(want) || records[0] != want[0] || records[1] != want[1] {
		t.Fatalf("records = %+v, want %+v", records, want)
	}
}

func TestReadLedgerMissingFileIsAnError(t *testing.T) {
	records, err := ReadLedgerAll(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err == nil || records != nil {
		t.Fatalf("ReadLedgerAll(absent) = %v, %v, want nil and an error", records, err)
	}
}
