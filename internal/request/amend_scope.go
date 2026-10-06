package request

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/ticketspec"
)

// AmendScope widens a quarantined ticket's own approved Allowed-Files:
// scope -- `factoryd amend-scope -reason "..." <id> <file>...`, the one
// human path to recover a run diff_scope quarantined for touching a file
// the plan never listed but that is genuinely part of the ticket (found
// live: a Flutter + Go app repo ticket registering a new HTTP route had to add it to the
// repo's own route-inventory test, which the planner never named, and
// approved ticket specs are hash-pinned -- see request.VerifyApprovedHashes
// -- so there was no operator recovery short of re-planning everything).
//
// Takes the request lock and mutates r the same way Approve/Reject/Retry/
// SendBack do. Refused, wrapping ErrIllegalTransition for a wrong-state/
// wrong-shape problem (the same sentinel those four wrap, so a caller can
// map it to 409) or a plain error for a caller-input problem, when:
//   - r.State is not StateQuarantined;
//   - r.TicketIndex names no ticket in r.Tickets -- the one Retry would
//     rebuild (see Retry's own doc comment for exactly how it picks this
//     ticket);
//   - that ticket already has a pull request (PRURL != "") -- amend-scope
//     only widens scope before a build exists to widen it for;
//   - reason is empty or all whitespace;
//   - files is empty;
//   - VerifyApprovedHashes(dataDir, r) fails -- amend-scope must never
//     bless some other post-approval edit riding along with it;
//   - any file is not a valid workspace-relative path
//     (ticketspec.ValidWorkspaceRelativePath), is repeated within files, or
//     is already in the ticket's current Allowed-Files
//     (ticketspec.ParseAllowedFiles);
//   - the ticket spec does not declare exactly one top-level Allowed-Files:
//     line (ticketspec.RewriteAllowedFilesLine's own refusal): zero means
//     there is no scope to widen, and more than one is already ambiguous
//     about which line diff_scope's own gate enforces.
//
// On success, the ticket spec's own Allowed-Files: line is rewritten to
// append each of files -- every other byte of the file is left untouched --
// written atomically (temp file + rename, this package's own convention),
// its recomputed SHA-256 replaces the ticket spec's entry in
// r.ApprovedSHA256 (ApprovedBy/ApprovedAt are left alone: this is not a
// fresh approval, just a re-pin of one file an operator, not the drafting
// agent, just edited), and a History entry records by, the ticket index,
// the files added, and reason. r.State stays StateQuarantined throughout --
// the operator runs `factoryd retry <id>` next, same as any other
// quarantine recovery.
func AmendScope(dataDir, id, by, reason string, files []string, now time.Time) (*Request, error) {
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if r.State != StateQuarantined {
		return nil, fmt.Errorf("request %s: cannot amend scope from state %q (must be %q) %w", id, r.State, StateQuarantined, ErrIllegalTransition)
	}
	if r.TicketIndex < 1 || r.TicketIndex > len(r.Tickets) {
		return nil, fmt.Errorf("request %s: ticket index %d has no matching ticket to amend %w", id, r.TicketIndex, ErrIllegalTransition)
	}
	ticket := &r.Tickets[r.TicketIndex-1]
	if ticket.PRURL != "" {
		return nil, fmt.Errorf("request %s: ticket %d already has a pull request (%s) -- amend-scope only widens scope before a ticket has one %w", id, r.TicketIndex, ticket.PRURL, ErrIllegalTransition)
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("request %s: amend-scope requires a reason", id)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("request %s: amend-scope requires at least one file", id)
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return nil, err
	}

	existing, err := ticketspec.ParseAllowedFiles(ticket.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("request %s: ticket %d: %w", id, r.TicketIndex, err)
	}
	existingSet := make(map[string]bool, len(existing))
	for _, f := range existing {
		existingSet[f] = true
	}
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if !ticketspec.ValidWorkspaceRelativePath(f) {
			return nil, fmt.Errorf("request %s: %q is not a valid workspace-relative path (must not be absolute and must not contain a \"..\" segment)", id, f)
		}
		if seen[f] {
			return nil, fmt.Errorf("request %s: %q is repeated in the files to add", id, f)
		}
		seen[f] = true
		if existingSet[f] {
			return nil, fmt.Errorf("request %s: %q is already in ticket %d's Allowed-Files", id, f, r.TicketIndex)
		}
	}

	newContent, err := ticketspec.RewriteAllowedFilesLine(ticket.SpecPath, files)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", id, err)
	}
	tmp := ticket.SpecPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(newContent), 0o600); err != nil {
		return nil, fmt.Errorf("request %s: write %s: %w", id, ticket.SpecPath, err)
	}
	if err := os.Rename(tmp, ticket.SpecPath); err != nil {
		return nil, fmt.Errorf("request %s: write %s: %w", id, ticket.SpecPath, err)
	}

	relPath, err := filepath.Rel(Dir(dataDir, id), ticket.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", id, err)
	}
	hash, err := HashFile(dataDir, id, relPath)
	if err != nil {
		return nil, fmt.Errorf("request %s: hash %s: %w", id, relPath, err)
	}
	if r.ApprovedSHA256 == nil {
		r.ApprovedSHA256 = make(map[string]string, 1)
	}
	r.ApprovedSHA256[relPath] = hash

	ts := now.UTC().Format(time.RFC3339Nano)
	r.History = append(r.History, Transition{
		From:   StateQuarantined,
		To:     StateQuarantined,
		At:     ts,
		By:     by,
		Reason: fmt.Sprintf("scope amended by %s: ticket %d Allowed-Files += %s (reason: %s)", by, r.TicketIndex, strings.Join(files, ", "), reason),
	})
	r.UpdatedAt = ts

	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}
