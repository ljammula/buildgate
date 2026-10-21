package sandbox

import (
	"path/filepath"
	"strings"

	"buildgate/internal/progress"
	"buildgate/internal/run"
)

// sandboxStep is the run's "sandbox" start-up step in its progress feed: from
// the credential push and the sandbox's creation until the worker's command
// has begun to write its output. It is nested in whatever stage is open
// (progress.StepStart), and carries the launch's log as a path relative to
// the run's directory. A spec with no ProgressPath marks nothing.
type sandboxStep struct {
	path, log string
	ended     bool
}

func beginSandboxStep(s LaunchSpec) *sandboxStep {
	if s.ProgressPath == "" {
		return &sandboxStep{ended: true}
	}
	st := &sandboxStep{path: s.ProgressPath, log: relativeToRunDir(s)}
	_ = progress.MarkLog(st.path, "sandbox", progress.StepStart, "", "", "")
	return st
}

// end writes the step's end once; later calls change nothing, so a deferred
// failure end after a success end is harmless.
func (st *sandboxStep) end(outcome, detail string) {
	if st.ended {
		return
	}
	st.ended = true
	_ = progress.MarkLog(st.path, "sandbox", progress.StepEnd, outcome, detail, st.log)
}

// relativeToRunDir is s.LogPath relative to the run's directory, "" when the
// log is not inside it.
func relativeToRunDir(s LaunchSpec) string {
	if s.LogPath == "" {
		return ""
	}
	dir, err := filepath.Abs(run.Dir(s.DataDir, s.RunID))
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(s.LogPath)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return rel
}
