package sandbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"buildgate/internal/run"
	"buildgate/internal/stats"
)

// workerMemoryFileName is the run's record of its workers' memory readings,
// one line per launch. It is kept apart from the sandbox record, which the
// orphan sweep and the run's spend read and which refuses an unreadable
// line: a reading written after a command must never make those fail.
const workerMemoryFileName = "sandbox_memory.jsonl"

// workerMemoryLine is one line of the memory record. StallMicros is absent
// when the kernel reported no memory stall information.
type workerMemoryLine struct {
	Name        string  `json:"name"`
	PeakBytes   uint64  `json:"peak_bytes"`
	StallMicros *uint64 `json:"stall_us,omitempty"`
}

// RecordWorkerMemory appends one launch's reading to the run's memory record.
func RecordWorkerMemory(dataDir, runID, name string, mem WorkerMemory) error {
	if dataDir == "" || runID == "" || !sandboxNamePattern.MatchString(name) {
		return fmt.Errorf("record worker memory: invalid run %q or sandbox %q", runID, name)
	}
	rec := workerMemoryLine{Name: name, PeakBytes: mem.PeakBytes}
	if mem.StallKnown {
		stall := mem.StallMicros
		rec.StallMicros = &stall
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(run.Dir(dataDir, runID), workerMemoryFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("record worker memory: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("record worker memory: %w", err)
	}
	return f.Close()
}

// RecordedWorkerMemory returns the run's memory readings in the order
// written. A line that does not parse is skipped, as is one with no peak: the
// record only sizes the machine.
func RecordedWorkerMemory(dataDir, runID string) ([]WorkerMemory, error) {
	f, err := os.Open(filepath.Join(run.Dir(dataDir, runID), workerMemoryFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []WorkerMemory
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec workerMemoryLine
		if json.Unmarshal(scanner.Bytes(), &rec) != nil || rec.PeakBytes == 0 {
			continue
		}
		mem := WorkerMemory{PeakBytes: rec.PeakBytes}
		if rec.StallMicros != nil {
			mem.StallMicros, mem.StallKnown = *rec.StallMicros, true
		}
		out = append(out, mem)
	}
	return out, scanner.Err()
}

// WorkerMemoryOf returns stats.Options.SandboxMemory over the memory records
// of dataDir's runs; a record that cannot be read gives none.
func WorkerMemoryOf(dataDir string) func(runID string) []stats.SandboxMemory {
	return func(runID string) []stats.SandboxMemory {
		readings, err := RecordedWorkerMemory(dataDir, runID)
		if err != nil {
			return nil
		}
		out := make([]stats.SandboxMemory, 0, len(readings))
		for _, m := range readings {
			out = append(out, stats.SandboxMemory{PeakBytes: m.PeakBytes, StallMicros: m.StallMicros, StallKnown: m.StallKnown})
		}
		return out
	}
}
