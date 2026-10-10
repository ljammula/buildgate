package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The diff file a review reads as fenced data: one section per changed file
// (bounded hunks of the base blob against the result blob, a few git processes
// at a time) and one per removed untracked entry, the whole within
// maxReviewInstructionDiffBytes.

// reviewInstructionDiffLine is more than the longest line the diff file writes
// about a cut: one "[truncated: ...]" line after a section, and the last line
// that counts the paths not listed.
const reviewInstructionDiffLine = 64

// diffSection is one path of the diff file: its header (with the mode line,
// when the executable bit changed), then a changed file's hunks or a removed
// entry's content, then the note of a removed entry that has no content.
type diffSection struct {
	head, note string
	diff       *stagedDiff
	body       []byte
}

// fixed is the bytes the section writes whatever room its content gets.
func (s diffSection) fixed() int { return len(s.head) + len(s.note) + reviewInstructionDiffLine }

func diffSections(diffs []stagedDiff, removed []removal) []diffSection {
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].path < diffs[j].path })
	out := make([]diffSection, 0, len(diffs)+len(removed))
	for i := range diffs {
		d := &diffs[i]
		label := "changed"
		switch {
		case d.base == nil:
			label = "added by the build"
		case d.res == nil:
			label = "removed by the build"
		}
		head := fmt.Sprintf("=== %s (%s) ===\n", strconv.Quote(d.path), label)
		if d.base != nil && d.res != nil && d.base.mode != d.res.mode {
			head += fmt.Sprintf("mode changed: %s -> %s\n", d.base.mode, d.res.mode)
		}
		out = append(out, diffSection{head: head, diff: d})
	}
	for _, r := range removed {
		sec := diffSection{head: fmt.Sprintf("=== %s (untracked, removed before review) ===\n", strconv.Quote(r.path)), body: r.body}
		if r.note != "" {
			sec.note = r.note + "\n"
		}
		out = append(out, sec)
	}
	return out
}

// writeReviewInstructionDiff writes dst/instructions.diff: for each changed
// file a quoted header, a mode line when the executable bit changed and the
// bounded hunks of the base blob against the result blob (git runs in dst on
// blobs written there, so no worktree attribute applies and no host path
// reaches the file), then one section per removed untracked entry. The file
// is at most maxReviewInstructionDiffBytes, headers included. Content gets the
// room that the headers of the sections after it do not need. When the headers
// of every section do not fit, only those of the changed files are kept room
// for, so their content is still shown, removed entries are listed by header
// alone as far as the file goes, and the last line counts the paths not
// listed.
func writeReviewInstructionDiff(ctx context.Context, dst string, diffs []stagedDiff, removed []removal) error {
	sections := diffSections(diffs, removed)
	const limit = maxReviewInstructionDiffBytes - reviewInstructionDiffLine
	reserved, reserve := len(sections), 0 // the sections whose headers are kept room for, and the room
	for _, sec := range sections {
		reserve += sec.fixed()
	}
	if reserve > limit {
		reserved, reserve = len(diffs), 0
		for _, sec := range sections[:reserved] {
			reserve += sec.fixed()
		}
	}
	var out bytes.Buffer
	listed := 0
	var batch []rawDiff // git's output for the changed files from section batchAt on
	batchAt := 0
	for i, sec := range sections {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := limit - out.Len()
		if sec.fixed() > left {
			break
		}
		out.WriteString(sec.head)
		room := 0
		if i < reserved {
			room = max(left-reserve, 0)
			reserve -= sec.fixed()
		}
		text, over := sec.body, int64(0)
		if sec.diff != nil {
			if i >= batchAt+len(batch) {
				batchAt, batch = i, rawDiffs(ctx, dst, diffs[i:min(i+reviewInstructionDiffJobs, len(diffs))])
			}
			raw := batch[i-batchAt]
			if raw.err != nil {
				return raw.err
			}
			text, over = raw.cut(min(room, maxReviewInstructionFileDiff))
		} else if len(text) > room {
			text, over = text[:room], int64(len(text)-room)
		}
		appendBounded(&out, text, over)
		out.WriteString(sec.note)
		listed++
	}
	if listed < len(sections) {
		fmt.Fprintf(&out, "[%d more instruction paths not listed]\n", len(sections)-listed)
	}
	return writeSnapshotFile(filepath.Join(dst, reviewInstructionDiffFile), out.Bytes(), false)
}

func appendBounded(out *bytes.Buffer, text []byte, over int64) {
	out.Write(text)
	if over > 0 {
		if len(text) > 0 && text[len(text)-1] != '\n' {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "[truncated: %d more bytes not shown]\n", over)
	}
}

// keepHunks drops everything git printed before the first hunk (its diff,
// index, --- and +++ lines carry paths) and keeps the hunk lines.
func keepHunks(raw []byte) []byte {
	var out bytes.Buffer
	started := false
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if len(line) == 0 || (!started && !bytes.HasPrefix(line, []byte("@@"))) {
			continue
		}
		started = true
		if strings.IndexByte("@+- \\", line[0]) >= 0 {
			out.Write(line)
		}
	}
	return out.Bytes()
}

// reviewInstructionDiffJobs is how many changed files are diffed at a time:
// each is one git process, and a change may hold two thousand.
const reviewInstructionDiffJobs = 8

// rawDiff is what git printed for one changed file: the first
// maxReviewInstructionFileDiff bytes, and how many bytes there were in all.
type rawDiff struct {
	head  []byte
	total int64
	err   error
}

// cut is the hunks within the first budget bytes of git's output, and the
// number of bytes after them: what a diff run with that budget returns.
func (r rawDiff) cut(budget int) ([]byte, int64) {
	budget = max(budget, 0)
	return keepHunks(r.head[:min(budget, len(r.head))]), max(r.total-int64(budget), 0)
}

// rawDiffs diffs each of diffs, all at once, and returns the outputs in order.
func rawDiffs(ctx context.Context, dst string, diffs []stagedDiff) []rawDiff {
	out := make([]rawDiff, len(diffs))
	var wg sync.WaitGroup
	for i := range diffs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = diffOne(ctx, dst, diffs[i])
		}()
	}
	wg.Wait()
	return out
}

func diffOne(ctx context.Context, dst string, d stagedDiff) rawDiff {
	if d.resTmp == "" && d.base != nil && d.res != nil || d.base != nil && d.base.isLink() {
		return rawDiff{} // mode only, or a link (identical in both trees)
	}
	oldPath, newPath := "/dev/null", "/dev/null"
	if d.baseFile != "" {
		oldPath = d.baseFile
	}
	if d.resTmp != "" {
		newPath = d.resTmp
	}
	w := &cappedWriter{max: maxReviewInstructionFileDiff}
	err := reviewGit(ctx, dst, w, "diff", "--no-index", "--text", "--no-ext-diff", "--no-textconv", "--no-color", "--", oldPath, newPath)
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return rawDiff{err: fmt.Errorf("review instructions: diff of %s: %w", strconv.Quote(d.path), err)}
	}
	return rawDiff{head: w.buf.Bytes(), total: int64(w.buf.Len()) + w.over}
}
