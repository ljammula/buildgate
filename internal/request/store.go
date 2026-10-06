package request

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// requestFileName is the publish signal for a request directory, exactly
// the way entry.json is for cmd/factoryd's own queue.QueueEntry: List
// skips a directory that doesn't have one yet, so a concurrent driver
// poll -- or a crash between writing request.md and request.json -- only
// ever observes either nothing published yet or a fully published
// request, never a half-written one.
const requestFileName = "request.json"

// TextFileName is request.md's own filename: the verbatim request text,
// written before request.json (see Dir's own doc comment on write
// order).
const TextFileName = "request.md"

// Dir returns <data-dir>/requests/<id>, one request's own directory.
//
// Write order within it matters, exactly like queue.QueueEntry's own
// entry.json: TextFileName first, then request.json LAST via Save's own
// atomic temp-file+rename -- request.json's existence is this package's
// publish signal.
func Dir(dataDir, id string) string {
	return filepath.Join(dataDir, "requests", id)
}

// Path returns where a request's JSON record lives.
func Path(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), requestFileName)
}

// ImportedSpecFileName is the spec an operator handed over with `factoryd
// submit -spec-file`, kept as submitted beside the request record.
const ImportedSpecFileName = "imported-spec.md"

// ImportedSpecPath returns where a request's handed-over spec lives.
func ImportedSpecPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), ImportedSpecFileName)
}

// ImportedTicketsDirName holds the tickets an operator handed over with
// `factoryd submit -plan-dir`, kept as submitted beside the request record.
const ImportedTicketsDirName = "imported-tickets"

// ImportedTicketsDir returns where a request's handed-over tickets live.
func ImportedTicketsDir(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), ImportedTicketsDirName)
}

// TextPath returns where a request's verbatim source text lives.
func TextPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), TextFileName)
}

// SpecPath returns where a request's drafted spec.md lives -- the file a
// spec approval covers (see specFileName's own doc comment in
// approve.go, the constant this wraps). Exported so a caller outside
// this package (the console request board's API handlers, reading the
// file to render in the request board) does not have to re-derive the
// filename itself.
func SpecPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), specFileName)
}

// Title returns the first line of a request's verbatim source text
// (request.md), trimmed and without Markdown heading marks -- the console
// request board's per-row title. A first line that is only a section label
// ("# Goal", "## Request") is skipped for the next non-empty line, the text
// that label introduces.
// Empty if request.md is missing or empty; never an error, since a title is a
// display convenience derived from the file, not durable state a caller
// should have to handle failing to read.
func Title(dataDir, id string) string {
	b, err := os.ReadFile(TextPath(dataDir, id))
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	first := strings.TrimSpace(strings.TrimLeft(lines[0], "#"))
	if strings.HasPrefix(lines[0], "#") && titleSectionLabels[strings.ToLower(strings.TrimSuffix(first, ":"))] && len(lines) > 1 {
		return strings.TrimSpace(strings.TrimLeft(lines[1], "#"))
	}
	return first
}

// titleSectionLabels are headings that name a section, not the request.
var titleSectionLabels = map[string]bool{
	"goal": true, "request": true, "summary": true, "task": true,
	"description": true, "problem": true, "overview": true, "context": true,
}

// SaveText writes a request's verbatim source text to request.md. Callers
// must call this BEFORE Save (see Dir's own doc comment on write order) --
// it does not itself need to be atomic, since nothing reads a request
// directory before request.json exists.
func SaveText(dataDir, id, text string) error {
	dir := Dir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create request dir: %w", err)
	}
	if err := os.WriteFile(TextPath(dataDir, id), []byte(text), 0o600); err != nil {
		return fmt.Errorf("write request text: %w", err)
	}
	return nil
}

// Save writes r atomically (write to a temp file, then rename), the same
// pattern run.Run.Save and cmd/factoryd's saveQueueEntry use, so a crash
// mid-write never leaves a corrupt or torn request.json behind -- a
// reader observes either the old complete file or the new complete file,
// never a partial one.
//
// Before writing, this is also History's own fallback safety net: if
// r.State no longer matches r.prevState (set by Load/New and kept in
// sync by every internal/request transition function's own advance
// call), some caller outside this package moved r.State directly --
// cmd/factoryd's own request driver has several such call sites that
// cannot be changed to call a named transition function -- so that move
// gets exactly one History entry here, attributed to "factory" with
// whatever r.Error already names as Reason. A transition already
// recorded by advance leaves r.prevState == r.State, so this is a no-op
// on the ordinary path, and a plain re-save with no state change at all
// never appends anything.
func (r *Request) Save(dataDir string) error {
	if r.State != r.prevState {
		r.History = append(r.History, Transition{
			From:   r.prevState,
			To:     r.State,
			At:     time.Now().UTC().Format(time.RFC3339Nano),
			By:     factoryActor,
			Reason: r.Error,
		})
		r.prevState = r.State
	}
	dir := Dir(dataDir, r.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create request dir: %w", err)
	}
	path := Path(dataDir, r.ID)
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	return os.Rename(tmp, path)
}

// Load reads one request by id.
func Load(dataDir, id string) (*Request, error) {
	b, err := os.ReadFile(Path(dataDir, id))
	if err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	var r Request
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	r.prevState = r.State
	return &r, nil
}

// List loads every request under <data-dir>/requests, oldest-submitted
// first. A missing requests directory (nothing ever submitted) returns an
// empty slice, not an error -- the same "absent means none yet"
// convention loadStatusRuns already uses.
//
// A request subdirectory with no request.json yet is silently skipped,
// not treated as an error: SaveText/Save's own write order (see Dir's doc
// comment) means a concurrent submit or a crash between those two writes
// can leave one on disk.
func List(dataDir string) ([]*Request, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "requests"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var requests []*Request
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(Path(dataDir, entry.Name())); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("request %q: %w", entry.Name(), err)
		}
		r, err := Load(dataDir, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("request %q: %w", entry.Name(), err)
		}
		requests = append(requests, r)
	}
	sort.SliceStable(requests, func(i, j int) bool {
		return submittedBefore(requests[i].SubmittedAt, requests[j].SubmittedAt)
	})
	return requests, nil
}

// submittedBefore reports whether a's SubmittedAt sorts before b's --
// oldest first. Compares parsed time.Time values, not raw strings, for
// the same reason cmd/factoryd's own queueSubmittedBefore does (RFC3339Nano
// trims trailing fractional zeros, so two timestamps with different
// fractional-digit counts are not safely comparable as plain strings). An
// unparseable value falls back to a raw string comparison rather than
// panicking.
func submittedBefore(a, b string) bool {
	aTime, aErr := time.Parse(time.RFC3339, a)
	bTime, bErr := time.Parse(time.RFC3339, b)
	if aErr == nil && bErr == nil {
		return aTime.Before(bTime)
	}
	return a < b
}

// idSlugMaxLen bounds the slug portion of a generated request id, the
// same bound cmd/factoryd's own ticketSlugMaxLen uses for a queue entry
// id.
const idSlugMaxLen = 40

// nonSlugChar matches any run of characters GenerateID's slug must
// collapse to a single "-".
var nonSlugChar = regexp.MustCompile(`[^a-z0-9]+`)

// GenerateID derives a request id from its source text: a lowercase,
// non-alphanumeric-collapsed-to-"-" slug, truncated to idSlugMaxLen, plus
// a "-<timestamp>" suffix -- ClaimID is what actually guarantees no
// collision.
func GenerateID(text string, now time.Time) string {
	slug := nonSlugChar.ReplaceAllString(strings.ToLower(text), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > idSlugMaxLen {
		slug = strings.Trim(slug[:idSlugMaxLen], "-")
	}
	if slug == "" {
		slug = "request"
	}
	return slug + "-" + now.Format("20060102-150405")
}

// ClaimID atomically CLAIMS a request directory for an id derived from
// base ("<data-dir>/requests/<id>", or "<id>-2", "-3", ... on collision)
// and returns that id, with the directory already created as a side
// effect -- the same os.Mkdir-based claim-not-check-then-act pattern
// cmd/factoryd's own uniqueTicketID uses, and for the same reason: it
// turns two concurrent submitters landing on the same candidate id into a
// guaranteed non-collision instead of a race. Callers must not
// MkdirAll/Mkdir the returned id's directory again -- it already exists.
func ClaimID(dataDir, base string) (string, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "requests"), 0o750); err != nil {
		return "", fmt.Errorf("create requests directory: %w", err)
	}
	for n := 1; ; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		err := os.Mkdir(Dir(dataDir, candidate), 0o750)
		if err == nil {
			return candidate, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", fmt.Errorf("claim request %q: %w", candidate, err)
	}
}

// Lock takes an exclusive, blocking flock on <request dir>/.lock for the
// caller's read-modify-write of that request's record, released by the
// returned function. Every load-mutate-save of a request.json that can
// run in a different process from another (an operator's `approve` or
// `reject` against worker's own reminder ticker) must hold it, or the
// later Save silently overwrites the earlier one's transition with a stale
// in-memory copy (found by adversarial review). Mirrors cmd/factoryd's
// lockQueueEntry.
func Lock(dataDir, id string) (func(), error) {
	dir := Dir(dataDir, id)
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock for request %q: %w", id, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock request %q: %w", id, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
