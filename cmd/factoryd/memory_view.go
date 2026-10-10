package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/api"
	"buildgate/internal/handoff"
	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

// maxMemoryRunsScanned bounds the finished runs one collection reads notes
// from: the project's newest.
const maxMemoryRunsScanned = 200

// headCommit is hostBoundary.headCommit's real body.
func (impl realHost) headCommit(ctx context.Context, repoDir string) (string, error) {
	out, err := gitObjectOutput(ctx, repoDir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if !commitHexPattern.MatchString(commit) {
		return "", fmt.Errorf("HEAD is not a commit (%q)", commit)
	}
	return commit, nil
}

// memorySectionAtHead reads root AGENTS.md at the checkout's HEAD from git
// objects, never from the worktree, and splits it around its fenced section.
// file is nil when the repository has no AGENTS.md there.
func memorySectionAtHead(ctx context.Context, dp *deps, repoRoot string) (file []byte, section memory.Section, err error) {
	ctx, cancel := context.WithTimeout(ctx, memoryGitTimeout)
	defer cancel()
	head, err := dp.host.headCommit(ctx, repoRoot)
	if err != nil {
		return nil, memory.Section{}, fmt.Errorf("read HEAD of %s: %w", repoRoot, err)
	}
	data, exists, err := dp.host.blobAtCommit(ctx, repoRoot, head, agentsFile)
	if err != nil {
		return nil, memory.Section{}, fmt.Errorf("read %s at HEAD: %w", agentsFile, err)
	}
	if !exists {
		return nil, memory.Section{}, nil
	}
	section, err = memory.ParseSection(data)
	if err != nil {
		return nil, memory.Section{}, fmt.Errorf("%s at HEAD: %w", agentsFile, err)
	}
	return data, section, nil
}

// staleProposal is the id of the repository's open memory request whose
// proposal no longer fits AGENTS.md at HEAD: the file there is neither the
// one the proposal was rendered from nor the one it proposes (its pull
// request merged). "" when there is none, or when it cannot be told.
func staleProposal(ctx context.Context, dp *deps, dataDir, repoRoot, project string) string {
	id, err := activeMemoryRequest(dataDir, project)
	if err != nil || id == "" {
		return ""
	}
	proposal, has, err := memory.LoadProposalFor(dataDir, memory.StoreKey(project, repoRoot), id)
	if err != nil || !has {
		return ""
	}
	hash, err := agentsHashAtHead(ctx, dp, repoRoot)
	if err != nil || hash == proposal.BaseBlobSHA256 || hash == proposal.ExpectedSHA256 {
		return ""
	}
	return id
}

// memoryRead is a project's memory as read without changing anything.
type memoryRead struct {
	// gateErr is the refusal a changing subcommand would get, nil when
	// memory is on.
	gateErr error
	budget  memory.Budget
	section memory.Section
	// sectionErr is why the section at HEAD could not be read.
	sectionErr error
	state      memory.StoreState
}

// readMemory reads the three switches, the section at HEAD and the store. It
// creates no file or directory.
func readMemory(ctx context.Context, dp *deps, settings sessionconfig.Settings, dataDir, repoRoot, project string) (memoryRead, error) {
	var ro memoryRead
	if err := memory.ValidProject(project); err != nil {
		return ro, err
	}
	store, err := memory.OpenReadOnly(dataDir, memory.StoreKey(project, repoRoot))
	if err != nil {
		return ro, err
	}
	engaged, err := release.IsEngaged(dataDir, project)
	if err != nil {
		return ro, fmt.Errorf("memory: read the kill switch for project %q: %w", project, err)
	}
	marker, err := store.Off()
	if err != nil {
		return ro, err
	}
	budget, listed := settings.MemoryFor(repoRoot)
	ro.budget = budget.OrDefault()
	ro.gateErr = memory.Gate(listed, marker, engaged)
	_, ro.section, ro.sectionErr = memorySectionAtHead(ctx, dp, repoRoot)
	if ro.state, err = store.Load(); err != nil {
		return ro, err
	}
	return ro, nil
}

// view renders what was read as the API's shape, candidates most seen first,
// then newest, dropped ones last.
func (ro memoryRead) view(project string) api.ProjectMemory {
	view := api.ProjectMemory{
		Project: project, On: ro.gateErr == nil,
		BudgetLines: ro.budget.Lines, BudgetChars: ro.budget.Chars,
		InForce: cleanLines(ro.section.Lines), Candidates: []api.MemoryCandidate{},
	}
	if ro.gateErr != nil {
		view.OffReason = strings.TrimPrefix(ro.gateErr.Error(), memory.ErrMemoryOff.Error()+": ")
	}
	if ro.sectionErr != nil {
		view.SectionError = sanitize.Line(ro.sectionErr.Error())
	}
	view.UsedLines, view.UsedChars = memory.Used(ro.section.Lines)
	lessons := append([]memory.Lesson(nil), ro.state.Lessons...)
	sort.SliceStable(lessons, func(i, j int) bool {
		a, b := lessons[i], lessons[j]
		if da, db := a.State == memory.StateDropped, b.State == memory.StateDropped; da != db {
			return db
		}
		if a.Seen != b.Seen {
			return a.Seen > b.Seen
		}
		return a.LastSeenAt > b.LastSeenAt
	})
	for _, l := range lessons {
		view.Candidates = append(view.Candidates, api.MemoryCandidate{
			ID: sanitize.Line(l.ID), Line: sanitize.Line(l.Line), Source: sanitize.Line(l.Source), State: sanitize.Line(string(l.State)), Seen: l.Seen,
			FirstSeenAt: sanitize.Line(l.FirstSeenAt), LastSeenAt: sanitize.Line(l.LastSeenAt), RequestID: sanitize.Line(l.RequestID),
		})
	}
	return view
}

// cleanLines is lines, each made safe to print on a terminal or a page
// (sanitize.Line). Lines are read from a repository or a store file, which a
// build or a hand edit can fill with escape sequences.
func cleanLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, sanitize.Line(l))
	}
	return out
}

// runOfRepository reports whether r was built for the repository at repoRoot:
// its recorded repository root, the value the store key hashes, cleaned the
// same way. A run that records none belongs to no repository's memory.
func runOfRepository(r *run.Run, repoRoot string) bool {
	return r.RepositoryRoot != "" && filepath.Clean(r.RepositoryRoot) == filepath.Clean(repoRoot)
}

// repositoryRunsNewestFirst loads the run records of the repository at
// repoRoot (not of every repository whose base name is project), newest
// record first, at most limit. A record that cannot be read, or that names
// another run than its directory, is left out.
func repositoryRunsNewestFirst(dataDir, project, repoRoot string, limit int) []*run.Run {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		return nil
	}
	type dated struct {
		r    *run.Run
		unix int64
	}
	var found []dated
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(run.Dir(dataDir, entry.Name()), "run.json"))
		if err != nil {
			continue
		}
		loaded, err := run.Load(dataDir, entry.Name())
		if err != nil || loaded.ID != entry.Name() || release.ProjectOf(loaded) != project || !runOfRepository(loaded, repoRoot) {
			continue
		}
		found = append(found, dated{loaded, info.ModTime().UnixNano()})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].unix > found[j].unix })
	if len(found) > limit {
		found = found[:limit]
	}
	runs := make([]*run.Run, 0, len(found))
	for _, d := range found {
		runs = append(runs, d.r)
	}
	return runs
}

// repositoryNotes returns what a finished run's build agent said is worth
// knowing about the repository, from the run's handoff, as the agent wrote
// it (backticks kept: the memory text rule judges them). The handoff is read
// only against the hash and state the run record holds, so one changed after
// the run recorded it yields nothing.
func repositoryNotes(dataDir string, r *run.Run) []string {
	if r.HandoffSHA256 == "" || r.State != run.StateQuarantined && r.State != run.StateHalted {
		return nil
	}
	doc, err := handoff.Load(run.Dir(dataDir, r.ID), r.HandoffSHA256, r.State)
	if err != nil || doc.AgentNotes == nil {
		return nil
	}
	return doc.AgentNotes.MemoryCandidates()
}

// memoryRequestState is Reconcile's view of a request: known when its record
// loads, active until it is done or cancelled.
func memoryRequestState(dataDir string) func(string) (active, known bool) {
	return func(id string) (bool, bool) {
		if id == "" {
			return false, false
		}
		r, err := request.Load(dataDir, id)
		if err != nil {
			return false, false
		}
		return r.State != request.StateDone && r.State != request.StateCancelled, true
	}
}

// refreshMemoryStore collects candidates from the repository's newest
// finished runs, oldest of them first, and reconciles the store with the
// section at HEAD, in one locked update. It returns the state it saved.
func refreshMemoryStore(store *memory.Store, dataDir, project, repoRoot string, sectionLines []string, now string) (added, refused int, state memory.StoreState, err error) {
	runs := repositoryRunsNewestFirst(dataDir, project, repoRoot, maxMemoryRunsScanned)
	err = store.Update(func(st *memory.StoreState) error {
		counted := make(map[string]bool, len(st.CountedRuns))
		for _, id := range st.CountedRuns {
			counted[id] = true
		}
		for i := len(runs) - 1; i >= 0; i-- {
			if counted[runs[i].ID] {
				continue
			}
			items := repositoryNotes(dataDir, runs[i])
			if len(items) == 0 {
				continue
			}
			a, r := memory.CollectFromNotes(st, runs[i].ID, items, sectionLines, now)
			added, refused = added+a, refused+r
		}
		memory.Reconcile(st, sectionLines, memoryRequestState(dataDir), now)
		state = *st
		return nil
	})
	return added, refused, state, err
}

// projectRepositoryRoot is the repository a project's first request claimed
// (release.RejectProjectCollision), "" when none did.
func projectRepositoryRoot(dataDir, project string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "projects", project, "repository"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// apiProjectMemoryProvider is GET /projects/{project}/memory: the read half
// of `factoryd memory list`, for the repository the project's requests are
// submitted against. It collects nothing and writes nothing.
func apiProjectMemoryProvider(dp *deps, settings sessionconfig.Settings, dataDir string) api.ProjectMemoryProvider {
	return func(ctx context.Context, project string) (api.ProjectMemory, bool, error) {
		if err := memory.ValidProject(project); err != nil {
			return api.ProjectMemory{}, false, nil
		}
		repoRoot, err := projectRepositoryRoot(dataDir, project)
		if err != nil || repoRoot == "" {
			return api.ProjectMemory{}, false, err
		}
		ro, err := readMemory(ctx, dp, settings, dataDir, repoRoot, project)
		if err != nil {
			return api.ProjectMemory{}, false, err
		}
		return ro.view(project), true, nil
	}
}

// memoryStoreKeyOfRun is the store key of the repository a run's request was
// submitted against (memory.StoreKey of the request's project and the git
// root of its workspace), false for a run of no request or one whose request
// cannot be loaded. A memory request's proposal and changes are kept under
// this key.
func memoryStoreKeyOfRun(dataDir string, r *run.Run) (string, bool) {
	if r.RequestID == "" {
		return "", false
	}
	req, err := request.Load(dataDir, r.RequestID)
	if err != nil || req.Workspace == "" {
		return "", false
	}
	return memory.StoreKey(req.Project, release.RepositoryRoot(req.Workspace)), true
}
