// Package prune frees the disk a data dir's finished work still holds: the
// isolated worktree of each run of a request that can never move again, its
// run's scratch directory and kept build session, and its round refs
// (refs/buildgate/rounds/<request>/, never a branch). It never removes a
// record: run.json, request.json and every other file under
// runs/ and requests/ (logs, prompts, evidence, progress feeds) stay, because
// `factoryd stats`, `watch`, `inspect`, `logs`, the console and
// scripts/baseline.py read them. No branch is ever deleted, a retry-from
// branch included: its commits may be unpushed work, and a local branch costs
// no disk of its own.
//
// What it selects (Plan) is decided from the records alone; what it removes
// (Apply) goes through a Remover the caller gives it for the git work.
package prune

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/workspace"
)

// Kinds of Item.
const (
	KindWorktree     = "worktree"
	KindScratch      = "scratch"
	KindBuildSession = "build-session"
)

// Item is one directory Apply removes.
type Item struct {
	Kind  string
	Path  string
	Bytes int64
	// Owner is the request the item belongs to, or the run for a run with
	// no request.
	Owner string
	RunID string
	// repoDir is the repository a worktree is registered in.
	repoDir string
}

// Plan is what pruning dataDir would remove.
type Plan struct {
	DataDir string
	// Requests are the finished requests whose round refs Apply prunes.
	Requests []string
	Items    []Item
	// Skipped lists each request directory left alone, with the reason: a
	// request.json that cannot be read, or whose id is not its directory's.
	Skipped []string
	// olderThan and now are the plan's own age rule, which Apply applies
	// again under each request's lock.
	olderThan time.Duration
	now       time.Time
}

// Bytes is the sum of the items' sizes.
func (p Plan) Bytes() int64 {
	var n int64
	for _, it := range p.Items {
		n += it.Bytes
	}
	return n
}

// Remover is the git work of Apply: removing a worktree registered in
// repoDir (its branch stays), and deleting a finished request's round refs
// (refs/buildgate/rounds/<id>/) from a repository. Neither touches a branch.
type Remover interface {
	RemoveWorktree(repoDir, worktreePath string) error
	DeleteRoundRefs(repoDir, requestID string) error
}

// Eligible reports whether a request may be pruned at now: it is finished
// (done, closed or cancelled: request.State.Finished, the states nothing can
// leave) and entered that state longer than olderThan ago. halted and
// quarantined are not: retry, send back and amend scope revive them.
func Eligible(r *request.Request, olderThan time.Duration, now time.Time) bool {
	if !r.State.Finished() {
		return false
	}
	since, ok := r.InStateSince()
	return ok && now.Sub(since) > olderThan
}

// loadRequests reads every request directory. A directory whose request.json
// cannot be read, or names another request's id, is returned in skipped and
// never planned: its fields cannot be trusted to say what is finished.
func loadRequests(dataDir string) (requests []*request.Request, skipped []string, err error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "requests"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, statErr := os.Stat(request.Path(dataDir, entry.Name())); os.IsNotExist(statErr) {
			continue
		}
		r, loadErr := request.Load(dataDir, entry.Name())
		switch {
		case loadErr != nil:
			skipped = append(skipped, fmt.Sprintf("request %s: %v", entry.Name(), loadErr))
		case r.ID != entry.Name():
			skipped = append(skipped, fmt.Sprintf("request %s: its request.json names %q", entry.Name(), r.ID))
		default:
			requests = append(requests, r)
		}
	}
	return requests, skipped, nil
}

// ownerOf is the id of the request run rec belongs to: its own RequestID, else
// the request its replay or a ticket of some request names it for (the same
// fallback as findOwningRequest, which also reads the requests' tickets). ""
// when no record says.
func ownerOf(rec *run.Run, byRun map[string]string) string {
	switch {
	case rec.RequestID != "":
		return rec.RequestID
	case rec.Replay != nil && rec.Replay.RequestID != "":
		return rec.Replay.RequestID
	}
	return byRun[rec.ID]
}

// ticketRuns maps every run a request's tickets name (a ticket's run, a
// round's runs, a round tag's run) to the request.
func ticketRuns(requests []*request.Request) map[string]string {
	out := map[string]string{}
	for _, r := range requests {
		for _, t := range r.Tickets {
			if t.RunID != "" {
				out[t.RunID] = r.ID
			}
			for _, rd := range t.Rounds {
				if rd.RunID != "" {
					out[rd.RunID] = r.ID
				}
				for _, id := range rd.PriorRunIDs {
					out[id] = r.ID
				}
			}
			for _, tag := range t.RoundTags {
				if tag.RunID != "" {
					out[tag.RunID] = r.ID
				}
			}
		}
	}
	return out
}

// PlanDataDir selects what pruning dataDir at now removes: the items of each
// eligible request (Eligible) every run of which is confirmed terminal. A run
// is a request's by ownerOf. A run no record ties to a request is never
// pruned: nothing in its record says it is not a request's ticket still to
// come back to.
func PlanDataDir(dataDir string, olderThan time.Duration, now time.Time) (Plan, error) {
	plan := Plan{DataDir: dataDir, olderThan: olderThan, now: now}
	requests, skipped, err := loadRequests(dataDir)
	if err != nil {
		return plan, err
	}
	plan.Skipped = skipped
	runs, err := run.LoadAll(dataDir)
	if err != nil {
		return plan, err
	}
	byTicket := ticketRuns(requests)
	byRequest := map[string][]*run.Run{}
	for _, r := range runs {
		if owner := ownerOf(r, byTicket); owner != "" {
			byRequest[owner] = append(byRequest[owner], r)
		}
	}
	pruned := map[string]bool{}
	for _, r := range requests {
		if !Eligible(r, olderThan, now) || !allConfirmedTerminal(byRequest[r.ID]) {
			continue
		}
		plan.Requests = append(plan.Requests, r.ID)
		for _, rec := range byRequest[r.ID] {
			pruned[rec.ID] = true
			plan.Items = append(plan.Items, runItems(dataDir, r.ID, rec)...)
		}
	}
	plan.Items = dropClaimedWorktrees(dataDir, plan.Items, pruned, runs)
	sort.SliceStable(plan.Items, func(i, j int) bool { return plan.Items[i].Bytes > plan.Items[j].Bytes })
	return plan, nil
}

func allConfirmedTerminal(runs []*run.Run) bool {
	for _, r := range runs {
		if !r.TerminalConfirmed() || r.KeptForResume {
			return false
		}
	}
	return true
}

// runItems is what of rec is still on disk to remove: its worktree, its
// scratch directory and its kept build session.
func runItems(dataDir, owner string, rec *run.Run) []Item {
	var items []Item
	if path, ok := ownedWorktree(dataDir, rec); ok {
		items = append(items, Item{Kind: KindWorktree, Path: path, Owner: owner, RunID: rec.ID, repoDir: rec.ProjectPath})
	}
	scratch := sandbox.RunScratchDir(dataDir, rec.ID)
	if isDir(scratch) {
		items = append(items, Item{Kind: KindScratch, Path: scratch, Owner: owner, RunID: rec.ID})
	}
	session := filepath.Join(run.Dir(dataDir, rec.ID), run.BuildSessionDirName)
	if isDir(session) {
		items = append(items, Item{Kind: KindBuildSession, Path: session, Owner: owner, RunID: rec.ID})
	}
	for i := range items {
		items[i].Bytes = dirBytes(items[i].Path)
	}
	return items
}

// ownedWorktree is rec's isolated worktree when it is one the factory made
// for it and it still exists: the run recorded a branch and a workspace other
// than its project, and that workspace is a linked worktree (a .git file, not
// a repository) directly inside dataDir/workspaces. Nothing else is ever a
// worktree to remove: an operator's own checkout fails every one of these.
func ownedWorktree(dataDir string, rec *run.Run) (string, bool) {
	if rec.Branch == "" || rec.WorkspacePath == "" || rec.WorkspacePath == rec.ProjectPath || rec.ProjectPath == "" {
		return "", false
	}
	parent, err := workspace.CanonicalPath(filepath.Join(dataDir, "workspaces"))
	if err != nil {
		return "", false
	}
	path, err := workspace.CanonicalPath(rec.WorkspacePath)
	if err != nil || filepath.Dir(path) != parent {
		return "", false
	}
	info, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

// dropClaimedWorktrees leaves out every worktree that a run not being pruned
// names as its WorkspacePath, or that an isolation marker of such a run names:
// a resume adopted it, or the run still works there. A worktree two pruned
// runs share is listed once. Markers that cannot be read keep every worktree.
func dropClaimedWorktrees(dataDir string, items []Item, pruned map[string]bool, runs []*run.Run) []Item {
	paths, err := workspace.ListIsolationMarkerPaths(dataDir)
	claimed := map[string]bool{}
	for _, p := range paths {
		m, loadErr := workspace.LoadIsolationMarker(p)
		if loadErr != nil {
			err = errors.Join(err, loadErr)
			continue
		}
		if pruned[m.RunID] {
			continue
		}
		if c, cErr := workspace.CanonicalPath(m.WorktreePath); cErr == nil {
			claimed[c] = true
		}
	}
	for _, rec := range runs {
		if pruned[rec.ID] || rec.WorkspacePath == "" {
			continue
		}
		if c, cErr := workspace.CanonicalPath(rec.WorkspacePath); cErr == nil {
			claimed[c] = true
		} else {
			claimed[rec.WorkspacePath] = true
		}
	}
	seen := map[string]bool{}
	var out []Item
	for _, it := range items {
		if it.Kind == KindWorktree && (err != nil || claimed[it.Path] || seen[it.Path]) {
			continue
		}
		seen[it.Path] = true
		out = append(out, it)
	}
	return out
}

// Apply removes the plan's items and deletes its requests' round refs, and
// returns the bytes freed: the sizes of the items that are gone afterwards.
// Before a worktree goes, what a build left in it that its run directory does
// not hold yet (the agent's report, the failed rounds' output) is copied
// there; a file the run directory already holds is never replaced. Each
// request is handled under its lock, held until its removal is done: its
// request and its runs are read again there, and only an item the fresh read
// still selects is removed. A request that is no longer eligible is skipped
// and reported. It tries every request and returns the joined failures.
func (p Plan) Apply(rm Remover) (int64, error) {
	var errs []error
	var freed int64
	for _, id := range p.Requests {
		n, err := p.applyRequest(id, rm)
		freed += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return freed, errors.Join(errs...)
}

func (p Plan) applyRequest(id string, rm Remover) (int64, error) {
	unlock, err := request.Lock(p.DataDir, id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	fresh, err := PlanDataDir(p.DataDir, p.olderThan, p.now)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(fresh.Requests, id) {
		return 0, fmt.Errorf("request %s is not finished and old enough now: left as it is", id)
	}
	r, err := request.Load(p.DataDir, id)
	if err != nil {
		return 0, err
	}
	planned := map[string]bool{}
	for _, it := range p.Items {
		if it.Owner == id {
			planned[it.Kind+"\x00"+it.Path] = true
		}
	}
	var errs []error
	var freed int64
	for _, it := range fresh.Items {
		if it.Owner != id || !planned[it.Kind+"\x00"+it.Path] {
			continue
		}
		if err := p.remove(it, rm); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", it.Kind, it.Path, err))
			continue
		}
		if _, err := os.Lstat(it.Path); os.IsNotExist(err) {
			freed += it.Bytes
		}
	}
	for _, repo := range roundRefRepos(r) {
		if err := rm.DeleteRoundRefs(repo, id); err != nil {
			errs = append(errs, fmt.Errorf("round refs of %s in %s: %w", id, repo, err))
		}
	}
	return freed, errors.Join(errs...)
}

// roundRefRepos is every repository a request's round ledger names.
func roundRefRepos(r *request.Request) []string {
	var repos []string
	for _, t := range r.Tickets {
		for _, tag := range t.RoundTags {
			if tag.Repo != "" && !slices.Contains(repos, tag.Repo) {
				repos = append(repos, tag.Repo)
			}
		}
	}
	return repos
}

func (p Plan) remove(it Item, rm Remover) error {
	switch it.Kind {
	case KindWorktree:
		retainMissingArtifacts(it.Path, run.Dir(p.DataDir, it.RunID))
		return rm.RemoveWorktree(it.repoDir, it.Path)
	case KindScratch:
		return sandbox.RemoveScratchDir(p.DataDir, it.RunID)
	case KindBuildSession:
		// Through an os.Root on the runs directory, as
		// release.PruneBuildSessions removes one: no link leads outside it.
		root, err := os.OpenRoot(run.Dir(p.DataDir, ""))
		if err != nil {
			return err
		}
		defer root.Close()
		return root.RemoveAll(path.Join(it.RunID, run.BuildSessionDirName))
	}
	return fmt.Errorf("unknown kind %q", it.Kind)
}

// retainMissingArtifacts copies the agent's report and the failed rounds'
// saved output out of worktree into runDir when runDir has neither yet, so a
// worktree that went before its run was rolled back leaves what a rollback
// would have kept. Best effort: the copy is for the operator, never evidence.
func retainMissingArtifacts(worktree, runDir string) {
	_, reportErr := os.Lstat(filepath.Join(runDir, run.AgentReportFileName))
	_, logsErr := os.Lstat(filepath.Join(runDir, "round-logs"))
	if os.IsNotExist(reportErr) && os.IsNotExist(logsErr) {
		_ = run.RetainBuildArtifacts(worktree, runDir)
	}
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// dirBytes is the size of the regular files under dir, links not followed.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, infoErr := d.Info(); infoErr == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
