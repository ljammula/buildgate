package meter

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// DefaultReservationMaxAge is how long an admitted request may stay without
// a response before the reaper charges its estimate and releases it.
const DefaultReservationMaxAge = 30 * time.Minute

// sandboxIDPattern bounds a sandbox id to a safe ledger file name.
var sandboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Config is what a Registry (and the Server around it) is built from.
type Config struct {
	// LedgerRoot holds <run>/<sandbox_id>.jsonl, one ledger per sandbox.
	LedgerRoot string
	// Now is the clock for windows, ledger stamps and reservation ages.
	Now    func() time.Time
	Logger *log.Logger
	// ReservationMaxAge is the reaper's age limit; zero means
	// DefaultReservationMaxAge.
	ReservationMaxAge time.Duration
}

func (c Config) withDefaults() (Config, error) {
	if c.LedgerRoot == "" {
		return c, errors.New("meter: ledger root is required")
	}
	if c.Now == nil {
		return c, errors.New("meter: a clock is required")
	}
	if c.Logger == nil {
		c.Logger = log.New(io.Discard, "", 0)
	}
	if c.ReservationMaxAge <= 0 {
		c.ReservationMaxAge = DefaultReservationMaxAge
	}
	return c, nil
}

// pendingRequest is what the meter keeps of an admitted request until its
// response settles it: the reserved estimate and what the response side needs
// to read usage. Never the request body.
type pendingRequest struct {
	estimateInputTokens  int
	estimateOutputTokens int
	estimateCostMicroUSD int64
	stream               bool
	effort               string
	admittedAt           time.Time
}

func (p pendingRequest) estimateTokens() int64 {
	return int64(p.estimateInputTokens) + int64(p.estimateOutputTokens)
}

// runEntry is the state of one sandbox. mu guards everything below it; the
// registry's own lock is never held while mu is.
type runEntry struct {
	once    sync.Once
	loadErr error

	mu             sync.Mutex
	now            func() time.Time
	logger         *log.Logger
	policy         Policy
	account        *Account
	ledger         *Ledger
	requestLimiter *WindowLimiter
	reservedTokens int64
	reservedCost   int64
	pending        map[string]pendingRequest
}

// Registry is the set of sandboxes the meter has seen, keyed by sandbox id.
type Registry struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*runEntry
}

// NewRegistry builds an empty registry.
func NewRegistry(cfg Config) (*Registry, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Registry{cfg: cfg, entries: make(map[string]*runEntry)}, nil
}

// entry returns the sandbox's entry, loading it from its ledger on first
// sight with the policy of that first request. A load failure is returned and
// forgotten, so the next request retries.
func (r *Registry) entry(sandboxID string, policy Policy) (*runEntry, error) {
	if !sandboxIDPattern.MatchString(sandboxID) {
		return nil, fmt.Errorf("sandbox id %q is not a safe ledger name", sandboxID)
	}
	r.mu.Lock()
	e := r.entries[sandboxID]
	if e == nil {
		e = &runEntry{}
		r.entries[sandboxID] = e
	}
	r.mu.Unlock()
	e.once.Do(func() { e.loadErr = r.load(e, sandboxID, policy) })
	if e.loadErr != nil {
		r.mu.Lock()
		if r.entries[sandboxID] == e {
			delete(r.entries, sandboxID)
		}
		r.mu.Unlock()
		return nil, e.loadErr
	}
	return e, nil
}

// lookup returns an already-loaded entry, or nil.
func (r *Registry) lookup(sandboxID string) *runEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[sandboxID]
}

// snapshot is every entry at this moment.
func (r *Registry) snapshot() []*runEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*runEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

// load opens the sandbox's ledger, builds its account and restores the
// totals the ledger holds.
func (r *Registry) load(e *runEntry, sandboxID string, policy Policy) error {
	dir := filepath.Join(r.cfg.LedgerRoot, policy.Run)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create ledger directory: %w", err)
	}
	path := filepath.Join(dir, sandboxID+".jsonl")
	records, err := ReadLedgerAll(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read ledger %s: %w", path, err)
	}
	ledger, err := OpenLedger(path, r.cfg.Logger)
	if err != nil {
		return err
	}
	account := NewAccount(AccountConfig{
		TokenBudget:         int(policy.TokenBudget),
		TokenBudgetWindow:   time.Duration(policy.TokenWindowSeconds) * time.Second,
		CostBudgetMicroUSD:  policy.CostBudgetMicroUSD,
		CostBudgetWindow:    time.Duration(policy.CostWindowSeconds) * time.Second,
		TokenCeiling:        int(policy.TokenCeiling),
		CostCeilingMicroUSD: policy.CostCeilingMicroUSD,
		Prices:              policy.Prices,
		UsageMarker:         "meter:" + sandboxID,
		Logger:              r.cfg.Logger,
		Ledger:              ledger,
		Now:                 r.cfg.Now,
	})
	in, out, cost := ReplayLedger(records)
	account.Restore(in, out, cost)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = r.cfg.Now
	e.logger = r.cfg.Logger
	e.policy = policy
	e.account = account
	e.ledger = ledger
	e.pending = make(map[string]pendingRequest)
	if policy.RequestsPerMinute > 0 {
		e.requestLimiter = NewWindowLimiter(int(policy.RequestsPerMinute), time.Minute)
	}
	return nil
}

// ReplayLedger rebuilds the consumed totals a ledger records: every usage
// record, plus the estimate of each admit record that has no complete record
// (a request in flight when the meter stopped).
func ReplayLedger(records []LedgerRecord) (inputTokens, outputTokens, costMicroUSD int64) {
	var admits []LedgerRecord
	completed := make(map[string]bool)
	for _, rec := range records {
		switch rec.Kind {
		case "":
			inputTokens = SaturatingAdd(inputTokens, rec.InputTokens)
			outputTokens = SaturatingAdd(outputTokens, rec.OutputTokens)
			costMicroUSD = SaturatingAdd(costMicroUSD, rec.CostMicroUSD)
		case LedgerKindAdmit:
			admits = append(admits, rec)
		case LedgerKindComplete:
			completed[rec.RequestID] = true
		}
	}
	for _, rec := range admits {
		if completed[rec.RequestID] {
			continue
		}
		inputTokens = SaturatingAdd(inputTokens, max(rec.EstimateTokens-rec.EstimateOutputTokens, 0))
		outputTokens = SaturatingAdd(outputTokens, rec.EstimateOutputTokens)
		costMicroUSD = SaturatingAdd(costMicroUSD, rec.EstimateCostMicroUSD)
	}
	return inputTokens, outputTokens, costMicroUSD
}

// Usage returns the sandbox's committed totals, or false when the meter has
// not seen the sandbox.
func (r *Registry) Usage(sandboxID string) (inputTokens, outputTokens, costMicroUSD int64, ok bool) {
	e := r.lookup(sandboxID)
	if e == nil {
		return 0, 0, 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.account == nil {
		return 0, 0, 0, false
	}
	inputTokens, outputTokens, costMicroUSD, _ = e.account.Usage()
	return inputTokens, outputTokens, costMicroUSD, true
}

// Reap settles every reservation older than the configured age at its
// estimate and returns how many it settled.
func (r *Registry) Reap() int {
	cutoff := r.cfg.Now().Add(-r.cfg.ReservationMaxAge)
	reaped := 0
	for _, e := range r.snapshot() {
		reaped += e.expire(cutoff)
	}
	return reaped
}

// Close closes every ledger file.
func (r *Registry) Close() {
	for _, e := range r.snapshot() {
		e.mu.Lock()
		if e.ledger != nil {
			_ = e.ledger.Close()
		}
		e.mu.Unlock()
	}
}
