package main

import (
	"fmt"
	"strings"

	"buildgate/internal/memory"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// memoryDocuments are what a memory request is submitted with: its request
// text, its spec in the spec skeleton, and its one ticket.
type memoryDocuments struct {
	requestText string
	spec        string
	ticket      string
}

// longestBacktickRun is the longest run of backticks in s.
func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] != '`' {
			run = 0
			continue
		}
		run++
		if run > longest {
			longest = run
		}
	}
	return longest
}

// fencedVerbatim puts content in a code fence longer than any run of
// backticks inside it, so nothing in content can close the fence.
func fencedVerbatim(content string) string {
	n := longestBacktickRun(content) + 1
	if n < 3 {
		n = 3
	}
	fence := strings.Repeat("`", n)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fence + "text\n" + content + fence + "\n"
}

// inlineVerbatim puts one line in inline code whose delimiter is longer than
// any run of backticks in it.
func inlineVerbatim(line string) string {
	delim := strings.Repeat("`", longestBacktickRun(line)+1)
	return delim + " " + line + " " + delim
}

// changeOrigin says where an added line came from, by run id only.
func changeOrigin(c memory.Change) string {
	if c.Source == memory.SourceOperator && len(c.Runs) == 0 {
		return "written by the operator"
	}
	ids := make([]string, 0, len(c.Runs))
	for _, id := range c.Runs {
		ids = append(ids, "`"+strings.ReplaceAll(sanitize.Line(id), "`", "'")+"`")
	}
	origin := "noted by the build agent of " + pluralRuns(len(ids)) + " " + strings.Join(ids, ", ")
	if c.Source == memory.SourceOperator {
		origin = "written by the operator; also " + origin
	}
	return origin
}

func pluralRuns(n int) string {
	if n == 1 {
		return "run"
	}
	return "runs"
}

func splitChanges(changes []memory.Change) (added, removed []memory.Change) {
	for _, c := range changes {
		if c.Remove {
			removed = append(removed, c)
		} else {
			added = append(added, c)
		}
	}
	return added, removed
}

// changeList renders the added and removed lines as two Markdown lists.
func changeList(changes []memory.Change) string {
	added, removed := splitChanges(changes)
	var b strings.Builder
	if len(added) > 0 {
		b.WriteString("Lines added:\n\n")
		for _, c := range added {
			fmt.Fprintf(&b, "- %s (%s)\n", inlineVerbatim(c.Line), changeOrigin(c))
		}
	}
	if len(removed) > 0 {
		if len(added) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Lines removed:\n\n")
		for _, c := range removed {
			fmt.Fprintf(&b, "- %s\n", inlineVerbatim(c.Line))
		}
	}
	return b.String()
}

// fileEnding says, in words, what a code fence cannot show: whether the
// file's last byte is a newline.
func fileEnding(expected string) string {
	if strings.HasSuffix(expected, "\n") {
		return "The file ends with exactly one newline after its last line"
	}
	return "The file does not end with a newline: its last byte is the last character shown"
}

// memoryRequestDocuments writes a memory request's spec and ticket around the
// expected AGENTS.md. The expected text is the last section of each, in a
// fence its content cannot close; every other word is the factory's.
func memoryRequestDocuments(p memory.Proposal, changes []memory.Change, verifyCommand string, fileExists bool) memoryDocuments {
	added, removed := splitChanges(changes)
	action := "Replace the file `AGENTS.md` at the repository root"
	if !fileExists {
		action = "The repository has no `AGENTS.md`. Create it at the repository root"
	}
	exact := fmt.Sprintf("%s so that it is exactly the text in the fenced block under \"%s\" at the end of this document, byte for byte. %s. It is %d bytes and its SHA-256 is `%s`.",
		action, strings.TrimPrefix(expectedFileHeading, "## "), fileEnding(p.Expected), len(p.Expected), memory.HashHex([]byte(p.Expected)))
	expectedSection := expectedFileHeading + "\n\n" + fencedVerbatim(p.Expected)

	var criteria, covered strings.Builder
	n := 0
	for _, c := range added {
		n++
		fmt.Fprintf(&criteria, "%d. The memory section of `AGENTS.md` holds this line exactly once, as written: %s\n", n, inlineVerbatim(c.Line))
	}
	for _, c := range removed {
		n++
		fmt.Fprintf(&criteria, "%d. The memory section of `AGENTS.md` no longer holds this line: %s\n", n, inlineVerbatim(c.Line))
	}
	n++
	fmt.Fprintf(&criteria, "%d. `AGENTS.md` is byte for byte the expected text, and no other file is added, changed, moved or removed.\n", n)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&covered, "- %d\n", i)
	}

	var spec strings.Builder
	spec.WriteString("# Spec\n\n## Problem\n\n")
	fmt.Fprintf(&spec, "The operator proposed a change to this repository's memory: the section of `AGENTS.md` headed \"%s\", which every later build agent reads. %d line(s) are added and %d removed.\n\n", strings.TrimPrefix(memory.SectionHeading, "## "), len(added), len(removed))
	spec.WriteString("## Scope\n\n" + exact + " No other file may change.\n\n" + changeList(changes) + "\n")
	spec.WriteString("## Non-goals\n\n- Rewording, reordering or reformatting any line, inside or outside the memory section.\n- Changing any file other than `AGENTS.md`.\n- Running or checking a command a line names.\n\n")
	spec.WriteString("## Affected services and packages\n\n- `AGENTS.md` at the repository root. No code, test or configuration file.\n\n")
	spec.WriteString("## Acceptance criteria\n\n" + criteria.String() + "\n")
	spec.WriteString("## Risks\n\n- A result whose `AGENTS.md` differs from the expected text by one byte, or that changes another file, is refused at release.\n- A line is a note a person or a build agent wrote down. Nothing has checked that a command it names works.\n\n")
	spec.WriteString("## Open questions\n\nNone.\n\n" + expectedSection)

	var ticket strings.Builder
	fmt.Fprintf(&ticket, "Verify-Command: %s\nAllowed-Files: AGENTS.md\nRequired-Changed-Files: AGENTS.md\nTests-Required: no -- this change edits only AGENTS.md\n\n", verifyCommand)
	ticket.WriteString("## Goal\n\nReplace `AGENTS.md` with the approved repository memory text.\n\n")
	ticket.WriteString("## Plan\n\n### Files to touch\n\n- `AGENTS.md` — its whole content becomes the expected text at the end of this document.\n\n")
	ticket.WriteString("### Steps\n\n1. " + exact + "\n2. Copy the text; do not reword, reorder, re-wrap or reformat any line, inside or outside the memory section.\n3. Check the file's size and SHA-256 against the numbers in step 1 (`shasum -a 256 AGENTS.md` or `sha256sum AGENTS.md`).\n4. Change no other file.\n\n")
	ticket.WriteString("### Tests to add\n\n- None: this change edits only `AGENTS.md`.\n\n")
	ticket.WriteString("### Acceptance criteria covered\n\n" + covered.String() + "\n")
	ticket.WriteString("## Out of scope\n\n- Any file other than `AGENTS.md`.\n- Running or checking a command a line names.\n\n" + expectedSection)

	return memoryDocuments{
		requestText: fmt.Sprintf("Update the repository memory in AGENTS.md: %d line(s) added, %d removed", len(added), len(removed)),
		spec:        spec.String(),
		ticket:      ticket.String(),
	}
}

// memoryPullRequestSection is what a memory request's pull request body says
// about the change: each line added or removed and the runs an added line
// came from, by id. It carries no log, note or other text from a run.
func memoryPullRequestSection(changes []memory.Change) string {
	if len(changes) == 0 {
		return ""
	}
	return "## Repository memory\n\nThis pull request replaces `AGENTS.md` with the text the operator approved at spec review. The factory rendered that text; the release check passed only because the file matches it byte for byte and no other file changed.\n\n" + changeList(changes) + "\n"
}

// memoryChangesMarkdown is the section a memory request's pull request body
// ends with, "" for every other run: the lines its proposal adds and removes
// and the ids of the runs an added line came from, read from the list saved
// beside the request's proposal. A list that is absent or cannot be read adds
// nothing; the release check has already judged the run.
func memoryChangesMarkdown(dataDir string, r *run.Run) string {
	key, ok := memoryStoreKeyOfRun(dataDir, r)
	if !ok {
		return ""
	}
	path, err := memory.ChangesPath(dataDir, key, r.RequestID)
	if err != nil {
		return ""
	}
	changes, err := memory.LoadChanges(path)
	if err != nil || len(changes) == 0 {
		return ""
	}
	return "\n" + memoryPullRequestSection(changes)
}
