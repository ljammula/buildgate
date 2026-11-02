package requestdriver

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// maxReviewReplyBytes bounds one reply the builder wrote for a reviewer
// comment, as posted.
const maxReviewReplyBytes = 1500

// reviewReplyInstruction is the part of a review round's brief that asks the
// builder to answer each reviewer comment in its final message, in the shape
// reviewReplyBlocks finds.
const reviewReplyInstruction = "\n## Replying to the reviewer\n\n" +
	"End your final message with one reply per comment under \"Reviewer comments to address\" (not the already-addressed ones), written to the reviewer in plain words: " +
	"what you changed for that comment, and anything you did not do and why. " +
	"Start each reply with a line `Reply (<path>:<line>):` (the path and line shown for that comment), " +
	"then the reply text; it runs to the next `Reply (` line or the end of the message.\n"

// reviewReply is one builder reply from its final message: the comment it
// answers, by path and line, and the text.
type reviewReply struct {
	path string
	line int
	text string
}

// reviewReplyBlocks finds the `Reply (<path>:<line>):` blocks in the builder's
// final message. It is structural: a line is a block header only when it
// begins with that prefix, holds path:line (a number) in the parentheses, and
// is outside a code fence of the message; text on the header line after the
// colon opens the block. Text before the first header is not a reply.
func reviewReplyBlocks(message string) []reviewReply {
	var out []reviewReply
	var cur *reviewReply
	var body []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.text = strings.TrimSpace(strings.Join(body, "\n"))
		if cur.text != "" {
			out = append(out, *cur)
		}
		cur, body = nil, nil
	}
	fenced := false
	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
		} else if !fenced {
			if path, n, rest, ok := replyHeader(trimmed); ok {
				flush()
				cur = &reviewReply{path: path, line: n}
				body = []string{rest}
				continue
			}
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// replyHeader parses `Reply (<path>:<line>):<rest>`. The header ends at the
// last "):" that leaves a path:line inside the parentheses, so a "):" in the
// reply text on the same line does not end it early and a path holding one
// still parses.
func replyHeader(line string) (path string, n int, rest string, ok bool) {
	const prefix = "Reply ("
	if !strings.HasPrefix(line, prefix) {
		return "", 0, "", false
	}
	for end := len(line); ; {
		end = strings.LastIndex(line[:end], "):")
		if end < len(prefix) {
			return "", 0, "", false
		}
		loc := line[len(prefix):end]
		colon := strings.LastIndex(loc, ":")
		if colon <= 0 {
			continue
		}
		num, err := strconv.Atoi(strings.TrimSpace(loc[colon+1:]))
		if err != nil {
			continue
		}
		return normalReplyPath(loc[:colon]), num, line[end+2:], true
	}
}

// normalReplyPath trims a path to the form a thread's Path has: no space
// around it and no leading "./".
func normalReplyPath(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(p), "./")
}

// matchReviewReplies assigns one round's replies to the included threads: a
// block goes to the first thread at its path and line that has no reply yet,
// and a block beyond the threads at that place joins the last of them. With
// exactly one included thread it gets every block. byThread is keyed by
// thread index; unmatched holds the blocks no thread took.
func matchReviewReplies(replies []reviewReply, threads []forge.Thread) (byThread map[int]string, unmatched []reviewReply) {
	byThread = map[int]string{}
	for _, rp := range replies {
		var at []int
		for i, t := range threads {
			if len(threads) == 1 || (normalReplyPath(t.Path) == rp.path && t.Line == rp.line) {
				at = append(at, i)
			}
		}
		if len(at) == 0 {
			unmatched = append(unmatched, rp)
			continue
		}
		idx := at[len(at)-1]
		for _, i := range at {
			if byThread[i] == "" {
				idx = i
				break
			}
		}
		if prev := byThread[idx]; prev != "" {
			byThread[idx] = prev + "\n\n" + rp.text
		} else {
			byThread[idx] = rp.text
		}
	}
	return byThread, unmatched
}

// matchRoundsReplies assigns each thread the reply from the newest build
// round that wrote one for it (only the first round's brief carries the
// instruction, so a later round may have none). unmatched is the newest
// round's blocks that no thread took.
func matchRoundsReplies(rounds [][]reviewReply, threads []forge.Thread) (byThread map[int]string, unmatched []reviewReply) {
	byThread = map[int]string{}
	first := true
	for i := len(rounds) - 1; i >= 0; i-- {
		if len(rounds[i]) == 0 {
			continue
		}
		m, un := matchReviewReplies(rounds[i], threads)
		if first {
			unmatched, first = un, false
		}
		for idx, text := range m {
			if byThread[idx] == "" {
				byThread[idx] = text
			}
		}
	}
	return byThread, unmatched
}

// replyEscaper backslash-escapes every character that can start markdown,
// HTML, an entity, a reference or a mention, so the builder's words survive
// as text and form no link, image, mention, cross-reference, HTML or fence.
var replyEscaper = strings.NewReplacer(
	"\\", "\\\\", "&", "\\&", "<", "\\<", ">", "\\>", "[", "\\[", "]", "\\]",
	"!", "\\!", "#", "\\#", "`", "\\`", "*", "\\*", "_", "\\_", "~", "\\~", "|", "\\|",
	"@", "\\@\u200b", "://", "\\://", "www.", "www\\.",
)

// cleanReplyText renders the builder's words inert before they are posted:
// cleaned of control characters and escapes (sanitize.Text), bounded, every
// markdown-active character escaped, and each line a blockquote line, so
// nothing in it (an image the host would fetch, a mention, an unclosed fence)
// reaches beyond its own quote. The words stay readable.
func cleanReplyText(s string) string {
	s = sanitize.Text(s)
	if len(s) > maxReviewReplyBytes {
		s = strings.ToValidUTF8(s[:maxReviewReplyBytes], "") + " [reply cut]"
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r", ""), "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + replyEscaper.Replace(l)
	}
	return strings.Join(lines, "\n")
}

// buildFinalMessages are the builder's final messages of roundRun's build
// rounds, oldest first; nil when the run recorded none.
func buildFinalMessages(roundRun *run.Run) []string {
	if roundRun == nil || roundRun.AgentEvidence == nil {
		return nil
	}
	var out []string
	for _, rd := range roundRun.AgentEvidence.Rounds {
		out = append(out, rd.FinalMessage)
	}
	return out
}

// maxReplyStatFiles bounds the files a reply's commit line names.
const maxReplyStatFiles = 10

// commitLine is the factory's own line of a reply: "<lead> in <sha>" and, from
// host git, the files the round changed with their line counts, so a change
// the builder did not mention (a revert of an earlier request) is visible to
// the reviewer. With no stats it is the bare "<lead> in <sha>.".
func commitLine(lead, sha string, stats []FileStat) string {
	if len(stats) == 0 {
		return fmt.Sprintf("%s in %s.", lead, sha)
	}
	var parts []string
	for i, s := range stats {
		if i == maxReplyStatFiles {
			parts = append(parts, fmt.Sprintf("and %d more", len(stats)-maxReplyStatFiles))
			break
		}
		if s.Binary {
			parts = append(parts, fmt.Sprintf("`%s` (binary)", replyInline(s.Path)))
			continue
		}
		parts = append(parts, fmt.Sprintf("`%s` +%d \u2212%d", replyInline(s.Path), s.Added, s.Deleted))
	}
	return fmt.Sprintf("%s in %s: %s.", lead, sha, strings.Join(parts, ", "))
}

// reviewReplyBody is the reply posted on a thread: the builder's text for it
// followed by the commit line, or the commit line alone ("Addressed in")
// when the builder wrote none.
func reviewReplyBody(text, sha string, stats []FileStat) string {
	if text == "" {
		return commitLine("Addressed", sha, stats)
	}
	return cleanReplyText(text) + "\n\n" + commitLine("Pushed", sha, stats)
}

// postReviewReplies replies on each included thread (the builder's words
// from the newest round that wrote them, else "Addressed in <sha>."), and posts the builder's
// replies that match no thread as one pull request comment. A failed reply
// is logged and leaves its thread unmarked, as before; nothing here fails the
// push.
func postReviewReplies(dp Deps, ctx context.Context, r *request.Request, ticket *request.Ticket, roundRun *run.Run, baseSHA string, threads []forge.Thread) {
	var rounds [][]reviewReply
	for _, m := range buildFinalMessages(roundRun) {
		rounds = append(rounds, reviewReplyBlocks(m))
	}
	byThread, unmatched := matchRoundsReplies(rounds, threads)
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stats []FileStat
	if baseSHA != "" {
		var err error
		if stats, err = dp.ChangedFileStats(r.Workspace, baseSHA, roundRun.ResultSHA); err != nil {
			log.Printf("request %s: ticket %d: files changed by the round, for its replies: %v", r.ID, ticket.Index, err)
			stats = nil
		}
	}
	for i, t := range threads {
		body := reviewReplyBody(byThread[i], roundRun.ResultSHA, stats)
		if err := PostReviewReply(dp, replyCtx, ticket.PRURL, t.CommentID, body); err != nil {
			log.Printf("request %s: ticket %d: reply on thread %s: %v", r.ID, ticket.Index, t.ID, err)
			continue
		}
		ticket.SeenThreadIDs = append(ticket.SeenThreadIDs, forge.ThreadSeenKey(t))
		ticket.RecordAddressed(request.AddressedComment{ThreadID: t.ID, Path: t.Path, Line: t.Line, Author: t.Author, Body: t.Body})
	}
	if len(unmatched) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Replies from the review round's build, pushed in %s, for comments not in this round's threads:\n", roundRun.ResultSHA)
	for _, rp := range unmatched {
		fmt.Fprintf(&b, "\n`%s:%d`\n\n%s\n", replyInline(rp.path), rp.line, cleanReplyText(rp.text))
	}
	if err := dp.CommentOnPullRequest(replyCtx, ticket.PRURL, b.String()); err != nil {
		log.Printf("request %s: ticket %d: comment on %s with unmatched replies: %v", r.ID, ticket.Index, ticket.PRURL, err)
	}
}
