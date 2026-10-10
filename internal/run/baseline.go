package run

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// SetupFailedExitCode is the exit status of a step whose repository setup
// command (`.factory.yml` setup:) failed: the step's own script exits with it
// and leaves SetupFailedPrefix plus the command as a line of its log.
const SetupFailedExitCode = 95

// SetupFailedPrefix starts the log line that names the failed setup command.
const SetupFailedPrefix = "buildgate: setup failed: "

// SetupFailedCommand is the setup command the log names as failed, "" when
// it names none. A log is the step's output, so the line is accepted only
// from its own start.
func SetupFailedCommand(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if cmd, ok := strings.CutPrefix(line, SetupFailedPrefix); ok {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
}

// SetupDigest is the hex SHA-256 over the setup commands, each prefixed by its
// length so no two lists share a digest; "" for an empty list. A step records
// it (Attempt.SetupSHA256) to say which setup commands it ran.
func SetupDigest(setup []string) string {
	if len(setup) == 0 {
		return ""
	}
	h := sha256.New()
	for _, c := range setup {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(c)))
		h.Write(n[:])
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SetupNotRunMessage is the Command of the canonical_verify result recorded,
// with exit -1, for an accepted run whose verify attempt does not carry the
// digest of the repository's setup commands: a worker older than `setup:`
// judged the run. Nothing a build can do changes it, so it is the operator's.
const SetupNotRunMessage = "buildgate: this worker did not run the repository's setup commands (it predates them): restart the worker with `factoryd restart`"

// SetupNotRun reports whether g is the result recorded for a run whose verify
// did not run the repository's setup commands (SetupNotRunMessage).
func (g GateResult) SetupNotRun() bool {
	return g.Check == "canonical_verify" && !g.Passed && g.ExitCode == -1 && len(g.Command) == 1 && g.Command[0] == SetupNotRunMessage
}

// ReclaimNotCheckedPrefix starts the Command of the canonical_verify result
// recorded, with exit -1, for an accepted result that was reclaimed after its
// submitter died and could not be checked against the repository's
// .factory.yml as the run was dispatched with it (the commit and hash on the
// run record): which gates and setup commands the result had to run is then
// unknown. The reason follows the prefix. Nothing a build can do changes it,
// so it is the operator's.
const ReclaimNotCheckedPrefix = "buildgate: reclaimed result not applied: .factory.yml as dispatched could not be checked: "

// ReclaimNotChecked reports whether g is the result recorded for a reclaimed
// result that could not be checked (ReclaimNotCheckedPrefix).
func (g GateResult) ReclaimNotChecked() bool {
	return g.Check == "canonical_verify" && !g.Passed && g.ExitCode == -1 && len(g.Command) == 1 && strings.HasPrefix(g.Command[0], ReclaimNotCheckedPrefix)
}

// BaselineVerifyAttemptKind is Attempt.Kind of the verify command's run on
// the base commit, before the build's first round.
const BaselineVerifyAttemptKind = "baseline_verify"

// HaltReasonBaselineVerifyFailed is Run.HaltReasonCode's value when a run
// halted before its build because no build of the ticket could be accepted:
// the verify command failed on the base commit with a failure the ticket
// does not name, or the command (or a setup command) left paths outside the
// ticket's Allowed-Files that the repository does not ignore, which the
// factory would commit and diff_scope quarantine. No model call was made.
// The fix is in the verify command, the sandbox image, the repository's
// .gitignore or the ticket, never in the ticket's code.
const HaltReasonBaselineVerifyFailed = "baseline_verify_failed"

// BaselineVerify is the result of running the ticket's verify command on
// the base commit, in a fresh sandbox, before the build spent a model call
// (RunBaselineVerifyActivity). Recorded for every run; nil only on a run
// recorded before the check existed.
type BaselineVerify struct {
	// Command is the verify command as run.
	Command string `json:"command"`
	// BaseSHA is the commit the command ran on.
	BaseSHA  string `json:"base_sha,omitempty"`
	ExitCode int    `json:"exit_code"`
	Passed   bool   `json:"passed"`
	// FailingTests names the tests (or, for a failure with no test name, the
	// files) the command reported failing, as its log printed them, at most
	// BaselineVerifyMaxNamed; FailingCount is how many there were.
	FailingTests []string `json:"failing_tests,omitempty"`
	FailingCount int      `json:"failing_count,omitempty"`
	// Unnamed is the failing tests the ticket does not name, at most
	// BaselineVerifyMaxNamed; UnnamedCount is how many there were.
	Unnamed      []string `json:"unnamed,omitempty"`
	UnnamedCount int      `json:"unnamed_count,omitempty"`
	// NamedAs is the words the ticket names the failing tests with, at most
	// BaselineVerifyMaxNamed: text of the ticket, never of the log.
	NamedAs []string `json:"named_as,omitempty"`
	// NeedsCreated is set when no test failed by name and the command's
	// first error names a path the ticket declares and the base commit
	// does not have: that path. The failure is then the ticket's own work.
	NeedsCreated string `json:"needs_created,omitempty"`
	// FirstError is the log's first recognised error line, for a failure
	// that named no test (a missing program, a failed install).
	FirstError string `json:"first_error,omitempty"`
	// SetupFailed is the repository setup command that failed on the base
	// commit (exit SetupFailedExitCode): the verify command never ran, and
	// no change the build makes could pass it.
	SetupFailed string `json:"setup_failed,omitempty"`
	// Expected is true when the command failed and the failure is the work
	// the ticket asks for (it names every failing test, or NeedsCreated is
	// set), so the build ran and was told about it.
	Expected bool `json:"expected,omitempty"`
	// LeftOutOfScope is the paths the command left in the worktree that the
	// repository does not ignore and the ticket's Allowed-Files do not cover
	// (what diff_scope would flag), at most BaselineVerifyMaxNamed;
	// LeftOutOfScopeCount is how many there were. The factory commits what a
	// build leaves, so each would quarantine every build of the ticket.
	LeftOutOfScope      []string `json:"left_out_of_scope,omitempty"`
	LeftOutOfScopeCount int      `json:"left_out_of_scope_count,omitempty"`
	// InheritedFrom is the id of the run whose result this is: the halted
	// run whose worktree this run resumed, or the earlier run of the same
	// ticket whose commit this run continues from. Neither starts from the
	// base commit, so neither takes a baseline of its own.
	InheritedFrom string `json:"inherited_from,omitempty"`
	LogPath       string `json:"log_path,omitempty"`
	DurationMs    int64  `json:"duration_ms,omitempty"`
}

// BaselineVerifyMaxNamed bounds BaselineVerify.FailingTests and Unnamed.
const BaselineVerifyMaxNamed = 20

// Halts reports whether the result stops the run before its build.
func (b *BaselineVerify) Halts() bool {
	return b != nil && ((!b.Passed && !b.Expected) || b.LeftOutOfScopeCount > 0)
}

// leavesOutOfScope reports whether the command's only reason to halt is the
// paths it leaves: it passed, or failed as the ticket expects.
func (b *BaselineVerify) leavesOutOfScope() bool {
	return b.LeftOutOfScopeCount > 0 && (b.Passed || b.Expected)
}

// leftOutOfScopeText names the first left path and how many others there were.
func (b *BaselineVerify) leftOutOfScopeText() string {
	return "the command leaves " + namedAndMore(b.LeftOutOfScope, b.LeftOutOfScopeCount) + " outside the ticket's Allowed-Files"
}

// Summary is the result in one line, as status, watch, the inbox and the
// console print it: "passed", or "failed" with the failing test named.
func (b *BaselineVerify) Summary() string {
	if b == nil {
		return ""
	}
	if b.Passed {
		if b.leavesOutOfScope() {
			return "passed, but " + b.leftOutOfScopeText()
		}
		return "passed"
	}
	if b.leavesOutOfScope() {
		return b.expectedSummary() + "; " + b.leftOutOfScopeText()
	}
	return b.expectedSummary()
}

// expectedSummary is Summary without the paths the command leaves.
func (b *BaselineVerify) expectedSummary() string {
	if b.SetupFailed != "" {
		return "setup fails on the base commit: " + b.SetupFailed
	}
	if b.NeedsCreated != "" {
		return fmt.Sprintf("failed as the ticket expects: the command needs %s, which the ticket creates", b.NeedsCreated)
	}
	if b.FailingCount == 0 {
		s := fmt.Sprintf("failed: exit %d", b.ExitCode)
		if b.FirstError != "" {
			s += fmt.Sprintf("; first error in log: %q", b.FirstError)
		}
		return s
	}
	if b.Expected {
		return "failed as the ticket expects: " + namedAndMore(b.FailingTests, b.FailingCount)
	}
	s := "failed: " + namedAndMore(b.FailingTests, b.FailingCount)
	switch {
	case b.UnnamedCount == b.FailingCount && b.FailingCount == 1:
		s += "; the ticket does not name it"
	case b.UnnamedCount == b.FailingCount:
		s += "; the ticket names none of them"
	default:
		s += "; the ticket does not name " + namedAndMore(b.Unnamed, b.UnnamedCount)
	}
	return s
}

// namedAndMore is the first name and how many others there were.
func namedAndMore(names []string, count int) string {
	if len(names) == 0 {
		return fmt.Sprintf("%d tests", count)
	}
	if count <= 1 {
		return names[0]
	}
	return fmt.Sprintf("%s and %d more", names[0], count-1)
}

// HaltMessage is why a run halted on its baseline: the failure and what
// to do about it. RunBaselineVerifyActivity's error carries it.
func (b *BaselineVerify) HaltMessage() string {
	if b.SetupFailed != "" {
		return b.Summary() + ". " + b.HaltAdvice()
	}
	return fmt.Sprintf("baseline verify %s. %s", b.Summary(), b.HaltAdvice())
}

// HaltAdvice is HaltMessage without the failure, which the run's triage
// sentence already names: Run.HaltError of a run halted on its baseline.
func (b *BaselineVerify) HaltAdvice() string {
	if b.leavesOutOfScope() {
		return "No model call was made: every build of this ticket would be quarantined by diff_scope, because the factory commits what the command leaves. " +
			"Ignore those paths in the repository's .gitignore, or make the command remove them."
	}
	if b.SetupFailed != "" {
		return "No model call was made: a setup: command of the repository's .factory.yml fails on the untouched repository, so no build could pass verification. " +
			"Fix the command or the sandbox image it runs in."
	}
	base := b.BaseSHA
	if len(base) > 12 {
		base = base[:12]
	}
	where := ""
	if base != "" {
		where = " (" + base + ")"
	}
	return "No model call was made: the verify command fails on the untouched repository" + where + ", so no build could pass it. " +
		"Fix the verify command or the sandbox image it runs in; if this ticket is meant to make those tests pass, name each of them in the ticket."
}

// BaselineVerifyFileName is the baseline record in a run's directory.
// RunBaselineVerifyActivity writes it once the command has run to an exit
// code, before it returns, pass or fail, so
// the result reaches the run record on every path a run ends by, a halt
// included: Temporal hands a failed workflow's caller an error and no result.
const BaselineVerifyFileName = "baseline_verify.json"

// SaveBaselineVerify writes b as dir's baseline record, atomically.
func SaveBaselineVerify(dir string, b *BaselineVerify) error {
	content, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baseline verify record: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	path := filepath.Join(dir, BaselineVerifyFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return fmt.Errorf("write baseline verify record: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadBaselineVerify reads dir's baseline record; (nil, nil) when the run
// has none.
func LoadBaselineVerify(dir string) (*BaselineVerify, error) {
	content, err := os.ReadFile(filepath.Join(dir, BaselineVerifyFileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b BaselineVerify
	if err := json.Unmarshal(content, &b); err != nil {
		return nil, fmt.Errorf("parse baseline verify record: %w", err)
	}
	return &b, nil
}

// AttachBaselineVerify sets r.BaselineVerify from the run's baseline record
// once it exists. Called by Persist, the one funnel every run-record write
// goes through, and by internal/triage, which words a halt before that
// save. An unreadable record is logged and leaves the field unset: it never
// fails a save.
func (r *Run) AttachBaselineVerify(dataDir string) {
	if r.BaselineVerify != nil || r.ID == "" {
		return
	}
	b, err := LoadBaselineVerify(Dir(dataDir, r.ID))
	if err != nil {
		log.Printf("run %s: warning: could not read its baseline verify record: %v", r.ID, err)
		return
	}
	r.BaselineVerify = b
}
