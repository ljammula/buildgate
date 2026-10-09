package run

import (
	"os"
	"path/filepath"
)

// LoadAll reads every run record under dataDir/runs. A directory whose
// record cannot be read, or names another run than its own directory, is
// left out: one damaged record must not hide what the others say. A data
// directory without runs holds none.
func LoadAll(dataDir string) ([]*Run, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Run
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, err := Load(dataDir, entry.Name())
		if err != nil || r.ID != entry.Name() {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
