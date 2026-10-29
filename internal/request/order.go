package request

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/run"
)

// TicketOrder marks a ticket the host carved from a mixed change by the
// repository's pr_order: it holds one group of the change an accepted run
// made, and nothing else.
type TicketOrder struct {
	// SourceRunID is the accepted run that was carved; its result commit is
	// what each carved commit's files are taken from.
	SourceRunID string `json:"source_run_id"`
	// Groups are the pr_order patterns the change was split by.
	Groups [][]string `json:"groups"`
	// Group is the 1-based group this ticket holds (len(Groups)+1 for the
	// files no group lists).
	Group int `json:"group"`
	// First is the index of the first ticket carved from the same run.
	First int `json:"first"`
	// Commit is the carved commit this ticket's run judges, Onto the commit
	// it is a child of (the diff base for the first ticket, the previous
	// carved commit after it) and Branch the local branch at Commit its run
	// starts on. Set by the host when it carves.
	Commit string `json:"commit,omitempty"`
	Onto   string `json:"onto,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// ReplaceWithCarved replaces ticket idx, whose accepted run sourceRunID is
// carved, with one ticket per entry of groups (1-based, in carve order). Each
// holds the replaced ticket's approved file byte for byte, so its pinned hash
// is the one that was approved; the tickets after it move up by the extra
// tickets (their files are renamed, their pins follow). The request stays in
// building at idx, TicketCount grows, and no ticket of the carve has a run.
func (r *Request) ReplaceWithCarved(dataDir string, idx int, sourceRunID string, order [][]string, groups []int) error {
	if r.State != StateBuilding || idx < 1 || idx > len(r.Tickets) || len(groups) < 2 || sourceRunID == "" {
		return fmt.Errorf("request %s: cannot carve ticket %d into %d in state %s", r.ID, idx, len(groups), r.State)
	}
	original := r.Tickets[idx-1]
	content, err := os.ReadFile(original.SpecPath)
	if err != nil {
		return fmt.Errorf("request %s: read ticket %d to carve: %w", r.ID, idx, err)
	}
	extra := len(groups) - 1
	dir := filepath.Dir(original.SpecPath)
	pathOf := func(index int) string { return filepath.Join(dir, fmt.Sprintf("%03d.spec.md", index)) }
	rel := func(path string) string {
		out, relErr := filepath.Rel(Dir(dataDir, r.ID), path)
		if relErr != nil {
			return path
		}
		return filepath.ToSlash(out)
	}
	for j := idx; j <= len(r.Tickets); j++ {
		if filepath.Base(r.Tickets[j-1].SpecPath) != filepath.Base(pathOf(j)) || filepath.Dir(r.Tickets[j-1].SpecPath) != dir {
			return fmt.Errorf("request %s: ticket %d's file %s is not %s, so the tickets cannot be renumbered", r.ID, j, r.Tickets[j-1].SpecPath, filepath.Base(pathOf(j)))
		}
	}
	later := append([]Ticket(nil), r.Tickets[idx:]...)
	for j := len(r.Tickets); j > idx; j-- {
		t := r.Tickets[j-1]
		to := pathOf(j + extra)
		if err := os.Rename(t.SpecPath, to); err != nil {
			return fmt.Errorf("request %s: move ticket %d to %d: %w", r.ID, j, j+extra, err)
		}
		if hash, ok := r.ApprovedSHA256[rel(t.SpecPath)]; ok {
			delete(r.ApprovedSHA256, rel(t.SpecPath))
			r.ApprovedSHA256[rel(to)] = hash
		}
		t.Index, t.SpecPath = j+extra, to
		later[j-idx-1] = t
	}
	carved := make([]Ticket, 0, len(groups))
	for k, group := range groups {
		path := pathOf(idx + k)
		if k > 0 {
			if err := os.WriteFile(path, content, 0o600); err != nil {
				return fmt.Errorf("request %s: write carved ticket %d: %w", r.ID, idx+k, err)
			}
			r.pinCarved(dataDir, path)
		}
		carved = append(carved, Ticket{
			Index:          idx + k,
			SpecPath:       path,
			ScopeAmendedBy: original.ScopeAmendedBy,
			Order:          &TicketOrder{SourceRunID: sourceRunID, Groups: order, Group: group, First: idx},
		})
	}
	tickets := append(append(append([]Ticket{}, r.Tickets[:idx-1]...), carved...), later...)
	r.Tickets = tickets
	r.TicketCount = len(tickets)
	return nil
}

// pinCarved records the hash of a carved ticket's file, equal to the one the
// replaced ticket was approved with.
func (r *Request) pinCarved(dataDir, path string) {
	rel, err := filepath.Rel(Dir(dataDir, r.ID), path)
	if err != nil {
		return
	}
	if hash, err := HashFile(dataDir, r.ID, filepath.ToSlash(rel)); err == nil {
		if r.ApprovedSHA256 == nil {
			r.ApprovedSHA256 = map[string]string{}
		}
		r.ApprovedSHA256[filepath.ToSlash(rel)] = hash
	}
}

// ReturnToPlanningForOrder sends the request back to planning once, as
// ReturnToPlanningForSize does for a size refusal: run sourceRunID is the
// accepted run whose change was carved by pr_order, size its measurement and
// groups the patterns the change was split by, reason why a carved commit's
// run was refused.
func (r *Request) ReturnToPlanningForOrder(sourceRunID string, size *run.ChangeSize, groups [][]string, reason string, now time.Time) error {
	if sourceRunID == "" || size == nil || len(groups) == 0 || r.SizeReplan != nil {
		return fmt.Errorf("request %s: invalid or repeated re-plan for order", r.ID)
	}
	if err := advance(r, []State{StateBuilding}, StatePlanning, factoryActor, strings.TrimSpace(reason), now); err != nil {
		return err
	}
	r.SizeReplan = &SizeReplan{RunID: sourceRunID, Measurement: size, Groups: groups}
	r.TicketIndex = 1
	return nil
}
