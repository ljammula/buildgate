package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/memory"
	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
)

// expectedFileHeading is the last section of a memory request's spec and of
// its ticket: the whole AGENTS.md the run must produce, in one code fence.
// It is last so that no heading inside the file is read as one of the
// document's own.
const expectedFileHeading = "## Expected AGENTS.md"

// memoryVerifyCommand is the verify command a request for the repository
// resolves when none is passed: the committed .factory.yml's.
func memoryVerifyCommand(repoRoot string) (string, error) {
	cfg, found, err := projectconfig.Load(repoRoot)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	if !found || strings.TrimSpace(cfg.VerifyCommand) == "" {
		return "", fmt.Errorf("this repository has no verify command: commit a verify_command in its %s, as any request for it needs", projectconfig.FileName)
	}
	return strings.TrimSpace(cfg.VerifyCommand), nil
}

// activeMemoryRequest is the id of the project's memory request that is
// neither done nor cancelled, "" when there is none.
func activeMemoryRequest(dataDir, project string) (string, error) {
	requests, err := request.List(dataDir)
	if err != nil {
		return "", fmt.Errorf("list requests: %w", err)
	}
	for _, r := range requests {
		if r.Source.Kind == request.SourceMemory && r.Project == project && r.State != request.StateDone && r.State != request.StateCancelled {
			return r.ID, nil
		}
	}
	return "", nil
}

// fullSectionHint adds what the operator can do to a refusal that the section
// has no room.
func fullSectionHint(err error) error {
	var refusal *memory.ApplyError
	if errors.As(err, &refusal) && (refusal.What == memory.ApplyOverLines || refusal.What == memory.ApplyOverChars) {
		return fmt.Errorf("%w\nthe section is full: pass -remove \"<exact line>\" for a line to take out (`factoryd memory list` prints them), or raise this repository's budget_lines / budget_chars under memory.repositories", err)
	}
	return err
}

// namedCandidates returns the candidates ids name, each once.
func namedCandidates(st *memory.StoreState, ids []string) ([]memory.Lesson, error) {
	var picked []memory.Lesson
	seen := map[string]bool{}
	for _, id := range ids {
		i, err := findLesson(st, id)
		if err != nil {
			return nil, err
		}
		l := st.Lessons[i]
		if l.State != memory.StateCandidate {
			return nil, fmt.Errorf("%s is %s, not a candidate", l.ID, l.State)
		}
		if !seen[l.ID] {
			seen[l.ID] = true
			picked = append(picked, l)
		}
	}
	return picked, nil
}

func lessonLines(lessons []memory.Lesson) []string {
	lines := make([]string, 0, len(lessons))
	for _, l := range lessons {
		lines = append(lines, l.Line)
	}
	return lines
}

// mostSeenCandidates takes candidates, most seen first then newest, while
// each still fits with the removals: the choice `propose` makes when no id is
// named. It never removes a line to make room.
func mostSeenCandidates(st *memory.StoreState, current, remove []string, budget memory.Budget) ([]memory.Lesson, []string, error) {
	lines, err := memory.Apply(current, nil, remove, budget, memory.MaxChangesPerRequest)
	if err != nil {
		return nil, nil, err
	}
	var candidates []memory.Lesson
	for _, l := range st.Lessons {
		if l.State == memory.StateCandidate {
			candidates = append(candidates, l)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Seen != candidates[j].Seen {
			return candidates[i].Seen > candidates[j].Seen
		}
		return candidates[i].LastSeenAt > candidates[j].LastSeenAt
	})
	var picked []memory.Lesson
	var noRoom error
	for _, l := range candidates {
		if len(picked)+len(remove) >= memory.MaxChangesPerRequest {
			break
		}
		next, err := memory.Apply(current, append(lessonLines(picked), l.Line), remove, budget, memory.MaxChangesPerRequest)
		if err != nil {
			noRoom = err
			continue
		}
		picked, lines = append(picked, l), next
	}
	switch {
	case len(picked) > 0 || len(remove) > 0:
		return picked, lines, nil
	case noRoom != nil:
		return nil, nil, fullSectionHint(noRoom)
	}
	return nil, nil, errors.New("there is no candidate to propose: `factoryd memory list` collects them, `factoryd memory add` adds your own")
}

// planMemoryChange picks the lessons to add and returns the section's lines
// after the change. Named ids are all or nothing: a section they do not fit
// is a refusal, never a smaller change.
func planMemoryChange(st *memory.StoreState, current, ids, remove []string, budget memory.Budget) ([]memory.Lesson, []string, error) {
	if len(ids) == 0 {
		return mostSeenCandidates(st, current, remove, budget)
	}
	picked, err := namedCandidates(st, ids)
	if err != nil {
		return nil, nil, err
	}
	lines, err := memory.Apply(current, lessonLines(picked), remove, budget, memory.MaxChangesPerRequest)
	if err != nil {
		return nil, nil, fullSectionHint(err)
	}
	return picked, lines, nil
}

func uniqueStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func proposalChanges(picked []memory.Lesson, remove []string) []memory.Change {
	changes := make([]memory.Change, 0, len(picked)+len(remove))
	for _, l := range picked {
		changes = append(changes, memory.Change{Line: l.Line, Source: l.Source, Runs: l.Runs})
	}
	for _, line := range remove {
		changes = append(changes, memory.Change{Remove: true, Line: line})
	}
	return changes
}

// propose builds one memory request: the candidates named (none named: the
// most seen that fit) are added to the section at HEAD and the -remove lines
// taken out, the whole expected AGENTS.md is rendered and saved as the
// request's proposal, and the request is submitted like any handed-over spec
// and plan. It stops at spec review; nothing is approved here.
func (mc *memoryCmd) propose(ctx context.Context, ids []string) (string, error) {
	store, budget, err := memoryGate(mc.dp, mc.settings, mc.dataDir, mc.repoRoot, mc.project)
	if err != nil {
		return "", err
	}
	verify, err := memoryVerifyCommand(mc.repoRoot)
	if err != nil {
		return "", err
	}
	if active, err := activeMemoryRequest(mc.dataDir, mc.project); err != nil {
		return "", err
	} else if active != "" {
		return "", fmt.Errorf("memory request %s for this repository is still open: one memory change at a time. Finish it (approve and merge) or `factoryd cancel %s` first", active, active)
	}
	file, section, err := memorySectionAtHead(ctx, mc.dp, mc.repoRoot)
	if err != nil {
		return "", err
	}
	_, _, state, err := refreshMemoryStore(store, mc.dataDir, mc.project, section.Lines, mc.stamp())
	if err != nil {
		return "", err
	}
	remove := uniqueStrings(mc.remove)
	picked, lines, err := planMemoryChange(&state, section.Lines, ids, remove, budget)
	if err != nil {
		return "", err
	}
	expected := section.Render(lines)
	if file != nil && string(expected) == string(file) {
		return "", errors.New("nothing to change: every line named is already in the section")
	}
	proposal := memory.Proposal{BaseBlobSHA256: "", Expected: string(expected)}
	changes := proposalChanges(picked, remove)
	if file != nil {
		proposal.BaseBlobSHA256 = memory.HashHex(file)
	}
	for _, l := range picked {
		proposal.LessonIDs = append(proposal.LessonIDs, l.ID)
	}
	requestID, err := mc.submitMemoryRequest(ctx, proposal, changes, verify, file != nil)
	if err != nil {
		return "", err
	}
	err = store.Update(func(st *memory.StoreState) error {
		for i := range st.Lessons {
			l := &st.Lessons[i]
			for _, p := range picked {
				if l.ID == p.ID && l.Move(memory.StateProposed, mc.stamp(), memoryOperator, "request "+requestID) == nil {
					l.RequestID = requestID
				}
			}
		}
		return nil
	})
	if err != nil {
		return requestID, fmt.Errorf("request %s is submitted, but its lines could not be marked proposed: %w", requestID, err)
	}
	fmt.Fprintln(mc.out, requestID)
	fmt.Fprintf(mc.out, "memory request: %d line(s) added, %d removed. It waits at spec review: read the exact section in its spec, then `factoryd approve %s`; nothing reaches the repository until you merge its pull request.\n", len(picked), len(remove), requestID)
	return requestID, nil
}

// submitMemoryRequest claims the request's id, saves the proposal and its
// list of changes under it and submits the request through the entry point
// `factoryd submit -spec-file -plan-dir` uses. A failed submit leaves no
// proposal, no list and no claimed id.
func (mc *memoryCmd) submitMemoryRequest(ctx context.Context, proposal memory.Proposal, changes []memory.Change, verify string, fileExists bool) (string, error) {
	requestID, err := request.ClaimID(mc.dataDir, request.GenerateID("memory "+mc.project, mc.now))
	if err != nil {
		return "", fmt.Errorf("claim a request id: %w", err)
	}
	proposal.RequestID = requestID
	proposalPath, err := memory.ProposalPath(mc.dataDir, mc.storeKey(), requestID)
	changesPath, _ := memory.ChangesPath(mc.dataDir, mc.storeKey(), requestID)
	undo := func() {
		for _, path := range []string{proposalPath, changesPath} {
			if path != "" {
				_ = os.Remove(path)
			}
		}
		_ = os.RemoveAll(request.Dir(mc.dataDir, requestID))
	}
	if err != nil {
		undo()
		return "", err
	}
	if err := memory.SaveProposal(proposalPath, proposal); err != nil {
		undo()
		return "", err
	}
	if err := memory.SaveChanges(changesPath, changes); err != nil {
		undo()
		return "", err
	}
	docs := memoryRequestDocuments(proposal, changes, verify, fileExists)
	scratch, err := os.MkdirTemp("", "factoryd-memory-request-")
	if err != nil {
		undo()
		return "", fmt.Errorf("write the request's spec and plan: %w", err)
	}
	defer os.RemoveAll(scratch)
	specFile, planDir := filepath.Join(scratch, "spec.md"), filepath.Join(scratch, "plan")
	if err := writeMemoryRequestFiles(specFile, planDir, docs); err != nil {
		undo()
		return "", err
	}
	tokens, cost := mc.settings.EffectiveRelayCeilings()
	_, err = submitRequest(mc.dp, ctx, submitParams{
		workspaceArg:               mc.repoRoot,
		trailingText:               []string{docs.requestText},
		specFile:                   specFile,
		planDir:                    planDir,
		dataDir:                    mc.dataDir,
		sessionTokenCeiling:        int64(tokens),
		sessionCostCeilingMicroUSD: cost,
		settings:                   mc.settings,
		claimedID:                  requestID,
		sourceKind:                 request.SourceMemory,
	})
	if err != nil {
		undo()
		return "", fmt.Errorf("submit the memory request: %w", err)
	}
	return requestID, nil
}

func writeMemoryRequestFiles(specFile, planDir string, docs memoryDocuments) error {
	if err := os.WriteFile(specFile, []byte(docs.spec), 0o600); err != nil {
		return fmt.Errorf("write the request's spec: %w", err)
	}
	if err := os.Mkdir(planDir, 0o700); err != nil {
		return fmt.Errorf("write the request's plan: %w", err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "001.spec.md"), []byte(docs.ticket), 0o600); err != nil {
		return fmt.Errorf("write the request's ticket: %w", err)
	}
	return nil
}
