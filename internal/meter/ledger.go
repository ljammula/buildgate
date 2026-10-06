package meter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// maxLedgerLineBytes bounds the largest single ledger line ReadLedger will
// scan; it matches internal/sandbox's own bound on a relay log line.
const maxLedgerLineBytes = 4 * 1024 * 1024

// LedgerRecord is one per-event line of the usage ledger: the tokens and cost
// of ONE response (not a cumulative total), the reasoning effort that request
// asked for, and the raw cache-hit count behind the weighted input figure.
// Append writes every field; ReadLedger fills only the four fields crash
// recovery sums (input_tokens, output_tokens, cost_micro_usd,
// reasoning_effort).
type LedgerRecord struct {
	TS                string `json:"ts"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CostMicroUSD      int64  `json:"cost_micro_usd"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	CachedInputTokens int64  `json:"cached_input_tokens,omitempty"`
	// Kind is "" for a usage record (every field above, exactly the line
	// shape the relay has always written) or one of the event kinds below,
	// which carry RequestID and the estimate fields instead of usage. A line
	// with a Kind never counts in ReadLedger's sums.
	Kind string `json:"kind,omitempty"`
	// RequestID links an admit record to its complete record.
	RequestID string `json:"request_id,omitempty"`
	// EstimateTokens/EstimateOutputTokens/EstimateCostMicroUSD are the
	// worst-case estimate an admit record reserved: EstimateTokens is the
	// input plus output tokens, EstimateOutputTokens the output part.
	EstimateTokens       int64 `json:"estimate_tokens,omitempty"`
	EstimateOutputTokens int64 `json:"estimate_output_tokens,omitempty"`
	EstimateCostMicroUSD int64 `json:"estimate_cost_micro_usd,omitempty"`
}

// Ledger event kinds written by the meter service next to usage records.
const (
	// LedgerKindAdmit: a request was admitted and its estimate reserved.
	LedgerKindAdmit = "admit"
	// LedgerKindComplete: an admitted request settled (exact usage, estimate
	// or release); its usage is in a preceding usage record, if any.
	LedgerKindComplete = "complete"
	// LedgerKindCeilingExceeded: a request was refused at a ceiling.
	LedgerKindCeilingExceeded = "ceiling_exceeded"
)

// Ledger is the usage ledger file, opened for append.
type Ledger struct {
	file   *os.File
	logger *log.Logger
}

// OpenLedger opens path for append, creating it when absent. The error text
// is the relay's start-up refusal: a caller that set a ledger path has
// already committed to needing this evidence, so failing to open it must stop
// the process rather than fall back to log-only accounting.
func OpenLedger(path string, logger *log.Logger) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open usage ledger %s: %w (this relay's spend evidence cannot be durably recorded)", path, err)
	}
	return &Ledger{file: f, logger: logger}, nil
}

// Close closes the ledger file.
func (l *Ledger) Close() error {
	return l.file.Close()
}

// Append durably records ONE accounting event's own input/output tokens and
// cost -- not the cumulative totals the "usage: " log line carries -- as a
// single JSON line appended to the ledger file and fsync'd before returning.
// A no-op on a nil Ledger (no ledger path was configured).
// rec.ReasoningEffort is THIS event's own requested reasoning effort (the
// ledger is a per-event record, unlike the "usage: " log line's running
// highest), added as a "reasoning_effort" field when non-empty, omitted
// entirely otherwise, so internal/sandbox's readUsageLedger can distinguish
// "never seen" from a value it must still re-validate before trusting.
// rec.InputTokens is already weighted (Account.AddUsage's weightedMeterInput,
// see cachedInputWeightPercent) -- the same figure the token window/ceiling
// and the cumulative input total use, so the fallback path sums to the same
// consumed-token figure a confirmed docker-logs read would have reported.
// rec.CachedInputTokens (the raw, unweighted cache-hit count this event's own
// parser reported, clamped to [0, meterInputTokens] by WeightedInputTokens) is
// recorded additively as "cached_input_tokens" when > 0, omitted entirely
// otherwise (2026-09-28 operator decision) -- so an audit reading this ledger
// can see both the weighted figure that counted against budgets and the raw
// split behind it, without this relay needing to keep a second, unweighted
// ledger. rec.TS defaults to the current UTC time.
//
// This exists for exactly the case the "usage: " log line cannot cover: that
// line lives only in the meter's own stdout. The ledger file is on a host
// directory the meter's container binds read-write, so the write survives
// however this process ends, and factoryd reads each sandbox's spend from it
// (sandbox.SandboxMeterUsage), never from the log.
//
// One JSON line per call, appended (not rewritten) and fsync'd individually: a
// crash mid-write can only ever corrupt or truncate the LAST line, never an
// earlier one, since each call's write+fsync completes before the next one
// starts (both only ever run while the relay's budgetMu is held). A write or
// fsync failure is logged and otherwise ignored -- this is a durability
// backstop for the crash case, not the primary accounting record, so it must
// never make an otherwise-successful request fail.
func (l *Ledger) Append(rec LedgerRecord) {
	if l == nil {
		return
	}
	ts := rec.TS
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if rec.Kind != "" {
		l.appendLine(eventLine(ts, rec))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{\"ts\":%q,\"input_tokens\":%d,\"output_tokens\":%d,\"cost_micro_usd\":%d",
		ts, rec.InputTokens, rec.OutputTokens, rec.CostMicroUSD)
	if rec.ReasoningEffort != "" {
		fmt.Fprintf(&b, ",\"reasoning_effort\":%q", rec.ReasoningEffort)
	}
	if rec.CachedInputTokens > 0 {
		fmt.Fprintf(&b, ",\"cached_input_tokens\":%d", rec.CachedInputTokens)
	}
	b.WriteString("}\n")
	l.appendLine(b.String())
}

// eventLine is the JSON line of a non-usage record: kind, request id and,
// when set, the estimate fields -- never input_tokens/output_tokens/
// cost_micro_usd.
func eventLine(ts string, rec LedgerRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\"ts\":%q,\"kind\":%q", ts, rec.Kind)
	if rec.RequestID != "" {
		fmt.Fprintf(&b, ",\"request_id\":%q", rec.RequestID)
	}
	if rec.EstimateTokens > 0 {
		fmt.Fprintf(&b, ",\"estimate_tokens\":%d", rec.EstimateTokens)
	}
	if rec.EstimateOutputTokens > 0 {
		fmt.Fprintf(&b, ",\"estimate_output_tokens\":%d", rec.EstimateOutputTokens)
	}
	if rec.EstimateCostMicroUSD > 0 {
		fmt.Fprintf(&b, ",\"estimate_cost_micro_usd\":%d", rec.EstimateCostMicroUSD)
	}
	b.WriteString("}\n")
	return b.String()
}

// appendLine writes one finished line and fsyncs it; a failure is logged.
func (l *Ledger) appendLine(line string) {
	if _, err := l.file.WriteString(line); err != nil {
		l.logger.Printf("usage ledger append failed: %v", err)
		return
	}
	if err := l.file.Sync(); err != nil {
		l.logger.Printf("usage ledger fsync failed: %v", err)
	}
}

// ReadLedger returns every well-formed ledger line of path, in order. A blank
// line, and a truncated or corrupt one -- the shape a crash mid-write leaves
// -- is skipped, never an error and never a stop: every other line was fsync'd
// before the next was appended, so it stays trustworthy. An error is returned
// only when the file cannot be opened (the returned slice is then nil) or the
// scan itself fails (the records read so far are returned with it).
func ReadLedger(path string) ([]LedgerRecord, error) {
	return scanLedger(path, false)
}

// ReadLedgerAll is ReadLedger's full reader: it also returns the event
// records (admit, complete, ceiling_exceeded) and fills every field of each
// record, which is what the meter service replays after a restart.
func ReadLedgerAll(path string) ([]LedgerRecord, error) {
	return scanLedger(path, true)
}

// scanLedger reads path line by line. withEvents false returns usage records
// only, with the four fields ReadLedger has always filled.
func scanLedger(path string, withEvents bool) ([]LedgerRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []LedgerRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxLedgerLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if withEvents {
			var entry LedgerRecord
			if json.Unmarshal([]byte(line), &entry) == nil {
				records = append(records, entry)
			}
			continue
		}
		var entry struct {
			InputTokens     int64           `json:"input_tokens"`
			OutputTokens    int64           `json:"output_tokens"`
			CostMicroUSD    int64           `json:"cost_micro_usd"`
			ReasoningEffort string          `json:"reasoning_effort"`
			Kind            json.RawMessage `json:"kind"`
		}
		if jsonErr := json.Unmarshal([]byte(line), &entry); jsonErr != nil {
			continue
		}
		if kind := string(entry.Kind); kind != "" && kind != "null" && kind != `""` {
			continue
		}
		records = append(records, LedgerRecord{
			InputTokens:     entry.InputTokens,
			OutputTokens:    entry.OutputTokens,
			CostMicroUSD:    entry.CostMicroUSD,
			ReasoningEffort: entry.ReasoningEffort,
		})
	}
	return records, scanner.Err()
}
