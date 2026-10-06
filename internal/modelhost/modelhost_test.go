package modelhost

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShouldLockPlaintextUpstream(t *testing.T) {
	if !ShouldLock("http://127.0.0.1:8080") {
		t.Error("ShouldLock(http://) = false, want true (non-TLS always locks)")
	}
}

func TestShouldLockPrivateAddresses(t *testing.T) {
	cases := []string{
		"https://127.0.0.1:8080",
		"https://localhost:8080",
		"https://10.1.2.3",
		"https://172.16.0.5",
		"https://192.168.1.5",
		"https://100.64.0.1",      // Tailscale CGNAT low end
		"https://100.101.1.2",     // real Tailscale address from a private notes file
		"https://100.127.255.254", // Tailscale CGNAT high end
		"https://model-host.example.ts.net",
		"https://my-box.lan",
		"https://my-box.local",
	}
	for _, upstream := range cases {
		if !ShouldLock(upstream) {
			t.Errorf("ShouldLock(%q) = false, want true", upstream)
		}
	}
}

func TestShouldLockRemoteSaaSUpstreams(t *testing.T) {
	cases := []string{
		"https://chatgpt.com/backend-api/codex",
		"https://api.githubcopilot.com",
		"https://api.anthropic.com",
	}
	for _, upstream := range cases {
		if ShouldLock(upstream) {
			t.Errorf("ShouldLock(%q) = true, want false (public HTTPS SaaS host)", upstream)
		}
	}
}

func TestShouldLockUnparseableFailsClosed(t *testing.T) {
	if !ShouldLock("::not a url::") {
		t.Error("ShouldLock(malformed) = false, want true (fail closed)")
	}
	if !ShouldLock("") {
		t.Error("ShouldLock(\"\") = false, want true (fail closed)")
	}
}

func TestLockPathSlotZeroMatchesDocumentedShape(t *testing.T) {
	path := LockPath("/x/locks", "https://100.101.1.2:8080", 0)
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "model-") || !strings.HasSuffix(base, ".lock") || strings.Contains(base, "slot") {
		t.Errorf("LockPath slot 0 = %q, want model-<hash>.lock shape", base)
	}
}

func TestLockPathDifferentHostsDifferentKeys(t *testing.T) {
	a := LockPath("/x/locks", "https://host-a:8080", 0)
	b := LockPath("/x/locks", "https://host-b:8080", 0)
	if a == b {
		t.Errorf("LockPath produced the same path for two different hosts: %q", a)
	}
}

func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestAcquireDisabledByZeroConcurrency(t *testing.T) {
	withTempHome(t)
	handle, err := Acquire(context.Background(), "https://100.64.1.1", "run-1", 0, nil)
	if err != nil {
		t.Fatalf("Acquire with concurrency 0: %v", err)
	}
	if handle != nil {
		t.Fatal("Acquire with concurrency 0 returned a non-nil handle")
	}
	locksDir, err := LocksDir()
	if err != nil {
		t.Fatalf("LocksDir: %v", err)
	}
	if _, statErr := os.Stat(locksDir); statErr == nil {
		t.Error("Acquire with concurrency 0 created the locks directory; it should be a pure no-op")
	}
}

// TestAcquireSerializesGoroutinesAndReportsHolder is the concurrency=1
// case: a second Acquire call blocks until the first Handle is released,
// and onWait is told who currently holds the lock while it waits.
func TestAcquireSerializesGoroutinesAndReportsHolder(t *testing.T) {
	withTempHome(t)
	upstream := "https://100.64.1.2"

	first, err := Acquire(context.Background(), upstream, "run-first", 1, nil)
	if err != nil {
		t.Fatalf("Acquire(first): %v", err)
	}
	if first == nil {
		t.Fatal("Acquire(first) returned a nil handle with concurrency 1")
	}

	var waitedHolder string
	var mu sync.Mutex
	onWait := func(holder string) {
		mu.Lock()
		defer mu.Unlock()
		if waitedHolder == "" {
			waitedHolder = holder
		}
	}

	secondDone := make(chan struct{})
	var second *Handle
	var secondErr error
	go func() {
		second, secondErr = Acquire(context.Background(), upstream, "run-second", 1, onWait)
		close(secondDone)
	}()

	// Give the second goroutine time to observe the lock as busy and
	// call onWait at least once before releasing the first holder.
	time.Sleep(3 * pollInterval / 2)

	mu.Lock()
	holderSeen := waitedHolder
	mu.Unlock()
	if !strings.Contains(holderSeen, "run-first") {
		t.Errorf("onWait holder = %q, want it to mention run-first", holderSeen)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release(first): %v", err)
	}

	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second Acquire did not complete after first was released")
	}
	if secondErr != nil {
		t.Fatalf("Acquire(second): %v", secondErr)
	}
	if second == nil {
		t.Fatal("Acquire(second) returned a nil handle after acquiring")
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release(second): %v", err)
	}
}

// TestAcquireConcurrencyTwoAllowsTwoHolders proves a configured
// concurrency above 1 lets that many callers through simultaneously
// (e.g. an operator who knows their model host can serve N requests at
// once), and a third still waits.
func TestAcquireConcurrencyTwoAllowsTwoHolders(t *testing.T) {
	withTempHome(t)
	upstream := "https://100.64.1.3"

	first, err := Acquire(context.Background(), upstream, "run-1", 2, nil)
	if err != nil || first == nil {
		t.Fatalf("Acquire(first): handle=%v err=%v", first, err)
	}
	defer first.Release()
	second, err := Acquire(context.Background(), upstream, "run-2", 2, nil)
	if err != nil || second == nil {
		t.Fatalf("Acquire(second): handle=%v err=%v", second, err)
	}
	defer second.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	third, err := Acquire(ctx, upstream, "run-3", 2, nil)
	if third != nil {
		t.Error("Acquire(third) succeeded with concurrency 2 already fully held")
	}
	if err == nil {
		t.Error("Acquire(third) returned nil error while both slots were held")
	}
}

func TestAcquireContextCanceledStopsWaiting(t *testing.T) {
	withTempHome(t)
	upstream := "https://100.64.1.4"
	first, err := Acquire(context.Background(), upstream, "run-1", 1, nil)
	if err != nil || first == nil {
		t.Fatalf("Acquire(first): handle=%v err=%v", first, err)
	}
	defer first.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handle, err := Acquire(ctx, upstream, "run-2", 1, nil)
	if handle != nil {
		t.Error("Acquire returned a handle despite an already-canceled context")
	}
	if err == nil {
		t.Error("Acquire returned nil error despite an already-canceled context")
	}
}

// TestAcquireExcludesRealSecondProcess proves the lock is a real OS-level
// flock, not merely an in-process mutex: a second, genuinely separate
// process contending for the same upstream must see it as busy. Follows
// the exec.Command(os.Args[0], "-test.run", ...) self-re-exec pattern
// internal/workspace/lock_test.go already uses for the identical proof
// on DirectLock.
func TestAcquireExcludesRealSecondProcess(t *testing.T) {
	home := withTempHome(t)
	upstream := "https://100.64.1.5"

	first, err := Acquire(context.Background(), upstream, "run-owner", 1, nil)
	if err != nil || first == nil {
		t.Fatalf("Acquire(first): handle=%v err=%v", first, err)
	}
	defer first.Release()

	cmd := exec.Command(os.Args[0], "-test.run", "^TestAcquireHelperProcess$")
	cmd.Env = append(os.Environ(),
		"FACTORYD_MODELHOST_HELPER=1",
		"HOME="+home,
		"FACTORYD_MODELHOST_UPSTREAM="+upstream,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v: %s", err, out)
	}
	// The helper's own test binary appends its usual "PASS\nok  ...\n"
	// report after our marker line, so check for the marker rather than
	// an exact match.
	if got := string(out); !strings.Contains(got, "\nbusy\n") && !strings.HasPrefix(got, "busy\n") {
		t.Fatalf("helper process output %q, want it to report \"busy\" while this process holds the lock", got)
	}
}

// TestAcquireHelperProcess is not a real test; it is
// TestAcquireExcludesRealSecondProcess's own re-exec target, gated on an
// environment variable so a normal `go test` run treats it as a no-op.
func TestAcquireHelperProcess(t *testing.T) {
	if os.Getenv("FACTORYD_MODELHOST_HELPER") != "1" {
		t.Skip("only runs as TestAcquireExcludesRealSecondProcess's helper subprocess")
	}
	upstream := os.Getenv("FACTORYD_MODELHOST_UPSTREAM")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	handle, err := Acquire(ctx, upstream, "run-helper", 1, nil)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	if handle != nil {
		_ = handle.Release()
		_, _ = w.WriteString("acquired\n")
		return
	}
	if err != nil {
		_, _ = w.WriteString("busy\n")
		return
	}
	_, _ = w.WriteString("unexpected\n")
}
