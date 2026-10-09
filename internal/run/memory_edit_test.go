package run

import (
	"reflect"
	"testing"
)

func TestRootInstructionNameFoldsTheFirstPathComponent(t *testing.T) {
	names := map[string]string{
		"AGENTS.md":                        "AGENTS.md",
		"agents.md":                        "agents.md",
		"Agents.MD":                        "Agents.MD",
		"AGENT\u017f.md":                   "AGENT\u017f.md",  // long s folds to S
		"AGENTS\u200c.md":                  "AGENTS\u200c.md", // ignored by HFS+ in a name
		string(rune(0xfeff)) + "AGENTS.md": string(rune(0xfeff)) + "AGENTS.md",
		"agents.md/inside.txt":             "agents.md",
		"docs/AGENTS.md":                   "",
		"AGENTS.md.bak":                    "",
		"AGENTS.override.md":               "",
		"xAGENTS.md":                       "",
		"":                                 "",
	}
	for path, want := range names {
		if got := RootInstructionName(path); got != want {
			t.Errorf("RootInstructionName(%q) = %q, want %q", path, got, want)
		}
	}
	got := ChangedRootInstructionNames([]string{"README.md", "agents.md", "AGENTS.md", "agents.md", "sub/AGENTS.md"})
	if want := []string{"agents.md", "AGENTS.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRootInstructionNames = %q, want %q", got, want)
	}
}

// Cleaning bounds what is recorded and never empties a list the release
// policy decides on, nor drops the second of two result names.
func TestMemoryEditCleanKeepsWhatThePolicyCounts(t *testing.T) {
	unprintable := "\x1b[0m\x07"
	m := &MemoryEdit{
		ChangedRootNames:  []string{unprintable},
		ResultRootNames:   []string{"AGENTS.md", unprintable},
		MarkerIn:          []string{unprintable},
		NotRegularFile:    []string{unprintable},
		OtherFilesChanged: []string{unprintable},
		Error:             unprintable,
	}
	m.Clean()
	if len(m.ChangedRootNames) != 1 || len(m.MarkerIn) != 1 || len(m.NotRegularFile) != 1 || len(m.OtherFilesChanged) != 1 {
		t.Fatalf("a list was emptied: %+v", m)
	}
	if len(m.ResultRootNames) != 2 {
		t.Fatalf("result names = %q, want two", m.ResultRootNames)
	}
	if m.Error == "" {
		t.Fatal("the error was emptied")
	}
	var none *MemoryEdit
	none.Clean()
}
