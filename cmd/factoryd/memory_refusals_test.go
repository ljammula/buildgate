package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

const memoryEscape = "\x1b[31m"

// A section line at HEAD that holds a terminal escape sequence is not read:
// `list` prints none of it and says the section must be fixed by hand, and
// `propose` refuses.
func TestMemoryListPrintsNoControlCharacterFromTheRepository(t *testing.T) {
	f := newMemFix(t, map[string]string{"AGENTS.md": "# Guide\n"})
	own := f.add("Use go 1.26")
	f.commitAgents(sectionFile("# Guide\n\n", "", "- A good line.", "- A line "+memoryEscape+"in red."))
	if err := f.cmd().list(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if strings.ContainsAny(out, "\x1b\u009b") || !strings.Contains(out, "unreadable section: fix AGENTS.md by hand") {
		t.Fatalf("list output:\n%q", out)
	}
	if _, err := f.propose(nil, own.ID); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("propose on an unreadable section: err = %v, want a refusal without the escape", err)
	}
	if _, err := f.propose([]string{"- A line " + memoryEscape + "in red."}); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("propose -remove of the line: err = %v, want a refusal without the escape", err)
	}
}

// Whatever the store or a saved list of changes holds, `show`, the API view
// and the spec, ticket and pull-request text print no control character.
func TestMemorySurfacesPrintNoControlCharacter(t *testing.T) {
	f := newMemFix(t, map[string]string{"AGENTS.md": "# Guide\n"})
	store, err := memory.Open(f.data, f.key())
	if err != nil {
		t.Fatal(err)
	}
	bad := memory.Lesson{ID: "abcdef0123456789", Line: "- stored " + memoryEscape + "line.", Source: memory.SourceAgent + memoryEscape, State: memory.StateCandidate,
		Runs: []string{"run-" + memoryEscape}, History: []memory.Transition{{From: memory.StateDropped, To: memory.StateCandidate, At: "now" + memoryEscape, By: "x" + memoryEscape, Reason: "y" + memoryEscape}}}
	if err := store.Update(func(st *memory.StoreState) error {
		st.Lessons = append(st.Lessons, bad)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.cmd().show([]string{bad.ID}); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); strings.Contains(out, "\x1b") || !strings.Contains(out, "- stored") {
		t.Fatalf("show output:\n%q", out)
	}
	ro, err := readMemory(context.Background(), f.dp, f.settings(), f.data, f.root, f.project)
	if err != nil {
		t.Fatal(err)
	}
	ro.section.Lines = []string{"- in force " + memoryEscape + "line."}
	view := ro.view(f.project)
	if len(view.InForce) != 1 || len(view.Candidates) != 1 || strings.Contains(view.InForce[0]+view.Candidates[0].Line+view.Candidates[0].Source, "\x1b") {
		t.Fatalf("view = %+v", view)
	}
	changes := []memory.Change{{Line: "- added " + memoryEscape + "line.", Source: memory.SourceAgent, Runs: []string{"run-1"}}, {Remove: true, Line: "- removed " + memoryEscape + "line."}}
	docs := memoryRequestDocuments(memory.Proposal{Expected: "# Guide\n"}, changes, "make test", true)
	for name, text := range map[string]string{"spec": docs.spec, "ticket": docs.ticket, "request": docs.requestText, "pull request": memoryPullRequestSection(changes)} {
		if strings.Contains(text, "\x1b") {
			t.Errorf("%s holds an escape character:\n%q", name, text)
		}
	}
}

// A repository with no section whose AGENTS.md ends inside a code fence: a
// section appended to it could never be read back, so `propose` refuses and
// says what to do.
func TestMemoryProposeRefusesAFileThatEndsInsideACodeFence(t *testing.T) {
	f := newMemFix(t, map[string]string{"AGENTS.md": "# Guide\n"})
	own := f.add("Use go 1.26")
	f.commitAgents("# Guide\n\n```sh\nmake test\n")
	_, err := f.propose(nil, own.ID)
	if err == nil || !strings.Contains(err.Error(), "close the fence by hand") {
		t.Fatalf("propose: err = %v, want a refusal naming the open fence", err)
	}
	if files := proposalFiles(t, f); len(files) != 0 {
		t.Fatalf("a refused propose left %v", files)
	}
}

// commitAgents commits text as the fixture's root AGENTS.md at HEAD.
func (f *memFix) commitAgents(text string) {
	f.t.Helper()
	f.repo.commit(map[string]*string{"AGENTS.md": sp(text)})
}

// `memory list` says a proposed request is stale once AGENTS.md at HEAD is
// neither the file it was rendered from nor the file it proposes.
func TestMemoryListMarksAStaleProposal(t *testing.T) {
	f, id, _, _ := proposedMemoryRequest(t)
	list := func() string {
		t.Helper()
		if err := f.cmd().list(context.Background()); err != nil {
			t.Fatal(err)
		}
		return f.out.String()
	}
	if out := list(); strings.Contains(out, "stale") {
		t.Fatalf("an unchanged base is listed as stale:\n%s", out)
	}
	expected := f.proposal(id).Expected
	f.commitAgents(sectionFile("# Guide\n\nEdited by hand.\n\n", "", "- An old line."))
	if out := list(); !strings.Contains(out, "memory request "+id+": stale: propose again") {
		t.Fatalf("list after AGENTS.md changed:\n%s", out)
	}
	f.commitAgents(expected) // its pull request merged
	if out := list(); strings.Contains(out, "stale") {
		t.Fatalf("a merged proposal is listed as stale:\n%s", out)
	}
}

// The worker halts a memory request whose AGENTS.md changed at HEAD since it
// was proposed, before a build is spent on it.
func TestStaleMemoryRequestIsHaltedBeforeItsBuild(t *testing.T) {
	f, id, _, _ := proposedMemoryRequest(t)
	building := func() *request.Request {
		t.Helper()
		r, err := request.Load(f.data, id)
		if err != nil {
			t.Fatal(err)
		}
		r.State = request.StateBuilding
		if err := r.Save(f.data); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := building()
	if reason := staleMemoryRequest(context.Background(), f.dp, f.data, r); reason != "" {
		t.Fatalf("an unchanged base: reason = %q, want none", reason)
	}
	ordinary := request.New("req-ordinary", f.root, f.project, request.Source{}, f.cmd().now)
	if reason := staleMemoryRequest(context.Background(), f.dp, f.data, ordinary); reason != "" {
		t.Fatalf("an ordinary request: reason = %q, want none", reason)
	}
	f.commitAgents(sectionFile("# Guide\n\nEdited by hand.\n\n", "", "- An old line."))
	acts := &requestActivities{dp: f.dp, dataDir: f.data, buildRunner: failingBuildRunner(t)}
	after, err := acts.AdvanceRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	halted, err := request.Load(f.data, id)
	if err != nil {
		t.Fatal(err)
	}
	if after != string(request.StateHalted) || halted.State != request.StateHalted || halted.Error != release.ReasonMemoryBaseMoved {
		t.Fatalf("after = %q state = %s error = %q, want halted with the stale reason", after, halted.State, halted.Error)
	}
	// A proposal that cannot be read is refused too, with its own sentence.
	f.commitAgents(sectionFile("# Guide\n\n", "", "- An old line."))
	r = building()
	path, _ := memory.ProposalPath(f.data, f.key(), id)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if reason := staleMemoryRequest(context.Background(), f.dp, f.data, r); reason != reasonMemoryDispatchUnchecked {
		t.Fatalf("no proposal file: reason = %q", reason)
	}
}

// Notes are collected from the runs of this repository only: a run of another
// checkout with the same base name, or one that records no repository, gives
// this store no candidate.
func TestMemoryCollectsNotesOnlyFromRunsOfThisRepository(t *testing.T) {
	f := newMemFix(t, nil)
	other := f.quarantinedRunWithNotes("run-other", worthKnowing("Use the other checkout's tool"))
	other.RepositoryRoot = filepath.Join(filepath.Dir(f.root), "elsewhere", f.project)
	unrecorded := f.quarantinedRunWithNotes("run-unrecorded", worthKnowing("Recorded by no repository"))
	unrecorded.RepositoryRoot = ""
	for _, r := range []*run.Run{other, unrecorded} {
		if err := r.Save(f.data); err != nil {
			t.Fatal(err)
		}
	}
	f.quarantinedRunWithNotes("run-mine", worthKnowing("Use go 1.26"))
	if err := f.cmd().list(context.Background()); err != nil {
		t.Fatal(err)
	}
	ls := f.lessons()
	if len(ls) != 1 || ls[0].Line != "- Use go 1.26." || strings.Join(ls[0].Runs, ",") != "run-mine" {
		t.Fatalf("lessons = %+v, want only this repository's note", ls)
	}
}
