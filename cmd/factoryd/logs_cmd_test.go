package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

var logsBase = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

// writeLog writes content at path with an mtime of logsBase+offset.
func writeLog(t *testing.T, path, content string, offset time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m := logsBase.Add(offset)
	if err := os.Chtimes(path, m, m); err != nil {
		t.Fatal(err)
	}
}

// logsFixture: request req1 with two tickets, ticket 2 current. Ticket 1's
// run has plain logs, ticket 2's has hash-named activity logs in the run dir.
func logsFixture(t *testing.T) (dataDir string, r *request.Request) {
	t.Helper()
	dataDir = t.TempDir()
	r = request.New("req1", "/ws", "proj", request.Source{}, logsBase)
	r.State = request.StateBuilding
	r.TicketIndex, r.TicketCount = 2, 2
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-a"}, {Index: 2, RunID: "run-b"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	writeLog(t, filepath.Join(request.Dir(dataDir, "req1"), "logs", "spec_draft.log"), "spec\n", 0)
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-a"), "build_app.log"), "a-build\n", time.Minute)
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-a"), "verify.log"), "a-verify\n", 2*time.Minute)
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-b"), "attempt-1", "compose", "db.log"), "db\n", 90*time.Second)
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-b"), "0123abcd-build.log"), "b-temporal-build\n", 3*time.Minute)
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-b"), "verify.log"), "l1\nl2\nl3\n", 4*time.Minute)
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-b"), "progress.jsonl"), "{}\n", 10*time.Minute)
	return dataDir, r
}

func runLogsString(t *testing.T, dataDir, id string, list, follow bool, n int) (string, error) {
	t.Helper()
	var buf lockedBuffer
	err := runLogs(context.Background(), &buf, dataDir, id, list, follow, n)
	return buf.String(), err
}

func TestLogsShowsNewestFileOfCurrentTicket(t *testing.T) {
	dataDir, _ := logsFixture(t)
	out, err := runLogsString(t, dataDir, "req1", false, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	// verify.log of run-b is the newest *.log; progress.jsonl never wins
	// and ticket 1's run is not considered.
	if !strings.HasPrefix(out, "==> runs/run-b/verify.log (") {
		t.Fatalf("header: %q", out)
	}
	if !strings.Contains(out, "l2\nl3\n") || strings.Contains(out, "l1") {
		t.Fatalf("want last 2 lines only: %q", out)
	}
}

func TestLogsRunIDAndTemporalFile(t *testing.T) {
	dataDir, _ := logsFixture(t)
	if err := os.Remove(filepath.Join(run.Dir(dataDir, "run-b"), "verify.log")); err != nil {
		t.Fatal(err)
	}
	out, err := runLogsString(t, dataDir, "run-b", false, false, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "==> runs/run-b/0123abcd-build.log") || !strings.Contains(out, "b-temporal-build") {
		t.Fatalf("temporal-path log not chosen: %q", out)
	}
}

func TestLogsListOldestFirstWithRelayNote(t *testing.T) {
	dataDir, _ := logsFixture(t)
	out, err := runLogsString(t, dataDir, "req1", true, false, 40)
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for _, p := range []string{
		"requests/req1/logs/spec_draft.log",
		"runs/run-a/build_app.log",
		"runs/run-b/attempt-1/compose/db.log",
		"runs/run-a/verify.log",
		"runs/run-b/0123abcd-build.log",
		"runs/run-b/verify.log",
		"runs/run-b/progress.jsonl",
	} {
		i := strings.Index(out, p)
		if i < 0 || i < idx {
			t.Fatalf("%s missing or out of order in:\n%s", p, out)
		}
		idx = i
	}
	if !strings.Contains(out, "relay-ledger/usage.jsonl") {
		t.Fatalf("relay note missing: %q", out)
	}
}

func TestLogsProcessLogsPickNewestAndListAll(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	writeLog(t, filepath.Join(logDir, "queue-run.out.log"), "launchd out\n", 0)
	writeLog(t, filepath.Join(logDir, "queue-run.err.log"), "launchd err\n", time.Minute)
	writeLog(t, filepath.Join(logDir, "quickstart-queue-run.out.log"), "qs out\n", 2*time.Minute)
	writeLog(t, filepath.Join(logDir, "quickstart-queue-run.err.log"), "qs err\n", time.Minute)
	writeLog(t, filepath.Join(logDir, "serve.out.log"), "serve out\n", 0)

	out, err := runLogsString(t, dataDir, "queue-run", false, false, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "logs/quickstart-queue-run.out.log") || !strings.Contains(out, "qs out") {
		t.Fatalf("newest queue-run log not chosen: %q", out)
	}
	list, err := runLogsString(t, dataDir, "queue-run", true, false, 40)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"queue-run.out.log", "queue-run.err.log", "quickstart-queue-run.out.log", "quickstart-queue-run.err.log"} {
		if !strings.Contains(list, "logs/"+n) {
			t.Fatalf("-list missing %s: %q", n, list)
		}
	}
	if strings.Contains(list, "serve.out.log") {
		t.Fatalf("serve log leaked into queue-run list: %q", list)
	}
	srv, err := runLogsString(t, dataDir, "serve", false, false, 40)
	if err != nil || !strings.Contains(srv, "serve out") {
		t.Fatalf("serve: %v %q", err, srv)
	}
}

func TestLogsUnknownID(t *testing.T) {
	dataDir := t.TempDir()
	for _, id := range []string{"nope", "../etc", ""} {
		_, err := runLogsString(t, dataDir, id, false, false, 40)
		if err == nil || !strings.Contains(err.Error(), "no request or run "+id+" in "+dataDir) {
			t.Fatalf("id %q: %v", id, err)
		}
	}
}

func TestLogsStripsControlSequences(t *testing.T) {
	dataDir := t.TempDir()
	writeLog(t, filepath.Join(dataDir, "logs", "serve.out.log"),
		"\x1b[31mred\x1b[0m\x1b]0;title\x07 ok\tTab\rover\x08\x00 end\n", 0)
	out, err := runLogsString(t, dataDir, "serve", false, false, 40)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07\r\x08\x00") {
		t.Fatalf("control bytes survived: %q", out)
	}
	if !strings.Contains(out, "red ok\tTabover end\n") {
		t.Fatalf("printable text lost: %q", out)
	}
}

func TestLogsFollowSwitchesToNewerFileAndExitsWhenTerminal(t *testing.T) {
	old := logsPollInterval
	logsPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })

	dataDir, r := logsFixture(t)
	var buf lockedBuffer
	done := make(chan error, 1)
	go func() { done <- runLogs(context.Background(), &buf, dataDir, "req1", false, true, 40) }()

	waitFor := func(sub string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(buf.String(), sub) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q in:\n%s", sub, buf.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("==> runs/run-b/verify.log")

	// Appended bytes arrive; a partial line waits for its newline.
	f, err := os.OpenFile(filepath.Join(run.Dir(dataDir, "run-b"), "verify.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("l4 par"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if strings.Contains(buf.String(), "l4") {
		t.Fatal("partial line printed early")
	}
	if _, err := f.WriteString("tial\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	waitFor("l4 partial")

	// The next phase's log is newer: the header switches.
	next := filepath.Join(run.Dir(dataDir, "run-b"), "full_suite_verify.log")
	// The appends above stamped verify.log with the real clock, so the new
	// file must be newer than that, not just newer than the fixture base.
	writeLog(t, next, "suite ok\n", time.Since(logsBase)+time.Hour)
	waitFor("==> runs/run-b/full_suite_verify.log")
	waitFor("suite ok")

	select {
	case err := <-done:
		t.Fatalf("exited before terminal state: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	r.State = request.StateDone
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not exit at terminal state")
	}
}

func TestLogsFollowOnTerminalRunPrintsTailAndExits(t *testing.T) {
	dataDir := t.TempDir()
	rn := &run.Run{ID: "run-x", State: run.StateAccepted}
	if err := rn.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	writeLog(t, filepath.Join(run.Dir(dataDir, "run-x"), "verify.log"), "done\n", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var buf lockedBuffer
	if err := runLogs(ctx, &buf, dataDir, "run-x", false, true, 40); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || !strings.Contains(buf.String(), "done") {
		t.Fatalf("ctx=%v out=%q", ctx.Err(), buf.String())
	}
}

func TestLogsDefaultSkipsNotificationsAndListShowsEarlierRuns(t *testing.T) {
	dataDir := t.TempDir()
	r := request.New("req-x", "/ws", "proj", request.Source{}, logsBase)
	r.State = request.StateQuarantined
	r.TicketIndex, r.TicketCount = 1, 1
	r.Tickets = []request.Ticket{{Index: 1, RunID: "req-x-001-b"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	// req-x-001-a is an earlier run of ticket 1 that the record no longer links.
	writeLog(t, filepath.Join(run.Dir(dataDir, "req-x-001-a"), "build_app.log"), "crashed build\n", time.Minute)
	writeLog(t, filepath.Join(run.Dir(dataDir, "req-x-001-b"), "verify.log"), "verify failed\n", 2*time.Minute)
	writeLog(t, filepath.Join(request.Dir(dataDir, "req-x"), "notifications.log"), "{\"state\":\"quarantined\"}\n", 3*time.Minute)

	out, err := runLogsString(t, dataDir, "req-x", false, false, 5)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(out, "req-x-001-b/verify.log") || strings.Contains(out, "notifications.log") {
		t.Errorf("default pick should be the newest work log (verify.log), not notifications.log:\n%s", out)
	}

	out, err = runLogsString(t, dataDir, "req-x", true, false, 5)
	if err != nil {
		t.Fatalf("logs -list: %v", err)
	}
	for _, want := range []string{"runs/req-x-001-a/build_app.log", "runs/req-x-001-b/verify.log", "requests/req-x/notifications.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("-list missing %s:\n%s", want, out)
		}
	}
}

func TestLogsListsSavedPromptsAndPrintsOneWithTerminalEscapesStripped(t *testing.T) {
	dataDir := t.TempDir()
	runDir := run.Dir(dataDir, "run-p")
	if err := os.MkdirAll(filepath.Join(runDir, "prompts", "build-1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "prompts", "build-2"), 0o750); err != nil {
		t.Fatal(err)
	}
	for attempt, text := range map[string]string{"build-1": "first\n\x1b[31mred\x1b[0m\n", "build-2": "second\n"} {
		if err := os.WriteFile(filepath.Join(runDir, "prompts", attempt, "build-round-1.md"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "build_app.log"), []byte("a log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&run.Run{ID: "run-p", State: run.StateQuarantined}).Save(dataDir); err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := runLogs(context.Background(), &list, dataDir, "run-p", true, false, 40); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"prompts/build-1/build-round-1.md", "prompts/build-2/build-round-1.md", "[prompt]", "build_app.log"} {
		if !strings.Contains(list.String(), want) {
			t.Errorf("-list lacks %q:\n%s", want, list.String())
		}
	}
	// A prompt is never the "newest log": the default shows the build log.
	var newest bytes.Buffer
	if err := runLogs(context.Background(), &newest, dataDir, "run-p", false, false, 40); err != nil || !strings.Contains(newest.String(), "a log") || strings.Contains(newest.String(), "first") {
		t.Errorf("default = %q, %v", newest.String(), err)
	}

	var one bytes.Buffer
	if err := runLogsPrompt(&one, dataDir, "run-p", "build-1/build-round-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one.String(), "first") || !strings.Contains(one.String(), "red") || strings.Contains(one.String(), "\x1b") || strings.Contains(one.String(), "second") {
		t.Errorf("-prompt build-1/build-round-1 = %q", one.String())
	}
	var both bytes.Buffer
	if err := runLogsPrompt(&both, dataDir, "run-p", "build-round-1"); err != nil || !strings.Contains(both.String(), "first") || !strings.Contains(both.String(), "second") {
		t.Errorf("-prompt build-round-1 = %q, %v, want every attempt's", both.String(), err)
	}
	for _, bad := range []string{"nothing", "../x", "build-1/../build-2/build-round-1", "build-1/build-round-1.md"} {
		if err := runLogsPrompt(&bytes.Buffer{}, dataDir, "run-p", bad); err == nil {
			t.Errorf("-prompt %q: want an error", bad)
		}
	}
	if err := runLogsPrompt(&bytes.Buffer{}, dataDir, "serve", "build-round-1"); err == nil {
		t.Error("-prompt on a process log: want an error")
	}
}
