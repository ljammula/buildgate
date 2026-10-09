package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Change is one line a memory request adds or removes. Source and Runs say
// who wrote an added line down: the operator, or the runs whose build agent
// noted it (ids only).
type Change struct {
	Remove bool     `json:"remove,omitempty"`
	Line   string   `json:"line"`
	Source string   `json:"source,omitempty"`
	Runs   []string `json:"runs,omitempty"`
}

// maxChangesBytes bounds a changes file: a handful of lines and run ids.
const maxChangesBytes = 64 << 10

// ChangesPath is where a memory request's list of changes is kept, beside
// its proposal: <dataDir>/memory/<storeKey>/proposals/<requestID>.changes.json.
// The release check never reads it; the pull request body does.
func ChangesPath(dataDir, storeKey, requestID string) (string, error) {
	proposal, err := ProposalPath(dataDir, storeKey, requestID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(proposal), requestID+".changes.json"), nil
}

// SaveChanges writes the list atomically, mode 0600.
func SaveChanges(path string, changes []Change) error {
	if changes == nil {
		changes = []Change{}
	}
	data, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("memory: encode changes: %w", err)
	}
	if len(data) > maxChangesBytes {
		return fmt.Errorf("memory: changes are larger than %d bytes", maxChangesBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("memory: create proposal directory: %w", err)
	}
	return writeAtomic(path, data)
}

// LoadChanges reads the list; a missing file is (nil, nil).
func LoadChanges(path string) ([]Change, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: stat changes: %w", err)
	}
	if info.Size() > maxChangesBytes {
		return nil, fmt.Errorf("memory: changes are larger than %d bytes", maxChangesBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("memory: read changes: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var changes []Change
	if err := dec.Decode(&changes); err != nil {
		return nil, fmt.Errorf("memory: decode changes: %w", err)
	}
	return changes, nil
}
