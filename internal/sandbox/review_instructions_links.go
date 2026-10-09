package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// maxReviewInstructionLinkHops bounds the links followed from one link.
const maxReviewInstructionLinkHops = 8

// linkChecker decides whether a symlink in the working tree is the base's own
// unchanged link. Such a link is not itself masked; where it points is
// covered by the normal rules (a table path), is outside the workspace, or is
// compared to the base by bytes. Any other symlink is an error.
type linkChecker struct {
	ctx     context.Context
	root    string
	baseSHA string
	links   map[string]string // base symlink path -> blob id of its text
}

// check applies the rule to the symlink at rel, hops links into a chain.
func (c *linkChecker) check(rel string, hops int) error {
	if hops > maxReviewInstructionLinkHops {
		return fmt.Errorf("review instructions: %s is a symlink in a chain of more than %d links", rel, maxReviewInstructionLinkHops)
	}
	sha, inBase := c.links[rel]
	if !inBase {
		return fmt.Errorf("review instructions: %s is a symlink in the working tree that the base does not have", rel)
	}
	text, err := os.Readlink(filepath.Join(c.root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("review instructions: read symlink %s: %w", rel, err)
	}
	baseText, err := readBlob(c.ctx, c.root, instrFile{sha: sha})
	if err != nil {
		return err
	}
	if string(baseText) != text {
		return fmt.Errorf("review instructions: %s is a symlink the build retargeted (base %q, now %q)", rel, baseText, text)
	}
	return c.followTarget(rel, text, hops)
}

// followTarget handles where the unchanged link rel with text points.
func (c *linkChecker) followTarget(rel, text string, hops int) error {
	if filepath.IsAbs(text) {
		return nil
	}
	target := path.Join(path.Dir(rel), filepath.ToSlash(text))
	if target == ".." || strings.HasPrefix(target, "../") {
		return nil
	}
	if target == "." {
		return fmt.Errorf("review instructions: %s is a link to the workspace root", rel)
	}
	parts := strings.Split(target, "/")
	if _, covered := matchInstructionPath(parts); covered {
		return nil
	}
	cur := ""
	for _, part := range parts {
		cur = path.Join(cur, part)
		info, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(cur)))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			break
		}
		if err != nil {
			return fmt.Errorf("review instructions: inspect %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return c.check(cur, hops+1)
		}
	}
	return c.compareTarget(rel, target)
}

// compareTarget compares the workspace path target, as the link rel points to
// it, with the base by bytes.
func (c *linkChecker) compareTarget(rel, target string) error {
	parts := strings.Split(target, "/")
	groups := map[string]*instrGroup{}
	budget := &instrBudget{}
	abs := filepath.Join(c.root, filepath.FromSlash(target))
	if _, err := os.Lstat(abs); err == nil {
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			r, err := filepath.Rel(c.root, p)
			if err != nil {
				return err
			}
			return recordWorkEntry(groups, budget, strings.Split(filepath.ToSlash(r), "/"), len(parts), p, d)
		})
		if err != nil {
			return fmt.Errorf("review instructions: %s is a link to %s: %w", rel, target, err)
		}
	}
	err := scanBaseTree(c.ctx, c.root, c.baseSHA, func(rec string) error {
		return recordBaseRecord(rec, groups, budget, nil, len(parts))
	}, target)
	if err != nil {
		return fmt.Errorf("review instructions: %s is a link to %s: %w", rel, target, err)
	}
	g := groups[strings.ToLower(target)]
	if g == nil {
		return nil
	}
	changed := g.work.kind != kindAbsent && g.base.kind != kindAbsent && g.work.kind != g.base.kind
	if !changed {
		diffs, err := diffGroup(c.ctx, c.root, g, target)
		if err != nil {
			return err
		}
		changed = len(diffs) > 0
	}
	if changed {
		return fmt.Errorf("review instructions: %s is a link to %s, which this build changed: a review cannot be given the base version of it", rel, target)
	}
	return nil
}

// checkTablePaths applies the rule to a symlink at, or above, any fixed
// table path in the working tree.
func (c *linkChecker) checkTablePaths() error {
	for _, p := range append(append([]string(nil), reviewInstructionDirs...), reviewInstructionFiles...) {
		cur := ""
		for _, part := range strings.Split(p, "/") {
			cur = path.Join(cur, part)
			info, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(cur)))
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				break
			}
			if err != nil {
				return fmt.Errorf("review instructions: inspect %s: %w", cur, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if err := c.check(cur, 1); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}
