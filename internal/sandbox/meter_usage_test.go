package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/meter"
)

// writeMeterLedger writes records with the meter's own ledger writer to the
// file the meter would use for sandboxID.
func writeMeterLedger(t *testing.T, ledgerRoot, dataDir, runID, sandboxID string, records ...meter.LedgerRecord) string {
	t.Helper()
	name, err := MeterRunName(dataDir, runID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ledgerRoot, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sandboxID+".jsonl")
	ledger, err := meter.OpenLedger(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		ledger.Append(rec)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func admit(id string, tokens, outputTokens, cost int64) meter.LedgerRecord {
	return meter.LedgerRecord{Kind: meter.LedgerKindAdmit, RequestID: id, EstimateTokens: tokens, EstimateOutputTokens: outputTokens, EstimateCostMicroUSD: cost}
}

func complete(id string) meter.LedgerRecord {
	return meter.LedgerRecord{Kind: meter.LedgerKindComplete, RequestID: id}
}

func TestSandboxMeterUsageSumsSettledRequests(t *testing.T) {
	root, dataDir := t.TempDir(), t.TempDir()
	writeMeterLedger(t, root, dataDir, "run-1", "sb-1",
		admit("a", 5000, 1000, 900),
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7, ReasoningEffort: "low"},
		complete("a"),
		admit("b", 5000, 1000, 900),
		meter.LedgerRecord{InputTokens: 300, OutputTokens: 40, CostMicroUSD: 11, ReasoningEffort: "high"},
		complete("b"),
	)
	usage, err := SandboxMeterUsage(root, dataDir, "run-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	want := MeterUsage{InputTokens: 400, OutputTokens: 60, CostMicroUSD: 18, ReasoningEffort: "high"}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	if usage.Tokens() != 460 {
		t.Errorf("Tokens() = %d, want 460", usage.Tokens())
	}
	if err := usage.CeilingErr(); err != nil {
		t.Errorf("CeilingErr() = %v, want nil", err)
	}
}

func TestSandboxMeterUsageChargesAnUnsettledRequestItsEstimate(t *testing.T) {
	root, dataDir := t.TempDir(), t.TempDir()
	writeMeterLedger(t, root, dataDir, "run-1", "sb-1",
		admit("a", 5000, 1000, 900),
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7},
		complete("a"),
		admit("b", 6000, 1500, 950),
	)
	usage, err := SandboxMeterUsage(root, dataDir, "run-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	// 100 + (6000-1500) input, 20 + 1500 output, 7 + 950 cost.
	want := MeterUsage{InputTokens: 4600, OutputTokens: 1520, CostMicroUSD: 957, Unsettled: 1}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	// A dropped stream is charged in full but is not a ceiling refusal.
	if err := usage.CeilingErr(); err != nil {
		t.Errorf("CeilingErr() = %v, want nil", err)
	}
}

func TestSandboxMeterUsageReportsACeilingRefusal(t *testing.T) {
	root, dataDir := t.TempDir(), t.TempDir()
	writeMeterLedger(t, root, dataDir, "run-1", "sb-1",
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7, ReasoningEffort: "not-a-level"},
		meter.LedgerRecord{Kind: meter.LedgerKindCeilingExceeded, RequestID: "c"},
	)
	usage, err := SandboxMeterUsage(root, dataDir, "run-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	want := MeterUsage{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7, ReasoningEffort: "other", ReasoningEffortAnomaly: true, CeilingExceeded: true}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
	if err := usage.CeilingErr(); !errors.Is(err, ErrRelayCeilingExceeded) {
		t.Errorf("CeilingErr() = %v, want ErrRelayCeilingExceeded", err)
	}
}

func TestSandboxMeterUsageOfASandboxWithNoLedgerIsZero(t *testing.T) {
	usage, err := SandboxMeterUsage(t.TempDir(), t.TempDir(), "run-1", "sb-1")
	if err != nil || usage != (MeterUsage{}) {
		t.Fatalf("usage = %+v, %v; want zero", usage, err)
	}
}

func TestSandboxMeterUsageRefusesAnIDThatIsNotAFileName(t *testing.T) {
	for _, id := range []string{"", "../other-run/sb", "a/b", ".", "..", ".hidden"} {
		if usage, err := SandboxMeterUsage(t.TempDir(), t.TempDir(), "run-1", id); err == nil {
			t.Errorf("sandbox id %q: usage = %+v, want an error", id, usage)
		}
	}
	if _, err := SandboxMeterUsage("", t.TempDir(), "run-1", "sb-1"); err == nil {
		t.Error("an empty ledger root was accepted")
	}
}

// recordLedger records a sandbox of the run with the ledger file the meter
// would write for it.
func recordLedger(t *testing.T, ledgerRoot, dataDir, runID, name, sandboxID string) {
	t.Helper()
	path, err := meterLedgerPath(ledgerRoot, dataDir, runID, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordSandbox(dataDir, runID, SandboxRecord{Name: name, ID: sandboxID, Ledger: path}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordedMeterSpendSumsTheRunsRecordedLedgersOnly(t *testing.T) {
	root, dataDir := t.TempDir(), t.TempDir()
	writeMeterLedger(t, root, dataDir, "run-1", "sb-1",
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7})
	writeMeterLedger(t, root, dataDir, "run-1", "sb-2",
		meter.LedgerRecord{InputTokens: 300, OutputTokens: 40, CostMicroUSD: 11},
		admit("x", 1000, 400, 50))
	writeMeterLedger(t, root, dataDir, "run-1", "sb-unrecorded",
		meter.LedgerRecord{InputTokens: 5555, OutputTokens: 5555, CostMicroUSD: 5555})
	writeMeterLedger(t, root, dataDir, "run-2", "sb-3",
		meter.LedgerRecord{InputTokens: 9999, OutputTokens: 9999, CostMicroUSD: 9999})
	recordLedger(t, root, dataDir, "run-1", "bg-a-1", "sb-1")
	recordLedger(t, root, dataDir, "run-1", "bg-a-2", "sb-2")
	recordLedger(t, root, dataDir, "run-1", "bg-a-3", "sb-made-no-request")
	recordLedger(t, root, dataDir, "run-2", "bg-b-1", "sb-3")
	if err := RecordSandbox(dataDir, "run-1", SandboxRecord{Name: "bg-a-4"}); err != nil {
		t.Fatal(err)
	}

	tokens, cost, err := RecordedMeterSpend(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	// 120 + 340 + the unsettled estimate 1000 tokens; 7 + 11 + 50 cost.
	if tokens != 1460 || cost != 68 {
		t.Fatalf("RecordedMeterSpend = %d tokens, %d micro-USD; want 1460, 68", tokens, cost)
	}
	if tokens, cost, err := RecordedMeterSpend(dataDir, "run-never-launched"); err != nil || tokens != 0 || cost != 0 {
		t.Fatalf("a run with no record: %d, %d, %v", tokens, cost, err)
	}
}

func TestRunRelaySpendIncludesWhatTheRunsSandboxesSpent(t *testing.T) {
	root, dataDir := t.TempDir(), t.TempDir()
	writeMeterLedger(t, root, dataDir, "run-1", "sb-1",
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7})
	recordLedger(t, root, dataDir, "run-1", "bg-a-1", "sb-1")
	tokens, cost, err := RunRelaySpend(dataDir, "run-1")
	if err != nil || tokens != 120 || cost != 7 {
		t.Fatalf("RunRelaySpend = %d, %d, %v; want 120, 7", tokens, cost, err)
	}
}

func TestRecordedMeterSpendFailsOnALedgerItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	root, dataDir := t.TempDir(), t.TempDir()
	path := writeMeterLedger(t, root, dataDir, "run-1", "sb-1",
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7})
	recordLedger(t, root, dataDir, "run-1", "bg-a-1", "sb-1")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if tokens, cost, err := RecordedMeterSpend(dataDir, "run-1"); err == nil {
		t.Fatalf("RecordedMeterSpend = %d, %d, nil; want an error", tokens, cost)
	}
	if tokens, cost, err := RunRelaySpend(dataDir, "run-1"); err == nil {
		t.Fatalf("RunRelaySpend = %d, %d, nil; want an error", tokens, cost)
	}
}
