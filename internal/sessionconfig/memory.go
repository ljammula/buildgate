package sessionconfig

import (
	"fmt"
	"path/filepath"

	"buildgate/internal/memory"
)

// Bounds of the memory.repositories budget keys.
const (
	MemoryBudgetLinesMin = 5
	MemoryBudgetLinesMax = 80
	MemoryBudgetCharsMin = 500
	MemoryBudgetCharsMax = 6000
)

// MemoryConfig is the session config's memory: block.
type MemoryConfig struct {
	Repositories []MemoryRepositoryConfig `yaml:"repositories,omitempty"`
}

// MemoryRepositoryConfig switches repository memory on for one repository.
type MemoryRepositoryConfig struct {
	Path        string `yaml:"path"`
	BudgetLines *int   `yaml:"budget_lines,omitempty"`
	BudgetChars *int   `yaml:"budget_chars,omitempty"`
}

// MemoryRepository is a resolved memory.repositories entry: Path is absolute
// and cleaned, Budget has its defaults applied.
type MemoryRepository struct {
	Path   string
	Budget memory.Budget
}

func resolveMemory(c *MemoryConfig) ([]MemoryRepository, error) {
	if c == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]MemoryRepository, 0, len(c.Repositories))
	for i, r := range c.Repositories {
		where := fmt.Sprintf("memory.repositories[%d]", i)
		if r.Path == "" {
			return nil, fmt.Errorf("%s: path is required", where)
		}
		p, err := expandHome(r.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: expand path %q: %w", where, r.Path, err)
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%s: path %q must be absolute (or start with ~/)", where, r.Path)
		}
		p = filepath.Clean(p)
		if seen[p] {
			return nil, fmt.Errorf("%s: path %q is listed twice", where, r.Path)
		}
		seen[p] = true
		b, err := resolveMemoryBudget(where, r)
		if err != nil {
			return nil, err
		}
		out = append(out, MemoryRepository{Path: p, Budget: b})
	}
	return out, nil
}

func resolveMemoryBudget(where string, r MemoryRepositoryConfig) (memory.Budget, error) {
	b := memory.Budget{Lines: memory.DefaultBudgetLines, Chars: memory.DefaultBudgetChars}
	if r.BudgetLines != nil {
		if *r.BudgetLines < MemoryBudgetLinesMin || *r.BudgetLines > MemoryBudgetLinesMax {
			return b, fmt.Errorf("%s: budget_lines must be %d to %d, got %d", where, MemoryBudgetLinesMin, MemoryBudgetLinesMax, *r.BudgetLines)
		}
		b.Lines = *r.BudgetLines
	}
	if r.BudgetChars != nil {
		if *r.BudgetChars < MemoryBudgetCharsMin || *r.BudgetChars > MemoryBudgetCharsMax {
			return b, fmt.Errorf("%s: budget_chars must be %d to %d, got %d", where, MemoryBudgetCharsMin, MemoryBudgetCharsMax, *r.BudgetChars)
		}
		b.Chars = *r.BudgetChars
	}
	return b, nil
}

// canonicalRoot cleans p and resolves symlinks when p exists.
func canonicalRoot(p string) string {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// MemoryFor returns the budget of the memory.repositories entry whose path is
// repoRoot, and true; memory is off for every repository not listed. The key
// is the root path, never its base name: two repositories can share one.
func (s Settings) MemoryFor(repoRoot string) (memory.Budget, bool) {
	if repoRoot == "" {
		return memory.Budget{}, false
	}
	want := canonicalRoot(repoRoot)
	for _, r := range s.Memory {
		if canonicalRoot(r.Path) == want {
			return r.Budget, true
		}
	}
	return memory.Budget{}, false
}
