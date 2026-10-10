package sandbox

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The plan of a review-instruction snapshot: both commits' listings indexed
// against the table under the retained-entry, recorded-directory and kept-byte
// budgets, the one spelling of every prefix, the candidates (the outermost
// table entry covering a set of paths), which of them the two trees hold
// differently, and the check of the directories above each mask.

// candidate is the outermost table entry covering a set of paths, with what
// each tree holds at or under it.
type candidate struct {
	canon, spelled string
	base, res      []treeEntry
}

type planState struct {
	base, res *gitTree
	spell     map[string]string  // folded prefix -> spelling, for relevant prefixes of both trees
	resDirs   map[string]bool    // directories of relevant result paths
	dirsDone  [2]map[string]bool // directories register recorded, for each commit
	cands     map[string]*candidate
	links     map[string]*[2]*treeEntry // relevant symlinks: base, result
	tracked   []treeEntry               // relevant result entries, to verify on disk
	targets   map[string]string         // file a link points to outside the table -> the first link naming it
	staged    int64                     // bytes written under dst so far
	retained  int                       // entries kept from the listings, for a test
	sideKept  [2]int                    // entries kept from each commit's listing, bounded by maxReviewInstructionRetained
	recDirs   [2]int                    // directories register recorded from each commit, bounded by maxReviewInstructionRecordedDirs
	keptBytes [2]int                    // bytes of path text kept from each commit, bounded by maxReviewInstructionKeptBytes
	shas      [2]string                 // the base and result commits
	reader    *blobReader               // the one git process every blob is read through
}

func newPlan() *planState {
	return &planState{base: newGitTree(), res: newGitTree(), spell: map[string]string{}, resDirs: map[string]bool{}, cands: map[string]*candidate{}, links: map[string]*[2]*treeEntry{}, targets: map[string]string{}, dirsDone: [2]map[string]bool{{}, {}}}
}

// load streams one commit's listing into the plan.
func (s *planState) load(ctx context.Context, root, sha string, side int) error {
	s.shas[side] = sha
	t := s.base
	if side == 1 {
		t = s.res
	}
	return streamTree(ctx, root, sha, func(e treeEntry) error { return s.index(t, e, side) })
}

// keep counts n more bytes of path text kept from one commit's listing.
func (s *planState) keep(side, n int) error {
	if s.keptBytes[side] += n; s.keptBytes[side] > maxReviewInstructionKeptBytes {
		return fmt.Errorf("review instructions: more than %d bytes of instruction-path, link or submodule paths in commit %s", maxReviewInstructionKeptBytes, s.shas[side])
	}
	return nil
}

// register records the spelling of every prefix that leads to or lies under a
// table match; two spellings of one folded prefix are an error. A directory is
// recorded once for each commit: an entry whose directory was recorded costs
// one lookup, whatever its depth, and a new directory only the components
// below its nearest recorded ancestor. What one commit adds is bounded: more
// than maxReviewInstructionRecordedDirs directories is an error, and the path
// of an entry that records something and every directory recorded count
// against maxReviewInstructionKeptBytes.
func (s *planState) register(ip instrPath, path string, side int) error {
	top := ip.lead
	if ip.n > 0 {
		top = len(ip.parts)
	}
	dirs := min(top, len(ip.parts)-1) // the leading components that are directories
	dirEnd := dirs - 1                // bytes of path they span, with their separators
	for _, part := range ip.parts[:dirs] {
		dirEnd += len(part)
	}
	done := s.dirsDone[side]
	if dirs > 0 && done[path[:dirEnd]] && dirs == top {
		return nil
	}
	start := 0 // components already recorded
	for k, end := dirs, dirEnd; k > 0 && start == 0; k-- {
		if done[path[:end]] {
			start = k
		}
		end -= len(ip.parts[k-1]) + 1
	}
	folded := strings.Join(ip.fold[:top], "/")
	if err := s.keep(side, len(path)+len(folded)); err != nil {
		return err
	}
	spellEnd, foldEnd := -1, -1
	for k := 0; k < top; k++ {
		spellEnd, foldEnd = spellEnd+1+len(ip.parts[k]), foldEnd+1+len(ip.fold[k])
		if k < start {
			continue
		}
		spelled := path[:spellEnd]
		if prev, ok := s.spell[folded[:foldEnd]]; ok && prev != spelled {
			return fmt.Errorf("review instructions: %q and %q are one path on a case-insensitive host", prev, spelled)
		}
		s.spell[folded[:foldEnd]] = spelled
		if k < dirs {
			if s.recDirs[side]++; s.recDirs[side] > maxReviewInstructionRecordedDirs {
				return fmt.Errorf("review instructions: more than %d directories that lead to or lie under an instruction path in commit %s", maxReviewInstructionRecordedDirs, s.shas[side])
			}
			if err := s.keep(side, len(spelled)); err != nil {
				return err
			}
			done[spelled] = true
			if side == 1 {
				s.resDirs[spelled] = true
			}
		}
	}
	return nil
}

func (s *planState) index(t *gitTree, e treeEntry, side int) error {
	ip := classify(e.path)
	kept := 0 // bytes of path text this entry leaves in the plan
	switch {
	case e.isLink():
		fold := foldName(e.path)
		t.linkFold[fold], t.foldLens[len(fold)], kept = true, true, len(fold)
	case e.isGitlink():
		fold := foldName(e.path)
		t.gitFold[fold], t.foldLens[len(fold)], kept = true, true, len(fold)
	}
	if e.isLink() || e.isGitlink() || ip.relevant() {
		t.byPath[e.path] = e
		kept += len(e.path)
		s.retained++
		if s.sideKept[side]++; s.sideKept[side] > maxReviewInstructionRetained {
			return fmt.Errorf("review instructions: more than %d instruction-path, link or submodule entries in commit %s", maxReviewInstructionRetained, s.shas[side])
		}
	}
	if err := s.keep(side, kept); err != nil {
		return err
	}
	if ip.n == 0 && ip.lead == 0 {
		return nil
	}
	if err := s.register(ip, e.path, side); err != nil {
		return err
	}
	if !ip.relevant() {
		return nil
	}
	if e.isGitlink() && ip.n > 0 {
		return fmt.Errorf("review instructions: %s is a submodule under an instruction path", strconv.Quote(e.path))
	}
	if side == 1 {
		s.tracked = append(s.tracked, e)
	}
	if e.isLink() {
		pair := s.links[e.path]
		if pair == nil {
			pair = &[2]*treeEntry{}
			s.links[e.path] = pair
		}
		pair[side] = &e
	}
	if ip.n == 0 {
		return nil
	}
	key := strings.Join(ip.fold[:ip.n], "/")
	c := s.cands[key]
	if c == nil {
		c = &candidate{canon: ip.canon, spelled: strings.Join(ip.parts[:ip.n], "/")}
		s.cands[key] = c
		if err := s.keep(side, len(key)+len(c.canon)+len(c.spelled)); err != nil {
			return err
		}
	}
	if side == 0 {
		c.base = append(c.base, e)
	} else {
		c.res = append(c.res, e)
	}
	return nil
}

func (c *candidate) isDir() bool {
	for _, side := range [][]treeEntry{c.res, c.base} {
		if len(side) > 0 {
			return side[0].path != c.spelled
		}
	}
	return false
}

// differing returns the candidates the two trees do not hold identically, in
// order, after the spelling, type and cap rules.
func (s *planState) differing(ctx context.Context) ([]*candidate, error) {
	keys := make([]string, 0, len(s.cands))
	for k := range s.cands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*candidate
	var baseFiles, resFiles int
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := s.cands[k]
		if sameEntries(c.base, c.res) {
			continue
		}
		if c.spelled != c.canon {
			return nil, fmt.Errorf("review instructions: instruction path %s is not spelled %s; a review cannot mask it", c.spelled, c.canon)
		}
		if len(c.base) > 0 && len(c.res) > 0 && (c.base[0].path == c.spelled) != (c.res[0].path == c.spelled) {
			return nil, fmt.Errorf("review instructions: the build changed %s between a file and a directory", c.spelled)
		}
		if baseFiles += len(c.base); baseFiles > maxReviewInstructionFiles || resFiles+len(c.res) > maxReviewInstructionFiles {
			return nil, fmt.Errorf("review instructions: more than %d files under instruction paths (at %s)", maxReviewInstructionFiles, c.spelled)
		}
		resFiles += len(c.res)
		out = append(out, c)
	}
	if len(out) > maxReviewInstructionMasks {
		return nil, fmt.Errorf("review instructions: %d instruction paths changed, over the limit of %d", len(out), maxReviewInstructionMasks)
	}
	return out, nil
}

// checkMaskParents refuses a mask whose target lies below a link: a caller
// creating the mountpoint would write through it. Every proper prefix of a
// target must not be a symlink in the result tree and must be, on disk, a
// real directory (or, for a mask over a path the result removed, absent).
func (s *planState) checkMaskParents(root string, cands []*candidate) error {
	for _, c := range cands {
		parts := strings.Split(c.canon, "/")
		for k := 1; k < len(parts); k++ {
			if s.res.isLink(foldName(strings.Join(parts[:k], "/"))) {
				return fmt.Errorf("review instructions: %s is a symlink in the result commit, above the instruction path %s", strconv.Quote(strings.Join(parts[:k], "/")), c.canon)
			}
		}
		if err := requireRealDirs(root, parts[:len(parts)-1], len(c.res) == 0); err != nil {
			return fmt.Errorf("review instructions: above the instruction path %s: %w", c.canon, err)
		}
	}
	return nil
}
