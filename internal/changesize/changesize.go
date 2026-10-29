// Package changesize measures source changes from git's committed diff output.
package changesize

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/run"
)

// Git supplies the factory's isolated, attribute-free view of a git diff.
type Git interface {
	Numstat(context.Context, string, string) ([]byte, error)
	Patch(context.Context, string, string, string) ([]byte, error)
}

// Measure counts the source paths in git's numstat and the distinct enclosing
// names in each path's zero-context hunk headers. It reads no file contents.
func Measure(ctx context.Context, git Git, base, result string, testPatterns []string) (*run.ChangeSize, error) {
	if len(testPatterns) == 0 {
		testPatterns = policy.DefaultTestPatterns
	}
	data, err := git.Numstat(ctx, base, result)
	if err != nil {
		return nil, err
	}
	size := &run.ChangeSize{Files: []run.ChangeSizeFile{}}
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			return nil, fmt.Errorf("invalid git numstat entry")
		}
		if parts[0] == "-" && parts[1] == "-" || excluded(parts[2], testPatterns) {
			continue
		}
		added, err := strconv.Atoi(parts[0])
		if err != nil || added < 0 {
			return nil, fmt.Errorf("invalid git numstat insertion count")
		}
		deleted, err := strconv.Atoi(parts[1])
		if err != nil || deleted < 0 {
			return nil, fmt.Errorf("invalid git numstat deletion count")
		}
		patch, err := git.Patch(ctx, base, result, parts[2])
		if err != nil {
			return nil, err
		}
		functions := hunkNames(string(patch))
		size.Files = append(size.Files, run.ChangeSizeFile{Path: parts[2], Insertions: added, Deletions: deleted, Functions: functions})
		size.SourceFiles++
		size.SourceLines += added + deleted
		size.Functions += len(functions)
	}
	return size, nil
}

func excluded(name string, tests []string) bool {
	if policy.IsTestFile(name, tests) || len(evidence.DependencyLockfilesTouched([]string{name})) > 0 {
		return true
	}
	lower := strings.ToLower(name)
	if strings.Contains("/"+lower, "/docs/") {
		return true
	}
	for _, ext := range []string{".md", ".markdown", ".rst", ".txt", ".adoc", ".asciidoc", ".org", ".tex"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	base := path.Base(lower)
	for _, suffix := range []string{".pb.go", ".g.dart", ".gen.go", "_gen.go", ".generated.go", ".generated.ts", ".generated.js", ".designer.cs", ".min.js", ".min.css"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func hunkNames(patch string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		i := strings.Index(line[3:], " @@")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[3+i+3:])
		if name != "" {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
