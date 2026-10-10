package sandbox

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// checkLinks applies the link rules to every symlink at, under, or exactly
// above a fixed path ("relevant" links), in either tree. A link must be the
// same in both trees, its text plain, and its target either at or under a
// table path (the candidate rules mask, verify and clean it) or one regular
// file that both trees hold identically (verified on disk, never removed): a
// mask can give a review the base version of a table path, not of anything
// else, and nothing here can verify a directory outside the table.
func (s *planState) checkLinks(ctx context.Context, root string) error {
	paths := make([]string, 0, len(s.links))
	for p := range s.links {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		pair := s.links[p]
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] {
			return fmt.Errorf("review instructions: symlink %s at an instruction path is not the same in both trees", p)
		}
		text, err := readBlob(ctx, root, pair[0].oid, maxReviewInstructionLinkBytes)
		if err != nil {
			return err
		}
		target, err := resolveLinkTarget(p, string(text))
		if err != nil {
			return fmt.Errorf("review instructions: %s: %w", p, err)
		}
		if err := s.checkLinkTarget(p, target); err != nil {
			return err
		}
	}
	return s.checkFileTargets(ctx, root)
}

// resolveLinkTarget returns the workspace-relative path the link at rel with
// the given text points to, or an error for text that is not plain.
func resolveLinkTarget(rel, text string) (string, error) {
	if text == "" || strings.HasPrefix(text, "/") || mountUnsafe(text) {
		return "", fmt.Errorf("link text %q is not a plain relative path", text)
	}
	leading := true
	for _, c := range strings.Split(text, "/") {
		switch {
		case c == "" || c == ".":
			return "", fmt.Errorf("link text %q has an empty or . component", text)
		case c != "..":
			leading = false
		case !leading:
			return "", fmt.Errorf("link text %q has a .. that is not leading", text)
		}
	}
	target := path.Join(path.Dir(rel), text)
	if target == "." || target == ".." || strings.HasPrefix(target, "../") {
		return "", fmt.Errorf("link text %q points at the workspace root or outside it", text)
	}
	for _, c := range strings.Split(target, "/") {
		if foldName(c) == foldedGit {
			return "", fmt.Errorf("link text %q points into .git", text)
		}
	}
	return target, nil
}

// checkLinkTarget refuses a target through another link or into a submodule,
// accepts one at or under the table, and queues any other for checkFileTargets.
func (s *planState) checkLinkTarget(link, target string) error {
	parts := strings.Split(target, "/")
	fold := foldComponents(parts)
	folded := strings.Join(fold, "/")
	end, spellEnd := -1, -1 // bytes of folded and of target that the first k+1 components span
	for k := range fold {
		end, spellEnd = end+1+len(fold[k]), spellEnd+1+len(parts[k])
		if !s.base.foldLens[end] && !s.res.foldLens[end] {
			continue // no symlink or submodule of either tree has a path this long
		}
		prefix := folded[:end]
		if s.base.isLink(prefix) || s.res.isLink(prefix) {
			return fmt.Errorf("review instructions: %s is a link through %s, which is itself a symlink", link, target[:spellEnd])
		}
		if s.base.gitFold[prefix] || s.res.gitFold[prefix] {
			return fmt.Errorf("review instructions: %s is a link into the submodule %s: a review cannot verify it", link, target[:spellEnd])
		}
	}
	if _, _, ok := matchInstructionPath(parts); ok {
		return nil
	}
	if _, ok := s.targets[target]; !ok {
		s.targets[target] = link
		return s.keepTarget(parts)
	}
	return nil
}

// keepTarget counts a queued link target as register counts a recorded path:
// its directories against maxReviewInstructionRecordedDirs, and its path and
// every directory prefix above it, which the worktree check spells out,
// against maxReviewInstructionKeptBytes of the result commit.
func (s *planState) keepTarget(parts []string) error {
	bytes, end := 0, -1
	for _, p := range parts {
		end += 1 + len(p)
		bytes += end
	}
	if s.recDirs[1] += len(parts) - 1; s.recDirs[1] > maxReviewInstructionRecordedDirs {
		return fmt.Errorf("review instructions: more than %d directories that lead to or lie under an instruction path in commit %s", maxReviewInstructionRecordedDirs, s.shas[1])
	}
	return s.keep(1, bytes)
}

// checkFileTargets reads each tree once more, keeping only the queued target
// paths, and requires every target to be one blob (a regular file) with the
// same id and mode in both trees. The result's entry is then verified on disk
// with the table's tracked entries. What it keeps is one entry and one
// directory name for each queued target, each as long as the target itself.
func (s *planState) checkFileTargets(ctx context.Context, root string) error {
	if len(s.targets) == 0 {
		return nil
	}
	lens := map[int]bool{} // the lengths of the queued targets: only a prefix that long can be one
	for t := range s.targets {
		lens[len(t)] = true
	}
	var found [2]map[string]treeEntry
	var dirs [2]map[string]bool
	for side, sha := range s.shas {
		found[side], dirs[side] = map[string]treeEntry{}, map[string]bool{}
		err := streamTree(ctx, root, sha, func(e treeEntry) error {
			if _, ok := s.targets[e.path]; lens[len(e.path)] && ok {
				found[side][e.path] = e
				s.retained++
			}
			for k := 0; k < len(e.path); k++ {
				if e.path[k] == '/' && lens[k] {
					if _, ok := s.targets[e.path[:k]]; ok && !dirs[side][e.path[:k]] {
						dirs[side][strings.Clone(e.path[:k])] = true // not a slice of the record, which may be a megabyte
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	targets := make([]string, 0, len(s.targets))
	for t := range s.targets {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		link := s.targets[t]
		b, hasB := found[0][t]
		r, hasR := found[1][t]
		switch {
		case dirs[0][t] || dirs[1][t]:
			return fmt.Errorf("review instructions: %s links an instruction path to the directory %s, which is not an instruction path: a review cannot verify it", link, t)
		case !hasB && !hasR:
			return fmt.Errorf("review instructions: %s links an instruction path to %s, which neither commit holds", link, t)
		case !hasB || !hasR || b != r:
			return fmt.Errorf("review instructions: %s is a link to %s, which this build changed: a review cannot be given the base version of it", link, t)
		case r.mode != "100644" && r.mode != "100755":
			return fmt.Errorf("review instructions: %s links an instruction path to %s, which is not a regular file", link, t)
		}
		s.tracked = append(s.tracked, r)
	}
	return nil
}
