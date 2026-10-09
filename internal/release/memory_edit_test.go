package release

import (
	"strings"
	"testing"

	"buildgate/internal/run"
)

func memoryReasons(t *testing.T, m *run.MemoryEdit) []string {
	t.Helper()
	r := cleanRun()
	r.MemoryEdit = m
	_, reasons := MergePolicyCheck(r, cleanPolicy())
	return reasons
}

func TestMergePolicyCheckMemoryEditReasons(t *testing.T) {
	cases := []struct {
		name string
		edit *run.MemoryEdit
		want string // "" means no reason at all
	}{
		{"nil evidence", nil, ""},
		{"nothing recorded, no proposal", &run.MemoryEdit{}, ""},
		{"base has a section, a root name changed", &run.MemoryEdit{BaseHasSection: true, ChangedRootNames: []string{"AGENTS.md"}}, ReasonMemorySectionNotMemoryChange + `: ["AGENTS.md"]`},
		{"base has a section, a variant added", &run.MemoryEdit{BaseHasSection: true, ChangedRootNames: []string{"agents.md"}, ResultRootNames: []string{"AGENTS.md", "agents.md"}}, ReasonMemorySectionNotMemoryChange + `: ["agents.md"]`},
		{"base has a section, nothing changed", &run.MemoryEdit{BaseHasSection: true, ResultRootNames: []string{"AGENTS.md"}}, ""},
		{"no section, prose edited", &run.MemoryEdit{ChangedRootNames: []string{"AGENTS.md"}, ResultRootNames: []string{"AGENTS.md"}}, ""},
		{"no section, markers added", &run.MemoryEdit{ChangedRootNames: []string{"AGENTS.md"}, ResultRootNames: []string{"AGENTS.md"}, MarkerIn: []string{"AGENTS.md"}}, ReasonMemoryMarkersAdded + `: ["AGENTS.md"]`},
		{"no section, a second spelling", &run.MemoryEdit{ChangedRootNames: []string{"agents.md"}, ResultRootNames: []string{"AGENTS.md", "agents.md"}}, ReasonSeveralRootInstructionNames},
		{"no section, changed into a link", &run.MemoryEdit{ChangedRootNames: []string{"AGENTS.md"}, ResultRootNames: []string{"AGENTS.md"}, NotRegularFile: []string{"AGENTS.md"}}, ReasonRootInstructionNotRegular},
		{"proposal and exact match", &run.MemoryEdit{BaseHasSection: true, ChangedRootNames: []string{"AGENTS.md"}, MarkerIn: []string{"AGENTS.md"}, Proposal: true, Matches: true}, ""},
		{"proposal, file differs", &run.MemoryEdit{ChangedRootNames: []string{"AGENTS.md"}, Proposal: true}, ReasonMemoryChangeNotApproved},
		{"proposal, build did nothing", &run.MemoryEdit{Proposal: true}, ReasonMemoryChangeNotApproved},
		{"proposal, match plus another file", &run.MemoryEdit{Proposal: true, Matches: true, OtherFilesChanged: []string{"README.md"}}, `README.md`},
		{"an error denies", &run.MemoryEdit{Error: "git exploded"}, "could not be completed: git exploded"},
		{"an error denies a memory run that matches", &run.MemoryEdit{Proposal: true, Matches: true, Error: "git exploded"}, "could not be completed: git exploded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reasons := memoryReasons(t, c.edit)
			if c.want == "" {
				if len(reasons) != 0 {
					t.Fatalf("reasons = %v, want none", reasons)
				}
				return
			}
			if len(reasons) != 1 || !strings.Contains(reasons[0], c.want) {
				t.Fatalf("reasons = %v, want one containing %q", reasons, c.want)
			}
		})
	}
}

func TestMergePolicyCheckMemoryEditNamesAtMostThreeOtherFiles(t *testing.T) {
	reasons := memoryReasons(t, &run.MemoryEdit{Proposal: true, Matches: true, OtherFilesChanged: []string{"a", "b", "c", "d", "e"}})
	if len(reasons) != 1 || !strings.Contains(reasons[0], `"a" "b" "c"`) || strings.Contains(reasons[0], `"d"`) || !strings.Contains(reasons[0], "and 2 more") {
		t.Fatalf("reasons = %v", reasons)
	}
}
