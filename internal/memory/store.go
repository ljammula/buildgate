package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/sanitize"
)

// ErrStore wraps every refusal of the store: a bad project name, a file that
// is too large or from another schema, a bound that cannot be met.
var ErrStore = errors.New("memory: store refused")

const (
	// StoreSchemaVersion is the only state.json schema this build reads.
	StoreSchemaVersion = 1
	// MaxStateBytes bounds state.json, on read and on write.
	MaxStateBytes = 1 << 20
	// MaxLessons bounds the lessons one project keeps.
	MaxLessons = 200
	// MaxCountedRuns bounds the run-id cursor, newest last.
	MaxCountedRuns = 2000

	maxProjectBytes = 128
	stateFile       = "state.json"
	lockFile        = ".lock"
	offFile         = "off"
)

// StoreState is the content of state.json.
type StoreState struct {
	SchemaVersion int      `json:"schema_version"`
	Lessons       []Lesson `json:"lessons"`
	// CountedRuns lists the run ids whose notes were already collected,
	// newest last.
	CountedRuns []string `json:"counted_runs,omitempty"`
}

// StoreKey names one repository's store directory: the lower-cased project
// name, then the first 12 hex characters of the SHA-256 of the repository's
// cleaned absolute root path. Two repositories that share a base name, or
// whose names differ only by case, get different stores, stop markers and
// proposals. Every caller of Open, OpenReadOnly, ProposalPath and ChangesPath
// passes this key where the store's directory name is wanted.
func StoreKey(project, repoRoot string) string {
	return strings.ToLower(project) + "-" + HashHex([]byte(filepath.Clean(repoRoot)))[:12]
}

// Store is one repository's durable memory under <data-dir>/memory/<key>,
// where key is its StoreKey.
type Store struct {
	dir string
}

// storeMu serializes Updates within a process; the flock serializes them
// across processes (two descriptors on one file inside a process are not
// reliably exclusive on every platform, so neither lock replaces the other).
var storeMu sync.Mutex

// ValidProject reports whether project is a single safe path component.
func ValidProject(project string) error {
	switch {
	case project == "":
		return fmt.Errorf("%w: project name is empty", ErrStore)
	case len(project) > maxProjectBytes:
		return fmt.Errorf("%w: project name is longer than %d bytes", ErrStore, maxProjectBytes)
	case strings.HasPrefix(project, "."):
		return fmt.Errorf("%w: project name starts with a dot", ErrStore)
	case strings.ContainsAny(project, "/\\\x00") || project != sanitize.Line(project):
		return fmt.Errorf("%w: project name %q is not a single path component", ErrStore, clip(project))
	}
	return nil
}

// Open creates the store's directories (mode 0700) and returns it. project is
// the repository's StoreKey.
func Open(dataDir, project string) (*Store, error) {
	if err := ValidProject(project); err != nil {
		return nil, err
	}
	dir := filepath.Join(dataDir, "memory", project)
	for _, d := range []string{dir, filepath.Join(dir, "proposals")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("memory: create %s: %w", d, err)
		}
	}
	return &Store{dir: dir}, nil
}

// OpenReadOnly returns the project's store without creating anything: Load
// and Off work on a store that was never written (empty, not stopped).
func OpenReadOnly(dataDir, project string) (*Store, error) {
	if err := ValidProject(project); err != nil {
		return nil, err
	}
	return &Store{dir: filepath.Join(dataDir, "memory", project)}, nil
}

// Load reads state.json. A missing file is an empty state.
func (s *Store) Load() (StoreState, error) {
	return s.read()
}

func (s *Store) read() (StoreState, error) {
	empty := StoreState{SchemaVersion: StoreSchemaVersion}
	path := filepath.Join(s.dir, stateFile)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("memory: stat state: %w", err)
	}
	if info.Size() > MaxStateBytes {
		return empty, fmt.Errorf("%w: state.json is %d bytes, over the %d byte limit", ErrStore, info.Size(), MaxStateBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty, fmt.Errorf("memory: read state: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var st StoreState
	if err := dec.Decode(&st); err != nil {
		return empty, fmt.Errorf("%w: state.json: %v", ErrStore, err)
	}
	if st.SchemaVersion != StoreSchemaVersion {
		return empty, fmt.Errorf("%w: state.json schema_version %d, this build reads %d", ErrStore, st.SchemaVersion, StoreSchemaVersion)
	}
	return st, nil
}

// lock takes the in-process mutex and an exclusive flock on .lock.
func (s *Store) lock() (func(), error) {
	storeMu.Lock()
	f, err := os.OpenFile(filepath.Join(s.dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		storeMu.Unlock()
		return nil, fmt.Errorf("memory: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		storeMu.Unlock()
		return nil, fmt.Errorf("memory: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		storeMu.Unlock()
	}, nil
}

// Update loads the state under an exclusive lock, calls fn, enforces the
// bounds and writes the result atomically. An error from fn or from a bound
// leaves the file as it was.
func (s *Store) Update(fn func(*StoreState) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	data, err := enforceBounds(&st)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, stateFile), data)
}

func lastMoveAt(l Lesson) string {
	if n := len(l.History); n > 0 {
		return l.History[n-1].At
	}
	return ""
}

// pruneOldest removes up to over lessons in the given state, oldest last
// transition first, and returns the remaining lessons.
func pruneOldest(lessons []Lesson, state State, over int) []Lesson {
	var idx []int
	for i, l := range lessons {
		if l.State == state {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return lastMoveAt(lessons[idx[a]]) < lastMoveAt(lessons[idx[b]]) })
	if over < len(idx) {
		idx = idx[:over]
	}
	drop := map[int]bool{}
	for _, i := range idx {
		drop[i] = true
	}
	out := make([]Lesson, 0, len(lessons)-len(drop))
	for i, l := range lessons {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// enforceBounds normalizes st and returns its encoding, or refuses.
func enforceBounds(st *StoreState) ([]byte, error) {
	st.SchemaVersion = StoreSchemaVersion
	if over := len(st.Lessons) - MaxLessons; over > 0 {
		st.Lessons = pruneOldest(st.Lessons, StateDropped, over)
	}
	if len(st.Lessons) > MaxLessons {
		return nil, fmt.Errorf("%w: %d lessons, over the %d lesson limit with none dropped to prune", ErrStore, len(st.Lessons), MaxLessons)
	}
	for _, l := range st.Lessons {
		if len(l.Runs) > MaxLessonRuns {
			return nil, fmt.Errorf("%w: lesson %s lists %d runs, over the %d limit", ErrStore, clip(l.ID), len(l.Runs), MaxLessonRuns)
		}
	}
	if n := len(st.CountedRuns); n > MaxCountedRuns {
		st.CountedRuns = append([]string(nil), st.CountedRuns[n-MaxCountedRuns:]...)
	}
	if st.Lessons == nil {
		st.Lessons = []Lesson{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("memory: encode state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxStateBytes {
		return nil, fmt.Errorf("%w: state would be %d bytes, over the %d byte limit", ErrStore, len(data), MaxStateBytes)
	}
	return data, nil
}

// writeAtomic writes path through a temp file in the same directory, fsync
// and rename, mode 0600.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return fmt.Errorf("memory: chmod temp: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("memory: write temp: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("memory: sync temp: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("memory: close temp: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("memory: rename state: %w", err)
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Off reports whether the stop marker exists.
func (s *Store) Off() (bool, error) {
	_, err := os.Lstat(filepath.Join(s.dir, offFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory: stat off marker: %w", err)
	}
	return true, nil
}

// SetOff writes the stop marker: one cleaned "who: reason" line, then the time.
func (s *Store) SetOff(by, reason string) error {
	line := sanitize.Line(by) + ": " + sanitize.Line(reason)
	if len(line) > maxMoveTextLen {
		line = strings.ToValidUTF8(line[:maxMoveTextLen], "")
	}
	body := line + "\n" + time.Now().UTC().Format(time.RFC3339) + "\n"
	return writeAtomic(filepath.Join(s.dir, offFile), []byte(body))
}

// ClearOff removes the stop marker; none present is not an error.
func (s *Store) ClearOff() error {
	err := os.Remove(filepath.Join(s.dir, offFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory: remove off marker: %w", err)
	}
	return nil
}

// Reconcile brings the store in line with the section at HEAD. A lesson whose
// exact line the section holds is removed, whatever its state: the line is in
// force and the file is the memory. A proposed lesson whose request is known
// and no longer active, with the line absent, goes back to candidate.
// requestState reports whether a request is still active and whether it is
// known at all. It does no I/O.
func Reconcile(st *StoreState, sectionLinesAtHead []string, requestState func(requestID string) (active bool, known bool), now string) {
	kept := make([]Lesson, 0, len(st.Lessons))
	for _, l := range st.Lessons {
		if contains(sectionLinesAtHead, l.Line) {
			continue
		}
		if l.State == StateProposed {
			if active, known := requestState(l.RequestID); known && !active {
				_ = l.Move(StateCandidate, now, "reconcile", "the request ended without the line")
				l.RequestID = ""
			}
		}
		kept = append(kept, l)
	}
	st.Lessons = kept
}
