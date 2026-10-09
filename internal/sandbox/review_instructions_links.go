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
// same in both trees, its text plain, and what it points to either covered by
// the candidate rules (a table path) or identical in both trees: a mask can
// give a review the base version of a table path, not of anything else.
func (s *planState) checkLinks(ctx context.Context, root string) error {
	paths := make([]string, 0, len(s.links))
	for p := range s.links {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
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
	return nil
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

// checkLinkTarget refuses a target through another link or changed between
// the trees. A target outside the table becomes a verified root: the worktree
// must hold exactly the result tree at and under it.
func (s *planState) checkLinkTarget(link, target string) error {
	fold := foldComponents(strings.Split(target, "/"))
	for k := range fold {
		prefix := strings.Join(fold[:k+1], "/")
		if s.base.isLink(prefix) || s.res.isLink(prefix) {
			return fmt.Errorf("review instructions: %s is a link through %s, which is itself a symlink", link, strings.Join(strings.Split(target, "/")[:k+1], "/"))
		}
	}
	if _, _, ok := matchInstructionPath(strings.Split(target, "/")); ok {
		return nil
	}
	f := strings.Join(fold, "/")
	if !sameEntries(s.base.foldedUnder(f), s.res.foldedUnder(f)) {
		return fmt.Errorf("review instructions: %s is a link to %s, which this build changed: a review cannot be given the base version of it", link, target)
	}
	s.roots[f] = target
	return nil
}
