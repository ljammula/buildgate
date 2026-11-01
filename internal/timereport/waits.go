// Package timereport answers "where did a request's time go?": per ticket and
// round, the minutes each stage of a run took and the minutes it waited for
// the repository lock, the request's RepositoryBusy waits, and how much of the
// request's span no run or wait covers. It reads run records, progress feeds
// and the request's wait log; the only thing it writes is that wait log
// (RecordWait). Nothing is gated.
package timereport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/request"
)

// WaitsFileName is the append-only wait log in a request's directory
// (request.Dir): one JSON line when a RepositoryBusy wait starts and one when
// it ends, never one per retry. A wait with no end line is open.
const WaitsFileName = "repository_waits.jsonl"

// Wait log events.
const (
	WaitStart = "start"
	WaitEnd   = "end"
)

// waitLine is one line of the wait log.
type waitLine struct {
	At    string `json:"at"`
	Event string `json:"event"`
}

// WaitsPath is where request id's wait log lives.
func WaitsPath(dataDir, id string) string {
	return filepath.Join(request.Dir(dataDir, id), WaitsFileName)
}

// RecordWait appends one line to request id's wait log: event is WaitStart
// when the request's step first found the repository held by another build,
// WaitEnd when a later step found it free (or the request left building).
func RecordWait(dataDir, id, event string, at time.Time) error {
	if event != WaitStart && event != WaitEnd {
		return fmt.Errorf("repository wait event %q: want %q or %q", event, WaitStart, WaitEnd)
	}
	line, err := json.Marshal(waitLine{At: at.UTC().Format(time.RFC3339Nano), Event: event})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(WaitsPath(dataDir, id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open repository wait log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write repository wait log: %w", err)
	}
	return f.Close()
}

// readWaits pairs request id's wait log into waits. A start with no end is
// open; so is a start followed by another start (the worker restarted while
// it waited, so nothing recorded the end). An end with no start is ignored. A
// missing log is no waits.
func readWaits(dataDir, id string) ([]Wait, error) {
	f, err := os.Open(WaitsPath(dataDir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var waits []Wait
	open := -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l waitLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, l.At)
		if err != nil {
			continue
		}
		switch l.Event {
		case WaitStart:
			waits = append(waits, Wait{Kind: KindRepositoryBusy, Start: l.At, Open: true, start: at})
			open = len(waits) - 1
		case WaitEnd:
			if open >= 0 {
				w := &waits[open]
				w.End, w.Open = l.At, false
				w.Seconds = max(at.Sub(w.start), 0).Seconds()
				w.end = at
				open = -1
			}
		}
	}
	return waits, sc.Err()
}

// WaitIsOpen reports whether request id's wait log ends in a start: a wait a
// worker began and did not end (it stopped, or lost its in-memory state, while
// the request waited).
func WaitIsOpen(dataDir, id string) bool {
	waits, err := readWaits(dataDir, id)
	return err == nil && len(waits) > 0 && waits[len(waits)-1].Open
}
