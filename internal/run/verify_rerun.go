package run

// The outcomes of a VerifyRerun.
const (
	// VerifyRerunPassed: the command exited zero when run again, so the
	// check's result is the rerun's.
	VerifyRerunPassed = "passed"
	// VerifyRerunFailed: the command failed again; the check's result is
	// the rerun's failure.
	VerifyRerunFailed = "failed"
	// VerifyRerunNotRun: the failure qualified for a rerun but none reached
	// an exit code (too little time was left, the launch failed, or its
	// worker stopped). Reason says why. The check's result is the first
	// run's failure.
	VerifyRerunNotRun = "not_run"
)

// VerifyRerunMaxFailing is the most failing tests a first run may report and
// still be run again: a flaky test fails alone, a broken change fails many.
const VerifyRerunMaxFailing = 3

// VerifyRerun records one rerun of the verify command, the full suite or the
// baseline verify after a failure that named only tests, none of which the
// build's change mentions (see internal/workflow's verify_rerun.go for the
// rule). It is set only when the failure qualified; a failure that did not
// is the check's result as it always was.
type VerifyRerun struct {
	// Outcome is VerifyRerunPassed, VerifyRerunFailed or VerifyRerunNotRun.
	Outcome string `json:"outcome"`
	// FailingTests is the tests the first run reported failing, as its log
	// printed them.
	FailingTests []string `json:"failing_tests"`
	// FirstExitCode, FirstLogPath and FirstLogSHA256 are the first run's.
	FirstExitCode  int    `json:"first_exit_code"`
	FirstLogPath   string `json:"first_log_path,omitempty"`
	FirstLogSHA256 string `json:"first_log_sha256,omitempty"`
	// ExitCode is the rerun's exit code; meaningful only when Outcome is not
	// VerifyRerunNotRun.
	ExitCode int `json:"exit_code"`
	// Reason says why Outcome is VerifyRerunNotRun, as one cleaned line.
	Reason string `json:"reason,omitempty"`
}

// PassedOnRerun reports whether r is a rerun that turned a failure into the
// check's pass.
func (r *VerifyRerun) PassedOnRerun() bool {
	return r != nil && r.Outcome == VerifyRerunPassed
}
