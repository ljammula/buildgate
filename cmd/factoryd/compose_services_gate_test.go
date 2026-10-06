package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/progress"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/testfixture"
)

// composeGateRepo commits compose (when non-empty) in a fresh fixture repo
// and returns the repo and its HEAD.
func composeGateRepo(t *testing.T, compose string) (string, string) {
	t.Helper()
	repo := testfixture.NewGitRepo(t)
	if compose != "" {
		if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "compose.yaml"}, {"commit", "-q", "-m", "compose"}} {
			if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(head))
}

func composeGateSettings(concurrency int) sessionconfig.Settings {
	settings := sessionconfig.DefaultSettings()
	settings.ComposeServicesConcurrency = concurrency
	return settings
}

// Two runs with sidecars, from any data dir or process, serialize through
// the host-wide slot, and the waiting one says so in its progress feed.
func TestComposeServicesGateSerializesRunsAndEmitsProgress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo, head := composeGateRepo(t, "services:\n  db:\n    image: postgres:16\n")
	settings := composeGateSettings(1)
	firstDataDir, secondDataDir := t.TempDir(), t.TempDir()

	first, err := acquireComposeServicesGate(context.Background(), firstDataDir, "run-first", settings, true, repo, head)
	if err != nil || first == nil {
		t.Fatalf("acquireComposeServicesGate(first) = %v, %v; want a held slot", first, err)
	}

	secondDone := make(chan error, 1)
	go func() {
		handle, err := acquireComposeServicesGate(context.Background(), secondDataDir, "run-second", settings, true, repo, head)
		if handle != nil {
			_ = handle.Release()
		}
		secondDone <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(progress.Path(secondDataDir, "run-second"))
		if strings.Contains(string(b), `"stage":"compose_services_lock"`) && strings.Contains(string(b), "queued behind run-first") {
			break
		}
		select {
		case <-secondDone:
			t.Fatal("the second run took the slot while the first still held it")
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("run-second's progress never recorded a compose_services_lock \"queued behind run-first\" event")
		}
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release(first): %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("acquireComposeServicesGate(second): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second run never took the slot after the first released it")
	}
	b, _ := os.ReadFile(progress.Path(secondDataDir, "run-second"))
	if !strings.Contains(string(b), `"stage":"compose_services_lock","event":"acquired"`) {
		t.Errorf("run-second's progress lacks the acquired event that clears its waiting reason:\n%s", b)
	}
}

// No slot applies when nothing will launch: a rejected compose file must
// halt at its build rather than queue behind another run first.
func TestComposeServicesGateSkipsRunsThatLaunchNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	valid := "services:\n  db:\n    image: postgres:16\n"
	for _, tc := range []struct {
		name        string
		compose     string
		enabled     bool
		concurrency int
	}{
		{"compose services off", valid, false, 1},
		{"slot disabled", valid, true, 0},
		{"no compose file", "", true, 1},
		{"rejected file", "services:\n  kafka:\n    image: bitnami/kafka:3.7\n", true, 1},
		{"only build services", "services:\n  app:\n    build: .\n", true, 1},
	} {
		repo, head := composeGateRepo(t, tc.compose)
		handle, err := acquireComposeServicesGate(context.Background(), t.TempDir(), "run-1", composeGateSettings(tc.concurrency), tc.enabled, repo, head)
		if handle != nil || err != nil {
			t.Errorf("%s: acquireComposeServicesGate = %v, %v; want nil, nil", tc.name, handle, err)
		}
	}
}
