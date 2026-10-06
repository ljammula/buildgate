package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"buildgate/internal/meter"
)

// MeterUsage is what a sandbox's meter ledger records: the same totals the
// meter itself rebuilds after a restart (meter.ReplayLedger), so the factory
// and the meter never disagree on what a sandbox spent.
type MeterUsage struct {
	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
	// ReasoningEffort is the highest effort any request asked for;
	// ReasoningEffortAnomaly reports one the factory does not recognize.
	ReasoningEffort        string
	ReasoningEffortAnomaly bool
	// CeilingExceeded reports a request the meter refused at a ceiling.
	CeilingExceeded bool
	// Unsettled counts requests admitted and never completed. Their
	// worst-case estimates are in the totals above.
	Unsettled int
}

// Tokens is input plus output, the figure a token ceiling limits.
func (u MeterUsage) Tokens() int64 { return meter.SaturatingAdd(u.InputTokens, u.OutputTokens) }

// CeilingErr is an error wrapping ErrRelayCeilingExceeded when the meter
// refused a request at a ceiling, nil otherwise. A request left unsettled is
// not one: a client that drops a stream (a timeout, a cancelled review call)
// leaves an admit with no complete record on an ordinary run. Its worst-case
// estimate is already in the totals, so it counts against the run's ceilings
// in full; the caller records the spend as partial.
func (u MeterUsage) CeilingErr() error {
	if u.CeilingExceeded {
		return fmt.Errorf("%w: the meter refused a request at the run's ceiling", ErrRelayCeilingExceeded)
	}
	return nil
}

// meterLedgerDir is the run's directory under the meter's ledger root.
func meterLedgerDir(ledgerRoot, dataDir, runID string) (string, error) {
	if ledgerRoot == "" {
		return "", errors.New("meter ledger root is required")
	}
	name, err := MeterRunName(dataDir, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(ledgerRoot, name), nil
}

// SandboxMeterUsage reads one sandbox's ledger, named by the gateway's id for
// the sandbox. A sandbox that made no model request has no ledger and no
// usage.
func SandboxMeterUsage(ledgerRoot, dataDir, runID, sandboxID string) (MeterUsage, error) {
	path, err := meterLedgerPath(ledgerRoot, dataDir, runID, sandboxID)
	if err != nil {
		return MeterUsage{}, err
	}
	return readMeterLedger(path)
}

// meterLedgerPath is the file the meter writes for one sandbox.
func meterLedgerPath(ledgerRoot, dataDir, runID, sandboxID string) (string, error) {
	if sandboxID == "" || sandboxID != filepath.Base(sandboxID) || strings.HasPrefix(sandboxID, ".") {
		return "", fmt.Errorf("meter usage: invalid sandbox id %q", sandboxID)
	}
	dir, err := meterLedgerDir(ledgerRoot, dataDir, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sandboxID+".jsonl"), nil
}

// RecordedMeterSpend returns the tokens (input plus output) and cost in
// micro-USD of every sandbox the run recorded with a ledger. A run that
// launched nothing through a Runtime has spent nothing here; a ledger that
// cannot be read is an error, never a zero.
func RecordedMeterSpend(dataDir, runID string) (tokens, costMicroUSD int64, err error) {
	records, err := RecordedSandboxes(dataDir, runID)
	if err != nil {
		return 0, 0, err
	}
	for _, rec := range records {
		if rec.Ledger == "" {
			continue
		}
		usage, err := readMeterLedger(rec.Ledger)
		if err != nil {
			return 0, 0, err
		}
		tokens = meter.SaturatingAdd(tokens, usage.Tokens())
		costMicroUSD = meter.SaturatingAdd(costMicroUSD, usage.CostMicroUSD)
	}
	return tokens, costMicroUSD, nil
}

func readMeterLedger(path string) (MeterUsage, error) {
	records, err := meter.ReadLedgerAll(path)
	if errors.Is(err, fs.ErrNotExist) {
		return MeterUsage{}, nil
	}
	if err != nil {
		return MeterUsage{}, fmt.Errorf("read meter ledger %s: %w", path, err)
	}
	var usage MeterUsage
	usage.InputTokens, usage.OutputTokens, usage.CostMicroUSD = meter.ReplayLedger(records)
	completed := map[string]bool{}
	for _, rec := range records {
		if rec.Kind == meter.LedgerKindComplete {
			completed[rec.RequestID] = true
		}
	}
	for _, rec := range records {
		switch rec.Kind {
		case meter.LedgerKindCeilingExceeded:
			usage.CeilingExceeded = true
		case meter.LedgerKindAdmit:
			if !completed[rec.RequestID] {
				usage.Unsettled++
			}
		case "":
			usage.noteEffort(rec.ReasoningEffort)
		}
	}
	return usage, nil
}

// noteEffort keeps the highest reasoning effort seen.
func (u *MeterUsage) noteEffort(effort string) {
	if effort == "" {
		return
	}
	if !meter.ValidReasoningEffort(effort) {
		effort = "other"
		u.ReasoningEffortAnomaly = true
	}
	if meter.ReasoningEffortHigherThan(effort, u.ReasoningEffort) {
		u.ReasoningEffort = effort
	}
}
