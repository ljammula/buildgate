package request

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// originMainAcceptanceCriteria is the small parser that origin/main used for
// the exact section. It is intentionally kept in the test rather than the
// implementation: this compatibility check catches an accidental change to
// the accepted corpus while allowing the production reader to gain its
// derived-section behavior.
func originMainAcceptanceCriteria(content string) ([]string, error) {
	lines := strings.Split(content, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == specAcceptanceCriteriaHeading {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return nil, nil
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}

	var criteria []string
	var current []string
	inFence := false
	flush := func() {
		if len(current) == 0 {
			return
		}
		folded := make([]string, len(current))
		for i, raw := range current {
			folded[i] = strings.TrimSpace(raw)
		}
		criteria = append(criteria, strings.Join(folded, " "))
		current = nil
	}
	for _, raw := range lines[start:end] {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		isFence := strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
		wasInFence := inFence
		if isFence {
			inFence = !inFence
		}
		indented := raw != strings.TrimLeft(raw, " \t")
		if !indented && !wasInFence && !isFence && specCriterionItemRE.MatchString(trimmed) {
			flush()
		}
		if !indented && !wasInFence && !isFence && specCriterionItemRE.MatchString(trimmed) {
			current = []string{raw}
			continue
		}
		if len(current) > 0 {
			current = append(current, raw)
		}
	}
	flush()
	return criteria, nil
}

func TestPlanCorpusAcceptanceCriteriaMatchesOriginMain(t *testing.T) {
	paths := make([]string, 0, 44)
	root := filepath.Join("..", "requestdriver", "testdata", "plan_corpus")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "spec.md" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatal("plan corpus has no spec.md files")
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		want, err := originMainAcceptanceCriteria(string(content))
		if err != nil {
			t.Fatalf("old parser %s: %v", path, err)
		}
		got, err := SpecAcceptanceCriteria(string(content))
		if err != nil {
			t.Fatalf("new parser %s: %v", path, err)
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			limit := len(got)
			if len(want) < limit {
				limit = len(want)
			}
			first := -1
			for i := 0; i < limit; i++ {
				if got[i] != want[i] {
					first = i
					break
				}
			}
			if first >= 0 {
				t.Errorf("%s: criteria changed at %d\n got[%d]: %q\nwant[%d]: %q", path, first, first, got[first], first, want[first])
			} else {
				t.Errorf("%s: criteria changed: got %q\nwant %q", path, got, want)
			}
		}
	}
}
