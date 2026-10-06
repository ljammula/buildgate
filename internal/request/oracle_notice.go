package request

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// MissingRunCommandProblem is the one actionable message for a request-level
// oracle/ directory that holds files but no RUN_COMMAND.txt: which file to
// write, what its command must do, and a concrete command to start from --
// the drafting pass's proposal when there is one, otherwise the general
// shape. The same text is shown up front (watch, quickstart, reminders, the
// oracle API listing and console) and in approval's refusal, so the operator
// never learns of the missing file only when approve refuses.
func MissingRunCommandProblem(dataDir, id string) string {
	suggestion := "shape: for Go `go test ./.oracle/...`, for Python `python -m pytest .oracle/`"
	if r, err := Load(dataDir, id); err == nil && r.OracleDraft != nil && r.OracleDraft.ProposedCommand != "" {
		suggestion = fmt.Sprintf("suggested for this oracle: %s", r.OracleDraft.ProposedCommand)
	}
	return fmt.Sprintf("no %s -- write %s/%s (one line: the exact command that runs the oracle files; it must name %q as a command word, because a wildcard like `go test ./...` skips that hidden mount) or remove the directory to skip the oracle, before approving. %s",
		TicketOracleRunCommandFilename, RequestOracleDirName, TicketOracleRunCommandFilename, TicketOracleMountPath, suggestion)
}

// GeneratedRunCommandNotice is the one actionable message for a
// request-level oracle/RUN_COMMAND.txt the drafting job wrote itself (see
// OracleDraft.GeneratedRunCommandSHA256), when the on-disk file still is that
// generated content: nobody has read or edited it yet. Unlike a missing
// RUN_COMMAND.txt, a generated one satisfies approval's hasRunCommand check
// with no refusal to force a human to look, so this notice is the only
// forcing function left -- found in review (2026-09-21): a host-generated
// command is built from the model's own drafted file and target-directory
// names, and CLI approval is unconditional by design (it trusts the operator
// read the oracle directory first), so this is what makes that trust
// informed instead of a rubber stamp.
func GeneratedRunCommandNotice(r *Request, command string) string {
	if r == nil || r.OracleDraft == nil || r.OracleDraft.GeneratedRunCommandSHA256 == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(command))
	if hex.EncodeToString(sum[:]) != r.OracleDraft.GeneratedRunCommandSHA256 {
		return ""
	}
	return fmt.Sprintf("%s/%s was written by the drafter, not you: %q. Read it before approving -- it decides which directories are overlaid and run. Edit it (or replace it) if it is wrong; your edit is never overwritten.",
		RequestOracleDirName, TicketOracleRunCommandFilename, command)
}

// OracleReviewChecklistHint is a standing one-line reminder shown whenever a
// request arrives at oracle_review, alongside OracleReviewNotice's own
// situational action item -- prompting the operator to check for either of
// two defect classes that have actually been seen live in drafted oracles:
// an errors.Is assertion against a second, freshly-constructed error (never == to
// another wrapped error without a custom Is method) that can force an
// otherwise-unrelated file's edit later, and a test reaching outside its
// own declared target file/package. Neither is caught by the host's
// compile self-check, which only proves the file compiles, not that its
// assertions are sound.
const OracleReviewChecklistHint = "Before approving: check the oracle's errors.Is/sentinel assertions (never against a second, freshly-constructed error) and whether it reaches beyond its own target file/package -- either can force an otherwise-correct ticket outside its declared scope later, and a diff_scope quarantine naming a file only touched to satisfy this oracle means the oracle may be wrong, not the build."

// OracleReviewNotice returns MissingRunCommandProblem when r sits at
// oracle_review with a populated oracle/ directory that lacks RUN_COMMAND.txt,
// GeneratedRunCommandNotice when RUN_COMMAND.txt is present but still exactly
// what the drafting job generated, else "". CLI surfaces call it so the gap
// is narrated on arrival at the review, not on approval's refusal.
func OracleReviewNotice(dataDir string, r *Request) string {
	if r == nil || r.State != StateOracleReview {
		return ""
	}
	dir := oracleDirPath(dataDir, r.ID)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	command, err := os.ReadFile(filepath.Join(dir, TicketOracleRunCommandFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return MissingRunCommandProblem(dataDir, r.ID)
		}
		return ""
	}
	return GeneratedRunCommandNotice(r, string(command))
}
