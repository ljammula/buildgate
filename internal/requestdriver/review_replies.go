package requestdriver

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// maxReviewReplyBytes bounds one reply the builder wrote for a reviewer
// comment, as posted.
const maxReviewReplyBytes = 1500

// reviewReplyInstruction is the part of a review round's brief that asks the
// builder to answer each reviewer comment in its final message, in the shape
// reviewReplyBlocks finds.
const reviewReplyInstruction = "\n## Replying to the reviewer\n\n" +
	"End your final message with one reply per reviewer comment above, written to the reviewer in plain words: " +
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
// begins with that prefix and holds path:line (a number) in the parentheses;
// text on the header line after the colon opens the block. Text before the
// first header is not a reply.
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
	for _, line := range strings.Split(message, "\n") {
		if path, n, rest, ok := replyHeader(strings.TrimSpace(line)); ok {
			flush()
			cur = &reviewReply{path: path, line: n}
			body = []string{rest}
			continue
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// replyHeader parses `Reply (<path>:<line>):<rest>`.
func replyHeader(line string) (path string, n int, rest string, ok bool) {
	const prefix = "Reply ("
	if !strings.HasPrefix(line, prefix) {
		return "", 0, "", false
	}
	end := strings.Index(line, "):")
	if end < 0 {
		return "", 0, "", false
	}
	loc := line[len(prefix):end]
	colon := strings.LastIndex(loc, ":")
	if colon <= 0 {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(strings.TrimSpace(loc[colon+1:]))
	if err != nil {
		return "", 0, "", false
	}
	return strings.TrimSpace(loc[:colon]), n, line[end+2:], true
}

// matchReviewReplies assigns the builder's replies to the included threads: a
// thread gets the replies for its path and line (joined, in order), and with
// exactly one included thread it gets every reply. byThread is keyed by
// thread index; unmatched holds the replies no thread took.
func matchReviewReplies(replies []reviewReply, threads []forge.Thread) (byThread map[int]string, unmatched []reviewReply) {
	byThread = map[int]string{}
	for _, rp := range replies {
		idx := -1
		if len(threads) == 1 {
			idx = 0
		} else {
			for i, t := range threads {
				if strings.TrimSpace(t.Path) == rp.path && t.Line == rp.line {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			unmatched = append(unmatched, rp)
			continue
		}
		if prev := byThread[idx]; prev != "" {
			byThread[idx] = prev + "\n\n" + rp.text
		} else {
			byThread[idx] = rp.text
		}
	}
	return byThread, unmatched
}

// cleanReplyText makes the builder's words safe to post: bounded, no raw
// HTML, no GitHub mention. Markdown is kept.
func cleanReplyText(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > maxReviewReplyBytes {
		s = strings.ToValidUTF8(s[:maxReviewReplyBytes], "") + " [reply cut]"
	}
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	// A zero-width space after "@" keeps GitHub from reading a user or team.
	s = strings.ReplaceAll(s, "@", "@​")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.TrimSpace(s)
}

// lastBuildFinalMessage is the builder's final message of roundRun's last
// build round, "" when the run recorded none.
func lastBuildFinalMessage(roundRun *run.Run) string {
	if roundRun == nil || roundRun.AgentEvidence == nil {
		return ""
	}
	rounds := roundRun.AgentEvidence.Rounds
	if len(rounds) == 0 {
		return ""
	}
	return rounds[len(rounds)-1].FinalMessage
}

// reviewReplyBody is the reply posted on a thread: the builder's text for it
// followed by the commit the factory pushed, or the fixed line when the
// builder wrote none.
func reviewReplyBody(text, sha string) string {
	if text == "" {
		return fmt.Sprintf("Addressed in %s.", sha)
	}
	return fmt.Sprintf("%s\n\nPushed in %s.", cleanReplyText(text), sha)
}

// postReviewReplies replies on each included thread (the builder's words
// where it wrote them, else "Addressed in <sha>."), and posts the builder's
// replies that match no thread as one pull request comment. A failed reply
// is logged and leaves its thread unmarked, as before; nothing here fails the
// push.
func postReviewReplies(dp Deps, ctx context.Context, r *request.Request, ticket *request.Ticket, roundRun *run.Run, threads []forge.Thread) {
	byThread, unmatched := matchReviewReplies(reviewReplyBlocks(lastBuildFinalMessage(roundRun)), threads)
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for i, t := range threads {
		body := reviewReplyBody(byThread[i], roundRun.ResultSHA)
		if err := PostReviewReply(dp, replyCtx, ticket.PRURL, t.CommentID, body); err != nil {
			log.Printf("request %s: ticket %d: reply on thread %s: %v", r.ID, ticket.Index, t.ID, err)
			continue
		}
		ticket.SeenThreadIDs = append(ticket.SeenThreadIDs, forge.ThreadSeenKey(t))
	}
	if len(unmatched) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Replies from the review round's build, pushed in %s, for comments not in this round's threads:\n", roundRun.ResultSHA)
	for _, rp := range unmatched {
		fmt.Fprintf(&b, "\n**%s:%d**\n\n%s\n", cleanReplyText(rp.path), rp.line, cleanReplyText(rp.text))
	}
	if err := dp.CommentOnPullRequest(replyCtx, ticket.PRURL, b.String()); err != nil {
		log.Printf("request %s: ticket %d: comment on %s with unmatched replies: %v", r.ID, ticket.Index, ticket.PRURL, err)
	}
}
