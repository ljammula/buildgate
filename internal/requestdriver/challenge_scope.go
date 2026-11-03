package requestdriver

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"buildgate/internal/handoff"
	"buildgate/internal/policy"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/ticketspec"
)

// Headings of the two blocks a challenge spec gets about a ticket's scope.
const (
	challengeAllowedBlockHeading = "# Files this ticket may change"
	challengeRaisedBlockHeading  = "# Raised before by an earlier ticket's challenge"
)

// maxRaisedListed bounds the entries of the raised block and the files of an
// out-of-scope entry.
const maxRaisedListed = 20

// approvedAllowedFiles are ticket's Allowed-Files when a person approved the
// list (the only list the factory refuses a diff by, and so the only one a
// challenge is scoped by); nil otherwise, or when the ticket declares none.
func approvedAllowedFiles(r *request.Request, ticket request.Ticket) []string {
	if approved, _ := request.ScopeApproved(r, ticket.Index); !approved || ticket.SpecPath == "" {
		return nil
	}
	allowed, err := ticketspec.ParseAllowedFiles(ticket.SpecPath)
	if err != nil {
		return nil
	}
	return allowed
}

// challengeScopeBlocks is the text added to a ticket's challenge spec: the
// files the ticket may change, then what earlier tickets' challenges raised
// whose files all lie among them (listed by criterion and files, never test
// prose). "" when the ticket has no approved Allowed-Files.
func challengeScopeBlocks(r *request.Request, ticket request.Ticket) string {
	allowed := approvedAllowedFiles(r, ticket)
	if len(allowed) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nWrite a test only when the code a failure of it would need fixed is in these files; each test's `subject:` paths must be among them.\n\n", challengeAllowedBlockHeading)
	for _, f := range allowed {
		fmt.Fprintf(&b, "- %s\n", handoff.CleanSentence(f))
	}
	var lines []string
	for _, e := range r.ChallengeOutOfScope {
		if e.Ticket >= ticket.Index || len(e.Files) == 0 || len(lines) == maxRaisedListed {
			continue
		}
		if !slices.ContainsFunc(e.Files, func(f string) bool { return !policy.PathCoveredByAllowed(f, allowed) }) {
			line := fmt.Sprintf("- %s: %s", handoff.CleanWord(e.Criterion), handoff.CleanSentence(strings.Join(e.Files, ", ")))
			if !slices.Contains(lines, line) {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) > 0 {
		fmt.Fprintf(&b, "\n%s\n\nA held test for each criterion below could not be fixed inside an earlier ticket, because the fix needs files that ticket may not change. Those files are in this ticket's scope now:\n\n%s\n", challengeRaisedBlockHeading, strings.Join(lines, "\n"))
	}
	return b.String()
}

// recordSubjectOutOfScope records on r, for ticket, each held file of runRecord
// that was not run because its subject lay outside the ticket's Allowed-Files
// (run.ChallengeSubjectOutside); a file already recorded is left. It reports
// whether anything was added.
func recordSubjectOutOfScope(r *request.Request, ticket *request.Ticket, runRecord *run.Run) bool {
	if runRecord == nil || runRecord.Challenge == nil {
		return false
	}
	allowed := approvedAllowedFiles(r, *ticket)
	added := false
	for _, f := range runRecord.Challenge.Files {
		if !strings.HasSuffix(f.NotRun, run.ChallengeSubjectOutside) || len(f.Subject) == 0 || r.ChallengeRaisedFor(ticket.Index, f.Path) {
			continue
		}
		files := outsideAllowed(f.Subject, allowed)
		if len(files) == 0 {
			files = f.Subject
		}
		r.ChallengeOutOfScope = append(r.ChallengeOutOfScope, request.ChallengeRaised{
			Ticket: ticket.Index, Path: f.Path, Criterion: f.Criterion, Files: files, Source: request.ChallengeRaisedSubject,
		})
		added = true
	}
	return added
}

// outsideAllowed are the paths of paths that allowed does not cover, capped.
func outsideAllowed(paths, allowed []string) []string {
	var out []string
	for _, p := range paths {
		if !policy.PathCoveredByAllowed(p, allowed) && !slices.Contains(out, p) && len(out) < maxRaisedListed {
			out = append(out, p)
		}
	}
	return out
}

// roundOutsideScope are the files an advice round's run changed outside the
// ticket's Allowed-Files, when the round was stopped by diff_scope: what its
// red held tests needed fixed. nil for any other round, or a ticket without an
// approved list. The held files themselves are not counted.
func roundOutsideScope(r *request.Request, ticket request.Ticket, roundRun *run.Run) []string {
	if roundRun == nil || !failedGate(roundRun, "diff_scope") {
		return nil
	}
	allowed := approvedAllowedFiles(r, ticket)
	if len(allowed) == 0 {
		return nil
	}
	var held []string
	if roundRun.Challenge != nil {
		for _, f := range roundRun.Challenge.Files {
			held = append(held, f.Path)
		}
	}
	return outsideAllowed(slices.DeleteFunc(slices.Clone(roundRun.ChangedFiles), func(p string) bool { return slices.Contains(held, p) }), allowed)
}

// recordRoundOutOfScope records on r, for ticket, each of reds as needing
// files (the round's changes outside Allowed-Files), so no further round is
// started for it (heldReds).
func recordRoundOutOfScope(r *request.Request, ticket *request.Ticket, reds []run.ChallengeFile, files []string) {
	if len(files) == 0 {
		return
	}
	for _, f := range reds {
		if !r.ChallengeRaisedFor(ticket.Index, f.Path) {
			r.ChallengeOutOfScope = append(r.ChallengeOutOfScope, request.ChallengeRaised{
				Ticket: ticket.Index, Path: f.Path, Criterion: f.Criterion, Files: files, Source: request.ChallengeRaisedDiffScope,
			})
		}
	}
}

// outOfScopeReason is the declined comment's reason for a round stopped by
// diff_scope: the files the fix needs.
func outOfScopeReason(files []string) string {
	shown := files[:min(len(files), 5)]
	for i, f := range shown {
		shown[i] = handoff.CleanSentence(f)
	}
	return "out of scope: needs " + strings.Join(shown, ", ")
}

// criterionTextFor returns the text of the criterion a label such as AC-3
// cites in the request's approved spec, "" when it cannot be read.
func criterionTextFor(dataDir string, r *request.Request) func(label string) string {
	return func(label string) string {
		n := 0
		if _, err := fmt.Sscanf(label, "AC-%d", &n); err != nil || n < 1 {
			return ""
		}
		path, err := approvedSpecPath(dataDir, r)
		if err != nil || path == "" {
			return ""
		}
		spec, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		criteria, err := request.SpecAcceptanceCriteria(string(spec))
		if err != nil || n > len(criteria) {
			return ""
		}
		return criterionLeadRE.ReplaceAllString(criteria[n-1], "")
	}
}
