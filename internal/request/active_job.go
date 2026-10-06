package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ActiveJobPath is the file recording id's running drafting job.
func ActiveJobPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), "active_job.json")
}

// WriteActiveJob records job as id's running drafting job (atomic
// replace).
func WriteActiveJob(dataDir, id string, job ActiveJob) error {
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal active job: %w", err)
	}
	path := ActiveJobPath(dataDir, id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write active job: %w", err)
	}
	return os.Rename(tmp, path)
}

// ClearActiveJob removes id's running-job record; a missing one is fine.
func ClearActiveJob(dataDir, id string) error {
	if err := os.Remove(ActiveJobPath(dataDir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear active job: %w", err)
	}
	return nil
}

// LoadActiveJob returns id's running-job record, or nil when there is
// none or it can't be read: it only feeds a display, never a decision.
func LoadActiveJob(dataDir, id string) *ActiveJob {
	b, err := os.ReadFile(ActiveJobPath(dataDir, id))
	if err != nil {
		return nil
	}
	var job ActiveJob
	if json.Unmarshal(b, &job) != nil {
		return nil
	}
	return &job
}
