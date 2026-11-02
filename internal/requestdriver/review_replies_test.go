package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

const replySHA = "cafef00dcafef00dcafef00dcafef00dcafef00d"

type postedReply struct {
	id   int64
	body string
}

// runReplyRound runs one accepted review round whose builder's last final
// message is final (evidence nil when withEvidence is false) over threads,
// and returns the replies and the pull request comments posted.
func runReplyRound(t *testing.T, threads []forge.Thread, final string, withEvidence bool) ([]postedReply, []string) {
	t.Helper()
	if !withEvidence {
		return runReplyRounds(t, threads, nil)
	}
	return runReplyRounds(t, threads, []string{"earlier", final})
}

// runReplyRounds is runReplyRound with the build rounds' final messages given
// oldest first (none: the run recorded no evidence).
func runReplyRounds(t *testing.T, threads []forge.Thread, finals []string) ([]postedReply, []string) {
	t.Helper()
	replies, comments, _ := runReplyRoundsOn(t, threads, finals, nil)
	return replies, comments
}

// runReplyRoundsOn is runReplyRounds that also returns the request after the
// round and sets the fake's numstat to stats (nil: the default fake, which
// refuses as a failed git does).
func runReplyRoundsOn(t *testing.T, threads []forge.Thread, finals []string, stats func(ctx context.Context, dir, base, head string) ([]requestdriver.FileStat, error)) ([]postedReply, []string, *request.Request) {
	t.Helper()
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		a := &run.Run{ID: requestdrivertest.ArgValue(args, "-ticket"), State: run.StateAccepted, WorkspacePath: t.TempDir(), ResultSHA: replySHA, Branch: requestdrivertest.ArgValue(args, "-on-branch")}
		if finals != nil {
			ev := &run.AgentEvidence{}
			for i, f := range finals {
				ev.Rounds = append(ev.Rounds, run.AgentEvidenceRound{Index: i + 1, FinalMessage: f})
			}
			a.AgentEvidence = ev
		}
		return saveAcceptedRoundRun(t, dataDir, a)
	}
	dp.pushExistingBranchFn = func(ctx context.Context, workspaceDir, sha, branch string) error {
		requestdrivertest.LastPushedSHA[branch] = sha
		return nil
	}
	if stats != nil {
		dp.changedFileStatsFn = stats
	}
	var replies []postedReply
	dp.replyToReviewCommentFn = func(ctx context.Context, prURL string, commentID int64, body string) error {
		replies = append(replies, postedReply{commentID, body})
		return nil
	}
	var comments []string
	dp.commentOnPullRequestFn = func(ctx context.Context, prURL, body string) error {
		comments = append(comments, body)
		return nil
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State == request.StateHalted {
		t.Fatalf("halted: %s", r.Error)
	}
	return replies, comments, r
}

// escapedChar matches a backslash escape and the character it protects.
var escapedChar = regexp.MustCompile(`\\.`)

var twoThreads = []forge.Thread{
	{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "first", CommentID: 11},
	{ID: "t2", Path: "b.go", Line: 9, Author: "alice", Body: "second", CommentID: 12},
}

func TestReviewReplySingleThreadCarriesTheBuildersWordsAndTheCommit(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	final := "Done.\nReply (elsewhere.go:99):\nChanged the id; formatId stays because a committed challenge test requires the decimal ids.\nSecond line."
	replies, comments := runReplyRound(t, threads, final, true)
	if len(replies) != 1 || len(comments) != 0 {
		t.Fatalf("replies = %+v, comments = %v", replies, comments)
	}
	want := "> Changed the id; formatId stays because a committed challenge test requires the decimal ids.\n> Second line.\n\nPushed in " + replySHA + "."
	if replies[0].body != want {
		t.Errorf("body = %q, want %q", replies[0].body, want)
	}
}

func TestReviewReplyMapsByPathAndLine(t *testing.T) {
	final := "Reply (b.go:9):\nB done.\nReply (a.go:3): A done."
	replies, comments := runReplyRound(t, twoThreads, final, true)
	if len(replies) != 2 || len(comments) != 0 {
		t.Fatalf("replies = %+v, comments = %v", replies, comments)
	}
	got := map[int64]bool{}
	for _, rp := range replies {
		got[rp.id] = true
		switch rp.id {
		case 11:
			if !strings.HasPrefix(rp.body, "> A done.\n\nPushed in ") {
				t.Errorf("thread a.go body = %q", rp.body)
			}
		case 12:
			if !strings.HasPrefix(rp.body, "> B done.\n\nPushed in ") {
				t.Errorf("thread b.go body = %q", rp.body)
			}
		}
	}
	if !got[11] || !got[12] {
		t.Errorf("replied ids = %v, want both 11 and 12", got)
	}
}

func TestReviewReplyForNoThreadIsOnePullRequestComment(t *testing.T) {
	final := "Reply (a.go:3):\nA done.\nReply (zzz.go:1):\nStray."
	replies, comments := runReplyRound(t, twoThreads, final, true)
	if len(comments) != 1 || !strings.Contains(comments[0], "zzz.go:1") || !strings.Contains(comments[0], "Stray.") || !strings.Contains(comments[0], replySHA) {
		t.Fatalf("comments = %v", comments)
	}
	if len(replies) != 2 {
		t.Fatalf("replies = %+v", replies)
	}
	for _, rp := range replies {
		if rp.id == 12 && rp.body != "Addressed in "+replySHA+"." {
			t.Errorf("thread without a reply body = %q", rp.body)
		}
	}
}

func TestReviewReplyWithoutBlocksOrEvidenceIsAddressedIn(t *testing.T) {
	want := "Addressed in " + replySHA + "."
	for name, tc := range map[string]struct {
		final string
		ev    bool
	}{"no_blocks": {"All done, nothing to say.", true}, "no_evidence": {"Reply (a.go:3):\nx", false}} {
		replies, comments := runReplyRound(t, twoThreads, tc.final, tc.ev)
		if len(replies) != 2 || len(comments) != 0 {
			t.Fatalf("%s: replies = %+v, comments = %v", name, replies, comments)
		}
		for _, rp := range replies {
			if rp.body != want {
				t.Errorf("%s: body = %q, want %q", name, rp.body, want)
			}
		}
	}
}

func TestReviewReplyIsInertMarkdown(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	for name, in := range map[string]string{
		"mention":   "thanks @octocat",
		"entity":    "&#64;user and &commat;user",
		"image":     "![](http://x/?d=1)",
		"link":      "[a](http://x)",
		"crossref":  "see org/repo#1",
		"html":      "<img src=x onerror=y>",
		"autolink":  "see https://x.example and www.example.com",
		"unclosed":  "```\nstill code",
		"tilde":     "~~~\nstill code",
		"backslash": "a \\ b",
	} {
		replies, _ := runReplyRound(t, threads, "Reply (a.go:3):\n"+in, true)
		if len(replies) != 1 {
			t.Fatalf("%s: replies = %+v", name, replies)
		}
		body := replies[0].body
		quoted, tail, ok := strings.Cut(body, "\n\nPushed in ")
		if !ok || tail != replySHA+"." {
			t.Fatalf("%s: commit line is not outside the quote: %q", name, body)
		}
		for _, l := range strings.Split(quoted, "\n") {
			if !strings.HasPrefix(l, ">") {
				t.Errorf("%s: line %q is not in the blockquote", name, l)
			}
		}
		// What a markdown renderer sees: every backslash-escaped character
		// is plain text, so only an unescaped one can start anything.
		active := escapedChar.ReplaceAllString(quoted, "")
		for _, bad := range []string{"@", "[", "]", "!", "<", ">", "&", "`", "~", "#", "://", "www."} {
			if strings.Contains(strings.TrimPrefix(strings.ReplaceAll(active, "\n>", "\n"), ">"), bad) {
				t.Errorf("%s: %q holds an unescaped %q", name, quoted, bad)
			}
		}
	}
}

func TestReviewReplyPlainSentenceStaysReadable(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	replies, _ := runReplyRound(t, threads, "Reply (a.go:3):\nRenamed the helper, and kept the old name (see tests).", true)
	if len(replies) != 1 || !strings.HasPrefix(replies[0].body, "> Renamed the helper, and kept the old name (see tests).\n\nPushed in ") {
		t.Fatalf("replies = %+v", replies)
	}
}

func TestReviewReplyIsCapped(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	replies, _ := runReplyRound(t, threads, "Reply (a.go:3):\n"+strings.Repeat("y", 5000), true)
	if len(replies) != 1 || len(replies[0].body) > 1700 || !strings.HasSuffix(replies[0].body, "Pushed in "+replySHA+".") {
		t.Fatalf("reply length = %d", len(replies[0].body))
	}
}

func TestReviewReplyTakesTheNewestRoundThatWroteOne(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	replies, _ := runReplyRounds(t, threads, []string{"Reply (a.go:3):\nround one words", "verify fixed, no replies here"})
	if len(replies) != 1 || !strings.HasPrefix(replies[0].body, "> round one words\n\nPushed in ") {
		t.Fatalf("replies = %+v", replies)
	}
	replies, _ = runReplyRounds(t, threads, []string{"Reply (a.go:3):\nold", "Reply (a.go:3):\nnew"})
	if len(replies) != 1 || !strings.HasPrefix(replies[0].body, "> new\n") {
		t.Fatalf("replies = %+v", replies)
	}
}

func TestReviewReplyTwoThreadsAtOnePlaceTakeBlocksInOrder(t *testing.T) {
	threads := []forge.Thread{
		{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "x", CommentID: 11},
		{ID: "t2", Path: "a.go", Line: 3, Author: "bob", Body: "y", CommentID: 12},
		{ID: "t3", Path: "b.go", Line: 1, Author: "bob", Body: "z", CommentID: 13},
	}
	final := "Reply (./a.go:3):\nfirst\nReply (a.go:3):\nsecond\nReply (a.go:3):\nthird"
	replies, comments := runReplyRound(t, threads, final, true)
	byID := map[int64]string{}
	for _, rp := range replies {
		byID[rp.id] = rp.body
	}
	if !strings.HasPrefix(byID[11], "> first\n\nPushed") || !strings.HasPrefix(byID[12], "> second\n>\n> third\n\nPushed") || byID[13] != "Addressed in "+replySHA+"." || len(comments) != 0 {
		t.Fatalf("replies = %v, comments = %v", byID, comments)
	}
}

func TestReviewReplyBlocksParsing(t *testing.T) {
	fenced := "Reply (a.go:3):\nreal\n```\nReply (b.go:9):\nnot a header\n```\nafter"
	replies, _ := runReplyRound(t, twoThreads, fenced, true)
	if len(replies) != 2 {
		t.Fatalf("replies = %+v", replies)
	}
	for _, rp := range replies {
		if rp.id == 11 && !strings.Contains(rp.body, "not a header") {
			t.Errorf("a.go body = %q, want the fenced line kept as text", rp.body)
		}
		if rp.id == 12 && rp.body != "Addressed in "+replySHA+"." {
			t.Errorf("b.go body = %q, want no reply from a fenced header", rp.body)
		}
	}
	replies, _ = runReplyRound(t, twoThreads, "Reply (a.go:3): fixed f(x): done\nReply (./b.go:9): ok", true)
	got := map[int64]string{}
	for _, rp := range replies {
		got[rp.id] = rp.body
	}
	if !strings.HasPrefix(got[11], "> fixed f\\(x\\)") && !strings.HasPrefix(got[11], "> fixed f(x): done") {
		t.Errorf("a.go body = %q", got[11])
	}
	if !strings.HasPrefix(got[12], "> ok") {
		t.Errorf("b.go body = %q, want ./ normalised", got[12])
	}
}

func TestReviewReplyOnlyForThreadsTheAddendumKept(t *testing.T) {
	var threads []forge.Thread
	for i := 0; i < requestdriver.MaxAddendumReviewThreads+2; i++ {
		threads = append(threads, forge.Thread{ID: fmt.Sprintf("t%d", i), Path: "a.go", Line: i + 1, Author: "alice", Body: "c", CommentID: int64(100 + i)})
	}
	last := len(threads)
	final := fmt.Sprintf("Reply (a.go:1):\nfirst\nReply (a.go:%d):\nlast", last)
	replies, comments := runReplyRound(t, threads, final, true)
	if len(replies) != requestdriver.MaxAddendumReviewThreads {
		t.Fatalf("replies = %d, want %d", len(replies), requestdriver.MaxAddendumReviewThreads)
	}
	for _, rp := range replies {
		if rp.id == int64(100+last-1) {
			t.Errorf("replied on a thread the addendum dropped")
		}
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "last") {
		t.Errorf("comments = %v, want the dropped thread's reply as one comment", comments)
	}
}

func TestRoundBriefAsksForPerCommentReplies(t *testing.T) {
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	path, err := requestdriver.WriteRoundAddendum(dataDir, r.ID, &r.Tickets[0], twoThreads, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "`Reply (<path>:<line>):`") {
		t.Fatalf("brief lacks the reply instruction:\n%s", content)
	}
}

func addressedFixture(n int, marker string) []request.AddressedComment {
	var out []request.AddressedComment
	for i := 0; i < n; i++ {
		out = append(out, request.AddressedComment{ThreadID: fmt.Sprintf("old%d", i), Path: "old.go", Line: i + 1, Author: "alice", Body: fmt.Sprintf("%s-%d ", marker, i) + strings.Repeat("x", 1500)})
	}
	return out
}

func TestPushedRoundRecordsItsCommentsAndTheNextBriefListsThemAsAddressed(t *testing.T) {
	first := []forge.Thread{{ID: "tA", Path: "a.go", Line: 3, Author: "alice", Body: "set Cache-Control: no-store", CommentID: 11}}
	_, _, r := runReplyRoundsOn(t, first, nil, nil)
	ticket := &r.Tickets[0]
	if len(ticket.Addressed) != 1 || ticket.Addressed[0].Body != "set Cache-Control: no-store" {
		t.Fatalf("addressed = %+v", ticket.Addressed)
	}
	second := []forge.Thread{{ID: "tB", Path: "b.go", Line: 9, Author: "alice", Body: "rename the module", CommentID: 12}}
	path, err := requestdriver.WriteRoundAddendum(filepath.Dir(ticket.SpecPath), r.ID, ticket, second, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	brief := string(raw)
	todo, done, ok := strings.Cut(brief, "## Review comments answered in an earlier round")
	if !ok || !strings.Contains(todo, "rename the module") || strings.Contains(todo, "no-store") || !strings.Contains(done, "no-store") || strings.Contains(done, "rename the module") {
		t.Fatalf("brief sections wrong:\n%s", brief)
	}
	if !strings.Contains(done, "the changes made for them stay unless a new comment asks otherwise") {
		t.Errorf("heading lacks the instruction:\n%s", done)
	}
}

func TestBriefDropsAddressedCommentsBeforeNewOnes(t *testing.T) {
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	ticket := &r.Tickets[0]
	filler := "Verify-Command: make verify\n\n## Goal\n\n" + strings.Repeat("0123456789abcdef\n", 148000/17)
	if err := os.WriteFile(ticket.SpecPath, []byte(filler), 0o600); err != nil {
		t.Fatal(err)
	}
	ticket.Addressed = addressedFixture(6, "OLDBODY")
	var fresh []forge.Thread
	for i := 0; i < 15; i++ {
		fresh = append(fresh, forge.Thread{ID: fmt.Sprintf("n%d", i), Path: "n.go", Line: i + 1, Author: "bob", Body: fmt.Sprintf("NEWBODY-%d ", i) + strings.Repeat("y", 1500), CommentID: int64(i + 1)})
	}
	path, err := requestdriver.WriteRoundAddendum(dataDir, r.ID, ticket, fresh, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	brief := string(raw)
	for i := 0; i < 15; i++ {
		if !strings.Contains(brief, fmt.Sprintf("NEWBODY-%d ", i)) {
			t.Errorf("new comment %d was dropped", i)
		}
	}
	if got := strings.Count(brief, "OLDBODY-"); got >= 6 || !strings.Contains(brief, "earlier review comments omitted") {
		t.Errorf("already-addressed entries kept = %d, want some dropped with a note", got)
	}
}

func TestReviewReplyCommitLineNamesFilesWithCounts(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	var gotBase string
	stats := func(_ context.Context, dir, base, head string) ([]requestdriver.FileStat, error) {
		gotBase = base
		if head != replySHA {
			t.Errorf("head %q", head)
		}
		return []requestdriver.FileStat{{Path: "a.go", Added: 3, Deleted: 1}, {Path: "img.png", Binary: true}, {Path: "we`ird\nname.go", Added: 1}}, nil
	}
	replies, _, _ := runReplyRoundsOn(t, threads, []string{"Reply (a.go:3):\nDone."}, stats)
	want := "> Done.\n\nPushed in " + replySHA + ": `a.go` +3 \u22121, `img.png` (binary), `we'ird name.go` +1 \u22120."
	if len(replies) != 1 || replies[0].body != want {
		t.Fatalf("replies = %+v\nwant %q", replies, want)
	}
	// The base is the remote head the push was checked against.
	if gotBase == "" {
		t.Errorf("base = %q", gotBase)
	}
	replies, _, _ = runReplyRoundsOn(t, threads, []string{"nothing"}, stats)
	if !strings.HasPrefix(replies[0].body, "Addressed in "+replySHA+": `a.go` +3") {
		t.Errorf("no-reply body = %q", replies[0].body)
	}
}

func TestReviewReplyCommitLineCapsFilesAndFallsBackWhenGitFails(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	many := func(context.Context, string, string, string) ([]requestdriver.FileStat, error) {
		var st []requestdriver.FileStat
		for i := 0; i < 13; i++ {
			st = append(st, requestdriver.FileStat{Path: fmt.Sprintf("f%d.go", i), Added: 1})
		}
		return st, nil
	}
	replies, _, _ := runReplyRoundsOn(t, threads, []string{"x"}, many)
	if !strings.HasSuffix(replies[0].body, "`f9.go` +1 \u22120, and 3 more.") || strings.Contains(replies[0].body, "f10.go") {
		t.Errorf("body = %q", replies[0].body)
	}
	failing := func(context.Context, string, string, string) ([]requestdriver.FileStat, error) {
		return nil, errors.New("git failed")
	}
	replies, _, _ = runReplyRoundsOn(t, threads, []string{"Reply (a.go:3):\nok"}, failing)
	if replies[0].body != "> ok\n\nPushed in "+replySHA+"." {
		t.Errorf("body = %q, want the bare line", replies[0].body)
	}
}

func TestRealChangedFileStatsOnARealRepository(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old.txt", []byte("a\nb\nc\nd\n"))
	write("keep.txt", []byte("1\n2\n"))
	git("add", "-A")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	git("mv", "old.txt", "new.txt")
	write("new.txt", []byte("a\nb\nc\nd\ne\n"))
	write("bin.dat", []byte{0, 1, 2, 0, 255})
	write("tab\tname.txt", []byte("x\n"))
	write("keep.txt", []byte("1\n"))
	git("add", "-A")
	git("commit", "-qm", "round")
	head := git("rev-parse", "HEAD")

	got, err := requestdriver.RealChangedFileStats(context.Background(), dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]requestdriver.FileStat{}
	for _, f := range got {
		by[f.Path] = f
	}
	want := map[string]requestdriver.FileStat{
		"old.txt":       {Path: "old.txt", Deleted: 4},
		"new.txt":       {Path: "new.txt", Added: 5},
		"bin.dat":       {Path: "bin.dat", Binary: true},
		"tab\tname.txt": {Path: "tab\tname.txt", Added: 1},
		"keep.txt":      {Path: "keep.txt", Deleted: 1},
	}
	if len(by) != len(want) {
		t.Fatalf("stats = %+v", got)
	}
	for k, w := range want {
		if by[k] != w {
			t.Errorf("%q = %+v, want %+v", k, by[k], w)
		}
	}
	if _, err := requestdriver.RealChangedFileStats(context.Background(), dir, "nosuchcommit", head); err == nil {
		t.Error("an unknown commit gave no error")
	}
}

func TestCIRoundBriefHasAddressedCommentsAndNoReplyInstruction(t *testing.T) {
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	ticket := &r.Tickets[0]
	ticket.Addressed = addressedFixture(2, "KEEPME")
	path, err := requestdriver.WriteRoundAddendum(dataDir, r.ID, ticket, nil, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	brief := string(raw)
	if !strings.Contains(brief, "## Review comments answered in an earlier round") || !strings.Contains(brief, "KEEPME-0") {
		t.Errorf("CI-round brief lacks the answered comments:\n%s", brief)
	}
	if strings.Contains(brief, "Replying to the reviewer") || strings.Contains(brief, "Reviewer comments to address") {
		t.Errorf("CI-round brief asks for replies to comments it has none of:\n%s", brief)
	}
}

func TestAdviceRoundBriefHasAddressedComments(t *testing.T) {
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	ticket := &r.Tickets[0]
	ticket.Addressed = addressedFixture(1, "KEEPME")
	path, err := requestdriver.WriteTicketBuildInput(dataDir, r, ticket)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "## Review comments answered in an earlier round") || !strings.Contains(string(raw), "KEEPME-0") {
		t.Errorf("advice-round build input lacks the answered comments:\n%s", raw)
	}
	ticket.Addressed = nil
	path, _ = requestdriver.WriteTicketBuildInput(dataDir, r, ticket)
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "answered in an earlier round") {
		t.Error("a ticket with nothing answered got the section")
	}
}

func TestParseNumstatZ(t *testing.T) {
	got := requestdriver.ParseNumstatZ("3\t1\ta.go\x00-\t-\timg.png\x00")
	want := []requestdriver.FileStat{{Path: "a.go", Added: 3, Deleted: 1}, {Path: "img.png", Binary: true}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
}
