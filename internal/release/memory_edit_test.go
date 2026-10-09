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
		{"nothing changed, no proposal", &run.MemoryEdit{}, ""},
		{"section changed, no proposal", &run.MemoryEdit{SectionChanged: true}, ReasonMemorySectionNotMemoryChange},
		{"proposal and exact match", &run.MemoryEdit{SectionChanged: true, Proposal: true, Matches: true}, ""},
		{"proposal, file differs", &run.MemoryEdit{SectionChanged: true, Proposal: true}, ReasonMemoryChangeNotApproved},
		{"proposal, build did nothing", &run.MemoryEdit{Proposal: true}, ReasonMemoryChangeNotApproved},
		{"proposal, match plus another file", &run.MemoryEdit{SectionChanged: true, Proposal: true, Matches: true, OtherFilesChanged: []string{"README.md"}}, `README.md`},
		{"fail closed", &run.MemoryEdit{Error: "git exploded", FailClosed: true}, "could not be completed: git exploded"},
		{"error that is no evidence", &run.MemoryEdit{Error: "git exploded"}, ""},
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
