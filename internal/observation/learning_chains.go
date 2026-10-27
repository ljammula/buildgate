package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"buildgate/internal/policy"
	"buildgate/internal/run"
)

const learningChainVersion = "fixed-check-chain-v1"

const maxLearningChains = 200

// LearningChain is a prose-free, read-only projection of one quarantined run
// and its directly linked accepted successor. It is audit data only; it is
// not consumed by a model or used to create repository memory.
type LearningChain struct {
	ID                          string   `json:"id"`
	RequestID                   string   `json:"request_id"`
	Ticket                      string   `json:"ticket"`
	FailedRunID                 string   `json:"failed_run_id"`
	AcceptedRunID               string   `json:"accepted_run_id"`
	FailedOutcome               string   `json:"failed_outcome"`
	AcceptedOutcome             string   `json:"accepted_outcome"`
	FailedAt                    string   `json:"failed_at"`
	AcceptedAt                  string   `json:"accepted_at"`
	FailedCheckTypes            []string `json:"failed_check_types"`
	FailedConfigCommitSHA       string   `json:"failed_config_commit_sha"`
	AcceptedConfigCommitSHA     string   `json:"accepted_config_commit_sha"`
	FailedConfigContentSHA256   string   `json:"failed_config_content_sha256"`
	AcceptedConfigContentSHA256 string   `json:"accepted_config_content_sha256"`
	Complete                    bool     `json:"complete"`
	Eligible                    bool     `json:"eligible"`
	IneligibleReason            string   `json:"ineligible_reason,omitempty"`
}

// learningCheckTypes is the closed set that can enter this projection. It
// follows policy's fixed gate registry and includes the two optional fixed
// review gates that are intentionally absent from AllGateChecks. Repository-
// defined gate names are never admitted.
var learningCheckTypes = func() map[string]bool {
	types := make(map[string]bool, len(policy.AllGateChecks)+2)
	for _, check := range policy.AllGateChecks {
		types[check] = true
	}
	types["code_review"] = true
	types["spec_conformity"] = true
	return types
}()

// learningChains emits only direct EarlierAttemptOf links. It never uses
// timestamps, branch names, project names, or textual similarity to join
// records. Unknown or inconsistent evidence stays visible as incomplete or
// ineligible metadata and cannot be used as a lesson.
func learningChains(runs []*run.Run) []LearningChain {
	byID, duplicates := indexLearningRuns(runs)
	var out []LearningChain
	processedAccepted := make(map[string]bool)
	for _, accepted := range runs {
		if accepted == nil || accepted.State != run.StateAccepted || accepted.EarlierAttemptOf == "" {
			continue
		}
		if processedAccepted[accepted.ID] {
			continue
		}
		processedAccepted[accepted.ID] = true
		failed := byID[accepted.EarlierAttemptOf]
		if failed == nil {
			out = append(out, incompleteLearningChain(accepted, "missing_predecessor"))
			continue
		}
		if failed.ID == accepted.ID {
			out = append(out, incompleteLearningChain(accepted, "invalid_run_id"))
			continue
		}
		if failed.State != run.StateQuarantined {
			out = append(out, incompleteLearningChain(accepted, "predecessor_not_quarantined"))
			continue
		}
		out = append(out, projectLearningChain(failed, accepted, duplicates))
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := moment(out[i].AcceptedAt), moment(out[j].AcceptedAt)
		if !left.Equal(right) {
			return left.After(right)
		}
		return out[i].ID < out[j].ID
	})
	if out == nil {
		return []LearningChain{}
	}
	return out
}

func incompleteLearningChain(accepted *run.Run, reason string) LearningChain {
	return LearningChain{
		ID:               digestID(strings.Join([]string{learningChainVersion, accepted.RequestID, accepted.Ticket, accepted.EarlierAttemptOf, accepted.ID}, "\x00")),
		RequestID:        accepted.RequestID,
		Ticket:           learningTicketDisplay(accepted.Ticket),
		FailedRunID:      accepted.EarlierAttemptOf,
		AcceptedRunID:    accepted.ID,
		AcceptedOutcome:  string(accepted.State),
		AcceptedAt:       accepted.UpdatedAt,
		IneligibleReason: reason,
	}
}

func indexLearningRuns(runs []*run.Run) (map[string]*run.Run, map[string]bool) {
	byID := make(map[string]*run.Run, len(runs))
	duplicates := make(map[string]bool)
	for _, r := range runs {
		if r == nil || r.ID == "" {
			continue
		}
		if _, ok := byID[r.ID]; ok {
			duplicates[r.ID] = true
			continue
		}
		byID[r.ID] = r
	}
	return byID, duplicates
}

func projectLearningChain(failed, accepted *run.Run, duplicates map[string]bool) LearningChain {
	chain := makeLearningChain(failed, accepted)
	if reason := learningChainCompletenessReason(failed, accepted, duplicates); reason != "" {
		chain.IneligibleReason = reason
		return chain
	}
	chain.Complete = true
	if reason := learningChainEligibilityReason(failed, accepted, chain); reason != "" {
		chain.IneligibleReason = reason
		return chain
	}
	chain.Eligible = true
	return chain
}

func learningChainCompletenessReason(failed, accepted *run.Run, duplicates map[string]bool) string {
	switch {
	case duplicates[failed.ID] || duplicates[accepted.ID]:
		return "duplicate_run_id"
	case failed.ID == accepted.ID || failed.ID == "" || accepted.ID == "":
		return "invalid_run_id"
	case failed.RequestID == "" || accepted.RequestID == "" || failed.RequestID != accepted.RequestID:
		return "request_mismatch"
	case !sameLearningTicket(failed.Ticket, accepted.Ticket):
		return "ticket_mismatch"
	case failed.RepositoryRoot == "" || accepted.RepositoryRoot == "" || failed.RepositoryRoot != accepted.RepositoryRoot:
		return "repository_mismatch"
	case !validLearningTime(failed.UpdatedAt) || !validLearningTime(accepted.UpdatedAt):
		return "missing_timestamp"
	case failed.ProjectConfigCommitSHA == "" || accepted.ProjectConfigCommitSHA == "":
		return "missing_config_snapshot"
	case failed.SpecSHA256 == "" || accepted.SpecSHA256 == "":
		return "missing_spec_snapshot"
	case !validSHA256(failed.SpecSHA256) || !validSHA256(accepted.SpecSHA256):
		return "invalid_snapshot_hash"
	case (failed.ProjectConfigSHA256 != "" && !validSHA256(failed.ProjectConfigSHA256)) ||
		(accepted.ProjectConfigSHA256 != "" && !validSHA256(accepted.ProjectConfigSHA256)):
		return "invalid_snapshot_hash"
	default:
		return ""
	}
}

// Corrective runs use their own -ticket ID, formed by appending
// -corrective<N> to the original ticket. The direct EarlierAttemptOf link
// remains the source of lineage; this check only accepts the known ticket
// ID shape for that linked run.
func sameLearningTicket(failedTicket, acceptedTicket string) bool {
	failedBase, failedValid := learningTicketBase(failedTicket)
	acceptedBase, acceptedValid := learningTicketBase(acceptedTicket)
	return failedValid && acceptedValid && failedBase == acceptedBase
}

func learningTicketBase(ticket string) (string, bool) {
	if ticket == "" {
		return "", false
	}
	separator := strings.LastIndex(ticket, "-corrective")
	if separator < 0 {
		return ticket, true
	}
	suffix := ticket[separator+len("-corrective"):]
	if suffix == "" {
		return "", false
	}
	allDigits := true
	for _, r := range suffix {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if !allDigits {
		return ticket, true
	}
	if separator == 0 || suffix[0] == '0' {
		return "", false
	}
	return ticket[:separator], true
}

type learningEligibilityRule struct {
	reason string
	fails  func(failed, accepted *run.Run, chain LearningChain) bool
}

// Keep inference eligibility as an ordered list of independent rules. New
// evidence requirements can add a rule without changing the projection or
// lineage checks; the first matching rule supplies the stable API reason.
var learningEligibilityRules = []learningEligibilityRule{
	{reason: "spec_changed", fails: func(failed, accepted *run.Run, _ LearningChain) bool {
		return failed.SpecSHA256 != accepted.SpecSHA256
	}},
	{reason: "config_changed", fails: func(failed, accepted *run.Run, _ LearningChain) bool {
		return failed.ProjectConfigSHA256 != accepted.ProjectConfigSHA256
	}},
	{reason: "accepted_with_override_or_rescue", fails: func(_, accepted *run.Run, _ LearningChain) bool {
		return len(accepted.Overrides) > 0 || len(accepted.Rescues) > 0
	}},
	{reason: "unknown_check_type", fails: func(_, _ *run.Run, chain LearningChain) bool {
		return chain.IneligibleReason == "unknown_check_type"
	}},
	{reason: "no_failed_check", fails: func(_, _ *run.Run, chain LearningChain) bool {
		return len(chain.FailedCheckTypes) == 0
	}},
	{reason: "missing_success_check", fails: func(failed, accepted *run.Run, _ LearningChain) bool {
		return failedChecksRecovered(failed.GateResults, accepted.GateResults) == "missing_success_check"
	}},
	{reason: "missing_command_evidence", fails: func(failed, accepted *run.Run, _ LearningChain) bool {
		return failedChecksRecovered(failed.GateResults, accepted.GateResults) == "missing_command_evidence"
	}},
	{reason: "check_command_changed", fails: func(failed, accepted *run.Run, _ LearningChain) bool {
		return failedChecksRecovered(failed.GateResults, accepted.GateResults) == "check_command_changed"
	}},
}

func learningChainEligibilityReason(failed, accepted *run.Run, chain LearningChain) string {
	for _, rule := range learningEligibilityRules {
		if rule.fails(failed, accepted, chain) {
			return rule.reason
		}
	}
	return ""
}

func failedChecksRecovered(failed, accepted []run.GateResult) string {
	for _, failedGate := range failed {
		if failedGate.Passed || !learningCheckTypes[failedGate.Check] {
			continue
		}
		if learningCheckRequiresCommand(failedGate.Check) && len(failedGate.Command) == 0 {
			return "missing_command_evidence"
		}
		foundPassed := false
		foundMatchingCommand := false
		for _, acceptedGate := range accepted {
			if acceptedGate.Check != failedGate.Check || !acceptedGate.Passed {
				continue
			}
			foundPassed = true
			if learningCheckRequiresCommand(failedGate.Check) && len(acceptedGate.Command) == 0 {
				return "missing_command_evidence"
			}
			if sameCommand(failedGate.Command, acceptedGate.Command) {
				foundMatchingCommand = true
				break
			}
		}
		if !foundPassed {
			return "missing_success_check"
		}
		if !foundMatchingCommand {
			return "check_command_changed"
		}
	}
	return ""
}

func learningCheckRequiresCommand(check string) bool {
	switch check {
	case "diff_scope", policy.TestsRedOnBaseCheck:
		return false
	default:
		return true
	}
}

func sameCommand(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func makeLearningChain(failed, accepted *run.Run) LearningChain {
	checks := map[string]bool{}
	unknown := false
	for _, gate := range failed.GateResults {
		if gate.Passed || gate.Check == "" {
			continue
		}
		if !learningCheckTypes[gate.Check] {
			unknown = true
			continue
		}
		checks[gate.Check] = true
	}
	checkTypes := make([]string, 0, len(checks))
	for check := range checks {
		checkTypes = append(checkTypes, check)
	}
	sort.Strings(checkTypes)
	id := digestID(strings.Join([]string{
		learningChainVersion,
		failed.RequestID,
		failed.Ticket,
		failed.ID,
		accepted.ID,
	}, "\x00"))
	chain := LearningChain{
		ID:                          id,
		RequestID:                   failed.RequestID,
		Ticket:                      learningTicketDisplay(failed.Ticket),
		FailedRunID:                 failed.ID,
		AcceptedRunID:               accepted.ID,
		FailedOutcome:               string(failed.State),
		AcceptedOutcome:             string(accepted.State),
		FailedAt:                    failed.UpdatedAt,
		AcceptedAt:                  accepted.UpdatedAt,
		FailedCheckTypes:            checkTypes,
		FailedConfigCommitSHA:       failed.ProjectConfigCommitSHA,
		AcceptedConfigCommitSHA:     accepted.ProjectConfigCommitSHA,
		FailedConfigContentSHA256:   failed.ProjectConfigSHA256,
		AcceptedConfigContentSHA256: accepted.ProjectConfigSHA256,
	}
	if unknown {
		chain.IneligibleReason = "unknown_check_type"
	}
	return chain
}

func learningTicketDisplay(ticket string) string {
	if base, ok := learningTicketBase(ticket); ok {
		return base
	}
	return ticket
}

func validLearningTime(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func digestID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
