package request

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// RevisionKindEdit marks a Revision that SnapshotEdit took: the text an
// operator's in-place edit replaced, not a rejected draft. A Revision with
// an empty Kind is a rejection's.
const RevisionKindEdit = "edit"

// Edit is one entry of Request.Edits: an operator's in-place edit of a
// reviewed file (spec.md in spec_review, a ticket in plan_review), recorded
// by RecordEdit so "edited by you" can be shown and audited afterwards.
type Edit struct {
	By string `json:"by"`
	At string `json:"at"`
	// Path is the edited file, relative to the request's own directory
	// ("spec.md", "tickets/001.spec.md").
	Path string `json:"path"`
	// FromState is the review state the edit was made in. stageFeedback
	// hands an edit to the drafter of that stage only.
	FromState State `json:"from_state"`
	// Revision is the index of the Revision (Kind RevisionKindEdit) holding
	// the whole text the edit replaced.
	Revision int `json:"revision"`
	// Diff is the edit as changed lines with a little context ("- " removed,
	// "+ " added, "  " unchanged), cut at maxEditDiffBytes. It is what a
	// later rejection's feedback hands the drafter and what the console
	// shows; the full before-text is in the revision.
	Diff string `json:"diff"`
	// DiffTruncated is true when Diff was cut or could not be computed.
	DiffTruncated bool `json:"diff_truncated,omitempty"`
}

const (
	// maxEditDiffBytes bounds one edit's Diff: several edits must fit the
	// feedback file a drafter reads (the driver caps that file as a whole).
	maxEditDiffBytes = 3 * 1024
	// maxEditDiffCells bounds the line-diff table, so a save of two very
	// long files costs a bounded allocation under the request lock.
	maxEditDiffCells = 4_000_000
	editDiffContext  = 2
	// maxSectionEditsBytes bounds the edits quoted in one rejection's
	// feedback section. The driver keeps the newest feedback within its own
	// cap and cuts at a section heading: a section whose edits outgrew that
	// cap would lose its heading and the operator's reason, which come first.
	maxSectionEditsBytes = 4 * 1024
	// MaxEditByLen bounds the operator name an edit is recorded under: it is
	// the client's own claim, stored on the request and in the revision.
	MaxEditByLen = 200
)

// SnapshotEdit copies relPath as it is on disk now (the text an edit is
// about to replace) into a new revisions/<n>/ directory with Kind
// RevisionKindEdit, and returns n. FeedbackSupplied is false: an edit
// reaches a drafter only through a later rejection (stageFeedback).
func SnapshotEdit(dataDir, id, by string, fromState State, relPath string, now time.Time) (int, error) {
	before, err := os.ReadFile(filepath.Join(Dir(dataDir, id), relPath))
	if err != nil {
		return 0, fmt.Errorf("request %s: read %s for revision: %w", id, relPath, err)
	}
	n, err := nextRevisionIndex(dataDir, id)
	if err != nil {
		return 0, err
	}
	dir := revisionDir(dataDir, id, n)
	dst := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, fmt.Errorf("request %s: create revision file dir: %w", id, err)
	}
	if err := os.WriteFile(dst, before, 0o600); err != nil {
		return 0, fmt.Errorf("request %s: write revision file: %w", id, err)
	}
	meta := Revision{
		Index:     n,
		At:        now.UTC().Format(time.RFC3339Nano),
		By:        by,
		FromState: fromState,
		Files:     []string{relPath},
		Kind:      RevisionKindEdit,
	}
	if err := writeRevisionMeta(id, dir, meta); err != nil {
		return 0, err
	}
	return n, nil
}

// RecordEdit replaces relPath's content with after as an operator's edit:
// it snapshots the current text as a revision, writes after (temp file then
// rename), and appends the Edit to r, saved. The caller holds the request
// lock, loaded r under it, and has validated after and checked it against
// the text the operator started from. An edit that changes nothing writes
// and records nothing.
//
// A failure after the snapshot leaves a revision no Edit names; a failure
// after the file write leaves the edit unrecorded. Neither loses text: the
// file is whole either way, and the error is returned.
func RecordEdit(dataDir string, r *Request, by, relPath, after string, now time.Time) error {
	path := filepath.Join(Dir(dataDir, r.ID), relPath)
	before, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("request %s: read %s: %w", r.ID, relPath, err)
	}
	if string(before) == after {
		return nil
	}
	n, err := SnapshotEdit(dataDir, r.ID, by, r.State, relPath, now)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(after), 0o600); err != nil {
		return fmt.Errorf("request %s: write %s: %w", r.ID, relPath, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("request %s: finalize %s: %w", r.ID, relPath, err)
	}
	diff, truncated := editDiff(string(before), after)
	r.Edits = append(r.Edits, Edit{
		By:            by,
		At:            now.UTC().Format(time.RFC3339Nano),
		Path:          relPath,
		FromState:     r.State,
		Revision:      n,
		Diff:          diff,
		DiffTruncated: truncated,
	})
	if err := r.Save(dataDir); err != nil {
		// The record could not be saved: put the replaced text back, so a
		// save the caller is told failed has not changed the file, and a
		// retry meets the text and the hash it started from.
		r.Edits = r.Edits[:len(r.Edits)-1]
		if werr := os.WriteFile(tmp, before, 0o600); werr == nil {
			werr = os.Rename(tmp, path)
			if werr == nil {
				return err
			}
		}
		return fmt.Errorf("%w (and %s could not be restored: it holds the edited text)", err, relPath)
	}
	return nil
}

// feedbackSectionHeadings are the strings the driver cuts the feedback file
// at (stageFeedback's own headings). Quoted text must never contain one: the
// cut would start a section in the middle of a diff line.
var feedbackSectionHeadings = strings.NewReplacer(
	"## Spec rejected ", "## Spec (rejected) ",
	"## Oracle rejected ", "## Oracle (rejected) ",
	"## Plan rejected ", "## Plan (rejected) ",
)

// quotedLine makes one line of edited text safe to quote on one line of a
// drafter's feedback: every control character and Unicode line separator but
// a tab becomes a space (a bare CR or U+2028 inside a line would otherwise
// start a line of its own for some readers), and a section heading of the
// feedback file is rewritten.
func quotedLine(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, ""))
	return feedbackSectionHeadings.Replace(text)
}

// editDiff renders before -> after as changed lines with editDiffContext
// lines around each change, hunks separated by "...". Every line carries a
// two-character prefix, so no line of either text can start a line of the
// result (a heading, a fence) when it is quoted in a drafter's feedback.
func editDiff(before, after string) (string, bool) {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	if (len(a)+1)*(len(b)+1) > maxEditDiffCells {
		return fmt.Sprintf("(%d lines replaced by %d lines; too large to list)\n", len(a), len(b)), true
	}
	ops := lineOps(a, b)
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		for j := max(0, i-editDiffContext); j <= min(len(ops)-1, i+editDiffContext); j++ {
			keep[j] = true
		}
	}
	var out strings.Builder
	gap := false
	for i, op := range ops {
		if !keep[i] {
			gap = out.Len() > 0
			continue
		}
		if gap {
			out.WriteString("...\n")
			gap = false
		}
		line := string(op.kind) + " " + quotedLine(strings.TrimSuffix(op.text, "\r")) + "\n"
		if out.Len()+len(line) > maxEditDiffBytes {
			out.WriteString("(more changes not listed)\n")
			return out.String(), true
		}
		out.WriteString(line)
	}
	return out.String(), false
}

type lineOp struct {
	kind byte // ' ', '-' or '+'
	text string
}

// lineOps is the longest-common-subsequence line diff of a and b.
func lineOps(a, b []string) []lineOp {
	w := len(b) + 1
	lcs := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	ops := make([]lineOp, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, lineOp{' ', a[i]})
			i, j = i+1, j+1
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			ops = append(ops, lineOp{'-', a[i]})
			i++
		default:
			ops = append(ops, lineOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, lineOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, lineOp{'+', b[j]})
	}
	return ops
}

// editsBefore renders, for the feedback section of the stage rejection made
// at `at`, the edits of that stage made after `after` (the stage's previous
// rejection or the send-back that reset the stage; zero for the first) and
// not after `at`: the operator's own changes to the draft being rejected,
// which the redraft must keep. The newest are kept within
// maxSectionEditsBytes, oldest first, with a line saying earlier ones were
// left out.
func editsBefore(r *Request, stage State, after, at time.Time) string {
	var quoted []string
	size := 0
	omitted := false
	for i := len(r.Edits) - 1; i >= 0; i-- {
		e := r.Edits[i]
		when, err := time.Parse(time.RFC3339Nano, e.At)
		if err != nil || e.FromState != stage || !when.After(after) || when.After(at) {
			continue
		}
		text := quotedEdit(e)
		if size+len(text) > maxSectionEditsBytes {
			omitted = true
			break
		}
		size += len(text)
		quoted = append(quoted, text)
	}
	var b strings.Builder
	if omitted {
		b.WriteString("(Earlier hand edits of this draft are not listed.)\n\n")
	}
	for i := len(quoted) - 1; i >= 0; i-- {
		b.WriteString(quoted[i])
	}
	return b.String()
}

// quotedEdit is one edit as a drafter reads it. An edit whose lines are not
// all listed says so, and asks only for what is listed to be kept.
func quotedEdit(e Edit) string {
	who, path := sanitizeFeedbackBy(e.By), oneLine(e.Path)
	if e.DiffTruncated {
		return fmt.Sprintf("Before rejecting, %s edited %s by hand. Not every changed line is listed; keep the listed changes in the redraft unless the note above says otherwise (\"-\" lines were removed, \"+\" lines were added):\n\n%s\n", who, path, e.Diff)
	}
	return fmt.Sprintf("Before rejecting, %s edited %s by hand. Keep these changes in the redraft unless the note above says otherwise (\"-\" lines were removed, \"+\" lines were added):\n\n%s\n", who, path, e.Diff)
}
