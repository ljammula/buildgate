package progress

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// The statuses of a StartupStep.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// StartupStep is one step of a run's start-up, from the image it runs in to
// the agent's first round, derived from the progress feed. The feed is the
// only source: nothing about a step is stored in run.json.
type StartupStep struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// StartedAt and EndedAt are the feed's own timestamps.
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Log is the step's log, relative to the run's directory, when the
	// factory recorded one.
	Log string `json:"log,omitempty"`
	// Wait is the lock or queue wait the run is in, on the step that waits
	// for it: the running step, else the first one not yet started.
	Wait string `json:"wait,omitempty"`
}

// The start-up steps, in the order an operator reads them.
const (
	StepImage     = "image"
	StepWorkspace = "workspace"
	StepInstall   = "install"
	StepSandbox   = "sandbox"
	StepVerify    = "verify"
	StepAgent     = "agent"
)

var startupOrder = []string{StepImage, StepWorkspace, StepInstall, StepSandbox, StepVerify, StepAgent}

// startupStepOf maps a factory stage to the start-up step it begins or ends.
func startupStepOf(stage string) (string, bool) {
	switch stage {
	case "image":
		return StepImage, true
	case "prepare_workspace":
		return StepWorkspace, true
	case "install":
		return StepInstall, true
	case "sandbox":
		return StepSandbox, true
	case "baseline_verify":
		return StepVerify, true
	}
	return "", false
}

// StartupSteps reads path's feed from its start, up to the agent's first
// round, and returns the six start-up steps (see StartupStepsFrom). A missing
// feed is not an error: every step is pending.
func StartupSteps(path string) ([]StartupStep, error) {
	b := newStartupBuilder()
	f, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) != nil {
				continue
			}
			if b.add(e) {
				break
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	return b.steps(), nil
}

// StartupStepsFrom is StartupSteps over events already read, in order.
//
// A step is started and ended by its factory lines: a top-level stage
// (image, prepare_workspace, baseline_verify) by start and end, a step nested
// in another stage (install, sandbox) by step_start and step_end. The first
// occurrence of a step wins. The agent step is done at the worker's first
// round start, and failed when the run finished first. A step with no line
// that a later step follows is skipped; otherwise it is pending. A line from
// the worker never starts, ends or names a step, and never supplies a log.
func StartupStepsFrom(events []Event) []StartupStep {
	b := newStartupBuilder()
	for _, e := range events {
		if b.add(e) {
			break
		}
	}
	return b.steps()
}

type startupBuilder struct {
	byName map[string]*StartupStep
	// started and ended record which steps a line began and ended, apart from
	// the status they come out with.
	started, ended map[string]bool
	waiting        string
	finished       bool
}

func newStartupBuilder() *startupBuilder {
	return &startupBuilder{byName: map[string]*StartupStep{}, started: map[string]bool{}, ended: map[string]bool{}}
}

// add folds one event in and reports whether the feed need be read no
// further: the agent started, or the run finished.
func (b *startupBuilder) add(e Event) (done bool) {
	switch e.Source {
	case "worker":
		if e.Stage == "round" && e.Event == "start" {
			b.step(StepAgent).StartedAt, b.step(StepAgent).EndedAt = e.Ts, e.Ts
			b.step(StepAgent).Detail = fmt.Sprintf("round %d", max(e.Round, 1))
			b.started[StepAgent], b.ended[StepAgent] = true, true
			return true
		}
	case "factory":
		return b.addFactory(e)
	}
	return false
}

func (b *startupBuilder) addFactory(e Event) bool {
	if e.Stage == "finished" && e.Event == "end" {
		b.finished = true
		return true
	}
	b.trackWait(e)
	name, ok := startupStepOf(e.Stage)
	if !ok {
		return false
	}
	switch e.Event {
	case "start", StepStart:
		if b.started[name] {
			return false
		}
		s := b.step(name)
		s.StartedAt, s.Detail, s.Log = e.Ts, e.Detail, e.Log
		b.started[name] = true
	case "end", StepEnd:
		if !b.started[name] || b.ended[name] {
			return false
		}
		s := b.step(name)
		s.EndedAt = e.Ts
		if e.Detail != "" {
			s.Detail = e.Detail
		}
		if e.Log != "" {
			s.Log = e.Log
		}
		if e.Outcome == "fail" {
			s.Status = StepFailed
		}
		b.ended[name] = true
	}
	return false
}

// trackWait follows the lock waits the way Summarize does.
func (b *startupBuilder) trackWait(e Event) {
	switch e.Event {
	case "waiting":
		if isLockStage(e.Stage) {
			b.waiting = e.Detail
		}
	case "note":
		if e.Stage == "queued" {
			b.waiting = e.Detail
		}
	case "acquired":
		if isLockStage(e.Stage) {
			b.waiting = ""
		}
	case "start":
		b.waiting = ""
	}
}

func (b *startupBuilder) step(name string) *StartupStep {
	s, ok := b.byName[name]
	if !ok {
		s = &StartupStep{Name: name}
		b.byName[name] = s
	}
	return s
}

// steps settles each step's status and places the current wait.
func (b *startupBuilder) steps() []StartupStep {
	out := make([]StartupStep, len(startupOrder))
	laterStarted := false
	for i := len(startupOrder) - 1; i >= 0; i-- {
		name := startupOrder[i]
		s := *b.step(name)
		switch {
		case b.ended[name] && s.Status == StepFailed:
		case b.ended[name]:
			s.Status = StepDone
		case b.started[name]:
			s.Status = StepRunning
		case laterStarted:
			s.Status = StepSkipped
		case name == StepAgent && b.finished:
			s.Status, s.Detail = StepFailed, "the run finished before the agent started"
		default:
			s.Status = StepPending
		}
		laterStarted = laterStarted || b.started[name]
		out[i] = s
	}
	if b.waiting != "" {
		placeWait(out, b.waiting)
	}
	return out
}

// placeWait puts the wait on the running step, else the first pending one.
func placeWait(steps []StartupStep, wait string) {
	for _, status := range []string{StepRunning, StepPending} {
		for i := range steps {
			if steps[i].Status == status {
				steps[i].Wait = wait
				return
			}
		}
	}
}

// StartupFooter is the one-line start-up position for `factoryd watch`'s
// footer, "start-up 3/6: install (running 42s)", or "" once the agent has
// started or the run finished before it.
func StartupFooter(steps []StartupStep, now time.Time) string {
	if len(steps) == 0 || steps[len(steps)-1].Status != StepPending {
		return ""
	}
	for i, s := range steps {
		if s.Status != StepRunning && s.Status != StepPending {
			continue
		}
		text := fmt.Sprintf("start-up %d/%d: %s", i+1, len(steps), s.Name)
		if s.Status == StepRunning {
			if at, err := time.Parse(tsLayout, s.StartedAt); err == nil {
				return fmt.Sprintf("%s (running %s)", text, max(now.Sub(at), 0).Round(time.Second))
			}
			return text + " (running)"
		}
		return text + " (pending)"
	}
	return ""
}
