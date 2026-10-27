package request

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WakePendingFileName is the marker, beside a request's record, that its last
// save was not announced to the worker: the wake after a new request or a
// decision failed or had no worker to reach. A running worker retries every
// marked request and clears the marker once the wake succeeded; a marker never
// causes anything to start or be signalled except that retry.
const WakePendingFileName = "wake-pending"

// WakePending is one marker: the request and the token it was written with, so
// a clear never removes a marker a later failed wake wrote meanwhile.
type WakePending struct {
	ID    string
	Token string
}

func wakePendingPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), WakePendingFileName)
}

// MarkWakePending records that requestID's wake failed. Marking again replaces
// the marker with a new token. The write is a temp file and a rename.
func MarkWakePending(dataDir, id string) error {
	dir := Dir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create request dir: %w", err)
	}
	path := wakePendingPath(dataDir, id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write wake marker: %w", err)
	}
	return os.Rename(tmp, path)
}

// ListWakePending returns every marker under dataDir, ordered by request id.
func ListWakePending(dataDir string) ([]WakePending, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "requests"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []WakePending
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(wakePendingPath(dataDir, e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("request %q: %w", e.Name(), err)
		}
		out = append(out, WakePending{ID: e.Name(), Token: strings.TrimSpace(string(b))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ClearWakePending removes the marker m names, unless it was replaced since
// it was listed. A missing marker is not an error.
func ClearWakePending(dataDir string, m WakePending) error {
	path := wakePendingPath(dataDir, m.ID)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != m.Token {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
