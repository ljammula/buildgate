package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// linkChecker decides whether a symlink in the working tree is the base's own,
// unchanged link, and whether what it points to is unchanged. A link passes
// only when every one of these holds, and anything else is an error:
//   - the base has a link at the same exact path with the same text;
//   - the text is relative and plain: no empty or "." component, ".." only as
//     leading components, nothing a mount argument cannot carry;
//   - the target stays inside the workspace and is not .git or below it;
//   - no component of the target is a symlink, in the working tree or in the
//     base (no chains);
//   - the target is covered by the table rules or equals the base by bytes and
//     executable bit.
type linkChecker struct {
	ctx    context.Context
	root   string
	base   *baseTree
	budget *instrBudget
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
	if foldPath(strings.SplitN(target, "/", 2)[0]) == foldedGit {
		return "", fmt.Errorf("link text %q points into .git", text)
	}
	return target, nil
}

// check applies the rules to the symlink at rel in the working tree.
func (c *linkChecker) check(rel string) error {
	bl, ok := c.base.links[foldPath(rel)]
	if !ok || bl.name != rel {
		return fmt.Errorf("review instructions: %s is a symlink in the working tree that the base does not have", rel)
	}
	text, err := os.Readlink(filepath.Join(c.root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("review instructions: read symlink %s: %w", rel, err)
	}
	baseText, err := readBlob(c.ctx, c.root, instrFile{sha: bl.sha})
	if err != nil {
		return err
	}
	if string(baseText) != text {
		return fmt.Errorf("review instructions: %s is a symlink the build retargeted (base %q, now %q)", rel, baseText, text)
	}
	target, err := resolveLinkTarget(rel, text)
	if err != nil {
		return fmt.Errorf("review instructions: %s: %w", rel, err)
	}
	if err := c.refuseLinkedComponents(rel, target); err != nil {
		return err
	}
	if _, _, covered := matchInstructionPath(strings.Split(target, "/")); covered {
		return nil
	}
	return c.compareTarget(rel, target)
}

// refuseLinkedComponents refuses a link whose own directory chain, or whose
// target path, passes through a symlink in the working tree or in the base.
func (c *linkChecker) refuseLinkedComponents(rel, target string) error {
	for _, p := range []string{path.Dir(rel), target} {
		if p == "." {
			continue
		}
		cur := ""
		for _, part := range strings.Split(p, "/") {
			cur = path.Join(cur, part)
			if err := c.refuseLinkAt(rel, cur); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *linkChecker) refuseLinkAt(rel, cur string) error {
	if _, ok := c.base.links[foldPath(cur)]; ok {
		return fmt.Errorf("review instructions: %s is a link through %s, which is a symlink at base", rel, cur)
	}
	info, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(cur)))
	if missingPath(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("review instructions: inspect %s: %w", cur, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("review instructions: %s is a link through %s, which is a symlink in the working tree", rel, cur)
	}
	return nil
}

// walkTarget records the working-tree side of the workspace path target.
func (c *linkChecker) walkTarget(groups map[string]*instrGroup, target string) error {
	n := len(strings.Split(target, "/"))
	abs := filepath.Join(c.root, filepath.FromSlash(target))
	if _, err := os.Lstat(abs); missingPath(err) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := c.budget.visit(); err != nil {
			return err
		}
		r, err := filepath.Rel(c.root, p)
		if err != nil {
			return err
		}
		return recordWorkEntry(groups, c.budget, strings.Split(filepath.ToSlash(r), "/"), n, "", p, d)
	})
}

// compareTarget compares the workspace path target, as the link rel points to
// it, with the base like a candidate: any difference is an error.
func (c *linkChecker) compareTarget(rel, target string) error {
	n := len(strings.Split(target, "/"))
	groups := map[string]*instrGroup{}
	err := c.walkTarget(groups, target)
	for _, e := range c.base.under(target) {
		if err != nil {
			break
		}
		err = recordBaseEntry(groups, c.budget, e, strings.Split(e.name, "/"), n, "", true)
	}
	if err != nil {
		return fmt.Errorf("review instructions: %s is a link to %s: %w", rel, target, err)
	}
	g := groups[foldPath(target)]
	if g == nil {
		return nil
	}
	spelling, kind := sidesDisagree(g)
	changed := spelling || kind
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

// checkTablePaths applies the rules to a symlink at, or above, any fixed table
// path in the working tree.
func (c *linkChecker) checkTablePaths() error {
	for _, p := range append(append([]string(nil), reviewInstructionDirs...), reviewInstructionFiles...) {
		cur := ""
		for _, part := range strings.Split(p, "/") {
			cur = path.Join(cur, part)
			info, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(cur)))
			if missingPath(err) {
				break
			}
			if err != nil {
				return fmt.Errorf("review instructions: inspect %s: %w", cur, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if err := c.check(cur); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}
