package triage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// writeRunFile writes content to name inside the run's own durable
// directory (run.Dir(dataDir, id)), creating it as needed -- mirrors
// where run_ticket.go itself writes spec.snapshot.md, attempt logs, and
// diff.patch.
func writeRunFile(t *testing.T, dataDir, id, name, content string) {
	t.Helper()
	dir := run.Dir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestTriageRun is table-driven over triageRun's own gate/halt priority
// order and every runner-log convention it recognizes. Each case builds
// only the run.Run fields and durable run-directory files (spec snapshot,
// attempt logs, diff.patch) triageRun actually reads, per its own doc
// comment: never guess from anything else.
func TestTriageRun(t *testing.T) {
	const id = "run1"

	tests := []struct {
		name  string
		setup func(t *testing.T, dataDir string) *run.Run
		want  string
	}{
		{
			name: "canonical_verify go test failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "verify.log", "ok  \tpkg/a\t0.01s\n--- FAIL: TestRefund_Idempotent (0.00s)\n    x_test.go:12: boom\nFAIL\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"internal/refund/refund.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 0, LogPath: filepath.Join(run.Dir(dataDir, id), "build_app.log")},
						{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			want: "canonical_verify failed: TestRefund_Idempotent (go test)",
		},
		{
			name: "canonical_verify pytest FAILED line",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "verify.log", "collected 3 items\n\nFAILED tests/test_x.py::test_thing - AssertionError\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"app.py"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 0},
						{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			want: "canonical_verify failed: tests/test_x.py::test_thing (pytest)",
		},
		{
			name: "canonical_verify pytest underscore block",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "verify.log", "collected 1 item\n\n________________ test_thing ________________\n\nAssertionError\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"app.py"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			want: "canonical_verify failed: test_thing (pytest)",
		},
		{
			name: "unit_tests jest bullet failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "unit_tests.log", "PASS src/a.test.js\nFAIL src/b.test.js\n  ● renders the widget\n\n    expect(received).toBe(expected)\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"src/b.js"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "unit_tests", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "unit_tests", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "unit_tests.log")},
					},
				}
			},
			want: "unit_tests failed: renders the widget (jest)",
		},
		{
			name: "lint flutter analyze failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "lint.log", "00:03 +5 -1: some widget test [E]\n  Expected: true\n  Actual: false\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"lib/main.dart"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "lint", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "lint", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "lint.log")},
					},
				}
			},
			want: "lint failed: some widget test (flutter test)",
		},
		{
			name: "security_audit cargo test failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "security_audit.log", "running 2 tests\ntest audit::checks_deny_list ... FAILED\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"src/audit.rs"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "security_audit", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "security_audit", ExitCode: 101, LogPath: filepath.Join(run.Dir(dataDir, id), "security_audit.log")},
					},
				}
			},
			want: "security_audit failed: audit::checks_deny_list (cargo test)",
		},
		{
			name: "canonical_verify build failure uses build log, not verify",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "build_app.log", "Traceback (most recent call last):\n--- FAIL: TestBuildBoom\n")
				writeRunFile(t, dataDir, id, "verify.log", "ok\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"main.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "build_app.log")},
						{Kind: "verify", ExitCode: 0, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			want: "canonical_verify failed: TestBuildBoom (go test)",
		},
		{
			name: "canonical_verify no marker falls back to exit code",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "verify.log", "some unrecognized tool output\nexit status 2\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"main.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 0},
						{Kind: "verify", ExitCode: 2, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			want: "canonical_verify failed: exit 2",
		},
		{
			// Fixture is the real verify.log from a live quarantined run (a
			// compile error inside a *_test.go file, not a runtime test
			// failure -- no "--- FAIL:" marker exists, only the go build/vet
			// line).
			name: "canonical_verify go build compile error",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "verify.log", "internal/habit/habit_current_streak_test.go:148:2: expected '}', found 'EOF'\nchmod: changing permissions of '/workspace/.git': Read-only file system\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"internal/habit/habit_current_streak_test.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 0},
						{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			// A whole-line marker (untrusted log text) is presented as an
			// explicit quote from the log, never as a factory verdict
			// phrased the way the old bounded test-name markers
			// (go test/pytest/jest/...) still are.
			want: `canonical_verify failed; first error in log: "internal/habit/habit_current_streak_test.go:148:2: expected '}', found 'EOF'"`,
		},
		{
			// Matches a live walk's actual GateResults shape --
			// canonical_verify is the first failing gate (diff_scope and
			// required_files_changed also failed), so its sentence must
			// name that instead of reading as the run's only problem.
			name: "canonical_verify first names how many other gates also failed",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "verify.log", "internal/habit/habit_current_streak_test.go:148:2: expected '}', found 'EOF'\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"internal/habit/habit_current_streak_test.go"},
					GateResults: []run.GateResult{
						{Check: "canonical_verify", Passed: false},
						{Check: "diff_scope", Passed: false},
						{Check: "required_files_changed", Passed: false},
					},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 0},
						{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
					},
				}
			},
			want: `canonical_verify failed; first error in log: "internal/habit/habit_current_streak_test.go:148:2: expected '}', found 'EOF'" (+2 more failing gates)`,
		},
		{
			name: "canonical_verify no changes across build rounds",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: false}},
					Attempts: []run.Attempt{
						{Kind: "build", ExitCode: 0},
						{Kind: "build", ExitCode: 0},
						{Kind: "build", ExitCode: 0},
						{Kind: "verify", ExitCode: 1},
					},
				}
			},
			want: "build: agent made no changes in 3 rounds",
		},
		{
			name: "diff_scope offending path",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nAllowed-Files: internal/refund/refund.go\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"internal/refund/refund.go", "internal/auth/x.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "diff_scope", Passed: false}},
				}
			},
			want: "diff_scope: changed internal/auth/x.go outside Allowed-Files",
		},
		{
			name: "required_files_changed missing file",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nRequired-Changed-Files: docs/CHANGELOG.md\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"internal/refund/refund.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "required_files_changed", Passed: false}},
				}
			},
			want: "required_files_changed: docs/CHANGELOG.md untouched",
		},
		{
			name: "required_content_present missing marker",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nRequired-Content: Key('reminder-quick-add')\n")
				writeRunFile(t, dataDir, id, "diff.patch", "diff --git a/lib/main.dart b/lib/main.dart\n--- a/lib/main.dart\n+++ b/lib/main.dart\n+Widget build() {\n+  return Text('hi');\n+}\n")
				return &run.Run{
					ID:            id,
					State:         run.StateQuarantined,
					GateResults:   []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "required_content_present", Passed: false}},
					DiffAvailable: true,
				}
			},
			want: `required_content_present: missing "Key('reminder-quick-add')"`,
		},
		{
			name: "tests_added no test file changed",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"internal/refund/refund.go"},
					GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "tests_added", Passed: false}},
				}
			},
			want: "tests_added: no test file changed",
		},
		{
			name: "tests_added opted out",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{
					ID:                  id,
					State:               run.StateQuarantined,
					ChangedFiles:        []string{"internal/refund/refund.go"},
					GateResults:         []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "tests_added", Passed: false}},
					TestsRequiredOptOut: "pure config change, no behavior to test",
				}
			},
			want: "tests_added: opted out (pure config change, no behavior to test)",
		},
		{
			name: "first failed gate wins over a later one",
			setup: func(t *testing.T, dataDir string) *run.Run {
				writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nAllowed-Files: a.go\n")
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"a.go", "b.go"},
					GateResults: []run.GateResult{
						{Check: "canonical_verify", Passed: true},
						{Check: "diff_scope", Passed: false},
						{Check: "tests_added", Passed: false},
					},
				}
			},
			want: "diff_scope: changed b.go outside Allowed-Files",
		},
		{
			name: "halted relay ceiling exceeded",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltReasonCode: run.HaltReasonRelayCeilingExceeded, HaltError: "relay budget exceeded: 12000 tokens"}
			},
			want: "halted: relay budget ceiling exceeded",
		},
		{
			name: "halted sandbox attempts exhausted",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "sandbox attempts exhausted: context deadline exceeded"}
			},
			want: "halted: sandbox attempts exhausted",
		},
		{
			name: "halted docker mount failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "resolve sandbox workspace mount: symlinked mount path is forbidden"}
			},
			want: "halted: Docker mount/visibility failure",
		},
		{
			name: "halted image pull failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "docker compose pull: exit status 1: manifest unknown"}
			},
			want: "halted: sandbox image pull failure",
		},
		{
			name: "halted compose file rejected",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltReasonCode: run.HaltReasonComposeServicesRejected, HaltError: "compose file rejected: kafka: image \"bitnami/kafka:3.7\" is not under an allow-listed registry/namespace"}
			},
			want: "halted: the target repo's compose file was rejected before the build; fix the named services or compose_services_* config and retry",
		},
		{
			name: "halted on the baseline verify",
			setup: func(t *testing.T, dataDir string) *run.Run {
				record := &run.BaselineVerify{Command: "pytest", ExitCode: 1, FailingTests: []string{"tests/test_pager.py::test_less"}, FailingCount: 1, Unnamed: []string{"tests/test_pager.py::test_less"}, UnnamedCount: 1}
				return &run.Run{ID: id, State: run.StateHalted, HaltReasonCode: run.HaltReasonBaselineVerifyFailed, BaselineVerify: record, HaltError: "temporal workflow did not complete: baseline verify activity: ..."}
			},
			want: "halted before the build: baseline verify failed: tests/test_pager.py::test_less; the ticket does not name it",
		},
		{
			name: "halted on a .factory directory the mount cannot carry",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltReasonCode: run.HaltReasonFactoryDirFailed, HaltError: "temporal workflow did not complete: ...",
					Attempts: []run.Attempt{{Kind: "baseline_verify", ExitCode: -1, FactoryDirError: ".factory in the worktree is not a directory (L---------)"}}}
			},
			want: `halted (operator finding): .factory/ cannot be mounted read-only: ".factory in the worktree is not a directory (L---------)"`,
		},
		{
			name: "halted on a .factory directory with no recorded reason",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltReasonCode: run.HaltReasonFactoryDirFailed, HaltError: "temporal workflow did not complete: ..."}
			},
			want: "halted (operator finding): .factory/ cannot be mounted read-only as the commit .factory.yml was read from holds it; see the refused attempt's record",
		},
		{
			name: "halted waiting for the compose sidecar slot",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "acquire compose services slot: timed out waiting for the host-wide compose sidecar slot (compose_services_concurrency) held by run-a (pid 7): context deadline exceeded"}
			},
			want: "halted: queued behind another run's compose sidecars (compose_services_concurrency) for a whole run's timeout; retry once that run finishes",
		},
		{
			name: "halted compose services failed to start",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "sandbox attempts exhausted: start compose services: docker compose up: exit status 1: container kafka exited (1) (service logs: /data/runs/r/attempt-3/compose/verify)"}
			},
			want: "halted: compose services failed to start; service logs are under attempt-N/compose/",
		},
		{
			name: "halted compose image pull failure",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "pull compose images: docker compose pull: exit status 1: toomanyrequests"}
			},
			want: "halted: compose service image pull failure",
		},
		{
			name: "halted timeout",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "relay container did not report listening within 30s"}
			},
			want: "halted: sandbox/relay timeout",
		},
		{
			// The worker process died or froze mid-activity. Must not
			// fall into the generic "timeout" case above, which blames the
			// sandbox/relay.
			name: "halted worker stopped mid-run",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "child workflow execution error (type: RunWorkflow): activity error (type: RunVerifyActivity, scheduledEventID: 11, startedEventID: 12, identity: 4242@host@): activity Heartbeat timeout (type: Heartbeat)"}
			},
			want: workerStoppedTriage,
		},
		{
			name: "halted unclassified falls back to first line",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted, HaltError: "context canceled\nmore detail on a second line"}
			},
			want: "halted: context canceled",
		},
		{
			name: "halted with nothing recorded returns empty",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateHalted}
			},
			want: "",
		},
		{
			name: "accepted run has no triage",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{ID: id, State: run.StateAccepted}
			},
			want: "",
		},
		{
			name: "diff_scope with no spec snapshot never guesses",
			setup: func(t *testing.T, dataDir string) *run.Run {
				return &run.Run{
					ID:           id,
					State:        run.StateQuarantined,
					ChangedFiles: []string{"a.go", "b.go"},
					GateResults:  []run.GateResult{{Check: "diff_scope", Passed: false}},
				}
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			r := tt.setup(t, dataDir)
			got := Run(r, dataDir)
			if got != tt.want {
				t.Errorf("Run() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTriageRunCapsSentenceLength proves truncateTriage's own 200-char
// cap is actually applied end to end, not just unit-tested in isolation.
func TestTriageRunCapsSentenceLength(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	var allowed []string
	var changed []string
	// Build an Allowed-Files list long enough that every changed file is
	// simultaneously out of scope and, joined together, well past 200
	// chars.
	allowed = append(allowed, "only/this/file/is/allowed.go")
	for i := 0; i < 20; i++ {
		changed = append(changed, "internal/pkg/some/fairly/long/generated/path/file_number_"+string(rune('a'+i))+".go")
	}
	writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nAllowed-Files: "+allowed[0]+"\n")
	r := &run.Run{
		ID:           id,
		State:        run.StateQuarantined,
		ChangedFiles: changed,
		GateResults:  []run.GateResult{{Check: "diff_scope", Passed: false}},
	}
	got := Run(r, dataDir)
	if len(got) > maxTriageSentenceLen {
		t.Fatalf("Run() returned %d bytes, want <= %d: %q", len(got), maxTriageSentenceLen, got)
	}
	if got == "" {
		t.Fatal("Run() = \"\", want a truncated but non-empty sentence")
	}
}

// TestTriageRunNeverDropsOrCutsSuffix is the regression test proving that
// a realistic long Go compile error, plus enough other failing gates to
// make "(+N more failing gates)" itself non-trivial, must never leave
// truncateTriage's 200-byte cap with no room for the suffix --
// appending the suffix to an already-full sentence and letting the cap
// run on the combined string could silently drop it or cut it mid-word.
func TestTriageRunNeverDropsOrCutsSuffix(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	longLine := "internal/workflow/activities.go:1517:10: cannot use res.SomeVeryLongFieldNameHereToPadThisOutFurther (variable of type int) as string value in struct literal"
	writeRunFile(t, dataDir, id, "verify.log", longLine+"\n")
	r := &run.Run{
		ID:           id,
		State:        run.StateQuarantined,
		ChangedFiles: []string{"internal/workflow/activities.go"},
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: false},
			{Check: "diff_scope", Passed: false},
			{Check: "required_files_changed", Passed: false},
			{Check: "required_content_present", Passed: false},
			{Check: "tests_added", Passed: false},
			{Check: "spec_conformity", Passed: false},
		},
		Attempts: []run.Attempt{
			{Kind: "build", ExitCode: 0},
			{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
		},
	}
	got := Run(r, dataDir)
	if len(got) > maxTriageSentenceLen {
		t.Fatalf("Run() returned %d bytes, want <= %d: %q", len(got), maxTriageSentenceLen, got)
	}
	const wantSuffix = "(+5 more failing gates)"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("Run() = %q, want it to end with %q intact, not dropped or cut", got, wantSuffix)
	}
}

// TestTriageRunQuotedMarkerAlwaysClosesItsQuoteAndKeepsTheSuffix proves
// that a 45-byte prefix plus a 160-byte (maxTriageMarkerLen) marker plus
// two quote characters already exceeds the 200-byte cap on its own,
// before %q escaping or the "(+N more failing gates)" suffix even enter
// into it -- the earlier fitTriageSuffix-only fix truncated the
// FINISHED sentence from the end, which could (and did, with a marker
// anywhere near maxTriageMarkerLen's own 160-byte cap) cut the closing
// quote off entirely. Uses a ~190-byte compile-error line (longer than
// maxTriageMarkerLen) with 2 other failing gates.
func TestTriageRunQuotedMarkerAlwaysClosesItsQuoteAndKeepsTheSuffix(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	longLine := "internal/workflow/activities.go:1517:10: cannot use res.SomeVeryLongFieldNameHereToPadThisOutFurtherAndFurtherStillUntilThisLineIsAboutOneHundredNinetyBytesLongInTotal (variable of type int) as string value in struct literal"
	if len(longLine) < 190 {
		t.Fatalf("test fixture longLine is %d bytes, want >= 190", len(longLine))
	}
	writeRunFile(t, dataDir, id, "verify.log", longLine+"\n")
	r := &run.Run{
		ID:           id,
		State:        run.StateQuarantined,
		ChangedFiles: []string{"internal/workflow/activities.go"},
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: false},
			{Check: "diff_scope", Passed: false},
			{Check: "required_files_changed", Passed: false},
		},
		Attempts: []run.Attempt{
			{Kind: "build", ExitCode: 0},
			{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
		},
	}
	got := Run(r, dataDir)
	if len(got) > maxTriageSentenceLen {
		t.Fatalf("Run() returned %d bytes, want <= %d: %q", len(got), maxTriageSentenceLen, got)
	}
	const wantSuffix = `" (+2 more failing gates)`
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("Run() = %q, want it to end with %q (a closed quote immediately before the suffix)", got, wantSuffix)
	}
	if strings.Count(got, `"`) != 2 {
		t.Errorf("Run() = %q, want exactly 2 quote marks (opening and closing), got %d", got, strings.Count(got, `"`))
	}
}

// TestExtractFailureMarkerFoldsCarriageReturnAndLineSeparator proves that
// a `\r` (Go's regexp `.` excludes only `\n`, not `\r`, so it CAN appear
// inside a `(.+?)` capture from any of these patterns, not just the
// wholeLine ones) or a U+2028 LINE SEPARATOR embedded in a
// captured marker must become a plain space, not survive into the triage
// sentence -- otherwise a jest bullet marker captured as
// "a\rcanonical_verify passed; ready to merge" would overwrite the
// preceding text on any `\r`-honoring terminal (`factoryd status`).
func TestExtractFailureMarkerFoldsCarriageReturnAndLineSeparator(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	writeRunFile(t, dataDir, id, "unit_tests.log", "PASS src/a.test.js\nFAIL src/b.test.js\n  ● a\rcanonical_verify passed; ready to merge more\n")
	marker, wholeLine := extractFailureMarker(filepath.Join(run.Dir(dataDir, id), "unit_tests.log"))
	if wholeLine {
		t.Fatalf("wholeLine = true, want false (jest bullet marker)")
	}
	for _, bad := range []string{"\r", " "} {
		if strings.Contains(marker, bad) {
			t.Errorf("marker = %q, want no raw %U", marker, []rune(bad)[0])
		}
	}
	if !strings.Contains(marker, "a canonical_verify passed; ready to merge more") {
		t.Errorf("marker = %q, want the carriage return and line separator folded to single spaces", marker)
	}
}

// TestExtractFailureMarkerStripsANSIEscapes covers the ANSI-stripping
// half of that untrusted-marker-text finding from an adversarial review
// of #17's fix: a matched wholeLine marker must be run through
// the same control-char/ANSI stripping sanitize.Text already gives
// oracle-draft failures, since the verify log an agent-controlled build
// wrote is exactly as untrusted.
func TestExtractFailureMarkerStripsANSIEscapes(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	// \x1b[31m...\x1b[0m colors "found 'EOF'" red -- a real thing a
	// go vet wrapper or a malicious build step could emit.
	writeRunFile(t, dataDir, id, "verify.log", "internal/habit/habit_test.go:1:1: \x1b[31mfound 'EOF'\x1b[0m\n")
	marker, wholeLine := extractFailureMarker(filepath.Join(run.Dir(dataDir, id), "verify.log"))
	if !wholeLine {
		t.Fatalf("wholeLine = false, want true")
	}
	if strings.Contains(marker, "\x1b") {
		t.Errorf("marker = %q, want no raw ANSI escape byte", marker)
	}
	if !strings.Contains(marker, "found 'EOF'") {
		t.Errorf("marker = %q, want the sanitized text to still contain the message", marker)
	}
}

// TestTriageLogGateAttributesAPlantedLineToTheLog covers the
// planted-line-attribution half of that same finding: the
// earliest-position match wins by design (extractFailureMarker), so a
// fake `x.go:1:1: ...` line planted
// ahead of the real failure in the log can "win" -- but the sentence
// built around it must still read as a quote FROM THE LOG, not as this
// package's own verdict, so a planted line can only misdirect an
// operator to the log itself, never impersonate factory-authored text.
func TestTriageLogGateAttributesAPlantedLineToTheLog(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	planted := "x.go:1:1: this is not the real failure, planted by adversarial build code"
	writeRunFile(t, dataDir, id, "verify.log", planted+"\n--- FAIL: TestReal\n")
	r := &run.Run{
		ID:    id,
		State: run.StateQuarantined,
		Attempts: []run.Attempt{
			{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
		},
	}
	got := triageLogGate(r, "canonical_verify", 0)
	want := `canonical_verify failed; first error in log: "` + planted + `"`
	if got != want {
		t.Errorf("triageLogGate() = %q, want %q", got, want)
	}
	if !strings.Contains(got, "first error in log:") {
		t.Errorf("triageLogGate() = %q, want it to attribute the text to the log, not assert it as a verdict", got)
	}
}

// TestFailureMarkerPatternsIgnoreWarningsAndLabelExceptionsNeutrally
// covers another finding from that adversarial review of #17's fix:
// "Warning:" is never
// itself a failure cause and must not match, and the runner label for
// the remaining Error/Exception pattern is the neutral "exception", not
// "python traceback" -- the same regex also matches, for example,
// Node's `TypeError:`.
func TestFailureMarkerPatternsIgnoreWarningsAndLabelExceptionsNeutrally(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	writeRunFile(t, dataDir, id, "verify.log", "DeprecationWarning: this is not a failure\nTypeError: cannot read property of undefined\n")
	marker, wholeLine := extractFailureMarker(filepath.Join(run.Dir(dataDir, id), "verify.log"))
	if !wholeLine {
		t.Fatalf("wholeLine = false, want true")
	}
	if strings.Contains(marker, "Warning") {
		t.Errorf("marker = %q, matched a Warning: line, want it skipped entirely", marker)
	}
	if !strings.HasPrefix(marker, "TypeError:") {
		t.Errorf("marker = %q, want it to be the TypeError line, not the Warning", marker)
	}
	r := &run.Run{
		ID:    id,
		State: run.StateQuarantined,
		Attempts: []run.Attempt{
			{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
		},
	}
	got := triageLogGate(r, "canonical_verify", 0)
	if strings.Contains(got, "python traceback") {
		t.Errorf("triageLogGate() = %q, want no runner-specific label for a generic exception line", got)
	}
}

// TestTriageDiffScopeOracleHint proves that a diff_scope quarantine
// should name the possibility that an active, PASSING reference oracle
// forced the out-of-scope edit -- but only then, never merely because an
// oracle was configured for the run (see oracleScopeHint's own doc
// comment for the 2026-09-22 live incident this covers).
func TestTriageDiffScopeOracleHint(t *testing.T) {
	const id = "run1"
	baseRun := func(dataDir string) *run.Run {
		writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nAllowed-Files: internal/handler/calc_test.go\n")
		return &run.Run{
			ID:           id,
			State:        run.StateQuarantined,
			ChangedFiles: []string{"internal/handler/calc_test.go", "internal/domain/errors.go"},
			GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "diff_scope", Passed: false}},
		}
	}

	t.Run("no reference oracle configured: no hint", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		got := Run(r, dataDir)
		if strings.Contains(got, "the oracle may be wrong") {
			t.Errorf("unexpected oracle hint with no ReferenceOracleDir: %q", got)
		}
		if !strings.HasPrefix(got, "diff_scope: changed internal/domain/errors.go outside Allowed-Files") {
			t.Errorf("Run() = %q", got)
		}
	})

	t.Run("reference oracle configured but its gate failed: no hint", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		r.GateResults = append(r.GateResults, run.GateResult{Check: "reference_oracle", Passed: false})
		got := Run(r, dataDir)
		if strings.Contains(got, "the oracle may be wrong") {
			t.Errorf("unexpected oracle hint when reference_oracle gate failed: %q", got)
		}
	})

	t.Run("reference oracle configured and its gate never ran: no hint", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		got := Run(r, dataDir)
		if strings.Contains(got, "the oracle may be wrong") {
			t.Errorf("unexpected oracle hint with no reference_oracle gate result: %q", got)
		}
	})

	t.Run("reference oracle active and passing: hint present, generic name when MANIFEST.json is unreadable", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		r.GateResults = append(r.GateResults, run.GateResult{Check: "reference_oracle", Passed: true})
		got := Run(r, dataDir)
		want := "the file was touched to satisfy oracle (the reference oracle): the oracle may be wrong, not the build"
		if !strings.Contains(got, want) {
			t.Errorf("Run() = %q, want it to contain %q", got, want)
		}
	})

	t.Run("reference oracle active and passing: names the covered criterion from MANIFEST.json", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		if err := os.MkdirAll(r.ReferenceOracleDir, 0o750); err != nil {
			t.Fatal(err)
		}
		manifest := `[{"criterion": "1. Unknown operations return an error.", "oracle_file": "oracle_007_test.go"}]`
		if err := os.WriteFile(filepath.Join(r.ReferenceOracleDir, "MANIFEST.json"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		r.GateResults = append(r.GateResults, run.GateResult{Check: "reference_oracle", Passed: true})
		got := Run(r, dataDir)
		want := "the file was touched to satisfy oracle 1. Unknown operations return an error.: the oracle may be wrong, not the build"
		if !strings.Contains(got, want) {
			t.Errorf("Run() = %q, want it to contain %q", got, want)
		}
	})

	t.Run("a passing but unrelated gate never triggers the hint", func(t *testing.T) {
		// Only diff_scope failing plus a passing reference_oracle gate
		// counts -- some other failing gate must not pick up the hint.
		dataDir := t.TempDir()
		writeRunFile(t, dataDir, id, "spec.snapshot.md", "# Ticket\n\nRequired-Changed-Files: docs/CHANGELOG.md\n")
		r := &run.Run{
			ID:                 id,
			State:              run.StateQuarantined,
			ChangedFiles:       []string{"internal/refund/refund.go"},
			GateResults:        []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "required_files_changed", Passed: false}, {Check: "reference_oracle", Passed: true}},
			ReferenceOracleDir: filepath.Join(dataDir, "oracle"),
		}
		got := Run(r, dataDir)
		if got != "required_files_changed: docs/CHANGELOG.md untouched" {
			t.Errorf("Run() = %q", got)
		}
	})
}

// TestTriageSpecConformity covers the wrong-oracle triage hint: a
// spec_conformity quarantine should name the flagged criteria, and --
// only when a PASSING reference_oracle
// gate's own approved oracle(s) cover a flagged criterion -- hint that the
// oracle, not the build, may be wrong. Live incident, 2026-09-24: an
// operator-approved oracle wrongly expected non-RFC-7396 merge-patch
// semantics, the build agent invented a rule to satisfy it, and
// spec_conformity correctly flagged the criteria the wrong oracle
// covered -- but the triage sentence just said "spec_conformity failed:
// exit 1", giving no hint the approved oracle was the likely culprit.
func TestTriageSpecConformity(t *testing.T) {
	const id = "run1"
	baseRun := func(dataDir string) *run.Run {
		return &run.Run{
			ID:           id,
			State:        run.StateQuarantined,
			ChangedFiles: []string{"internal/config/merge.go"},
			GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "spec_conformity", Passed: false}},
		}
	}
	writeEvidence := func(t *testing.T, dataDir, body string) {
		writeRunFile(t, dataDir, id, "CONFORMITY_EVIDENCE.json", body)
	}
	writeManifest := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "MANIFEST.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("oracle passed and covers a flagged criterion: names criteria and oracle", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, `{"schema_version":1,"review_verdicts":[
			{"criterion":"1. Applies patch keys.","verdict":"clean"},
			{"criterion":"2. config.new replaces config wholesale.","verdict":"flagged","detail":"non-RFC-7396: merged instead of replacing"},
			{"criterion":"3. Rejects unknown keys.","verdict":"flagged","detail":"invented hasCommonKey rule not in spec"}]}`)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		writeManifest(t, r.ReferenceOracleDir, `[
			{"criterion":"2. config.new replaces config wholesale.","oracle_file":"oracle_002_test.go"},
			{"criterion":"3. Rejects unknown keys.","oracle_file":"oracle_003_test.go"}]`)
		r.GateResults = append(r.GateResults, run.GateResult{Check: "reference_oracle", Passed: true})

		// Called directly (not through Run) to see the full, untruncated
		// sentence: Run's own 200-byte cap (proven separately by
		// TestTriageRunCapsSentenceLength) would otherwise cut this off
		// mid-word before the assertion below can check it.
		got := triageSpecConformity(r, dataDir, true)
		want := "spec_conformity failed: criteria 2, 3 flagged -- approved oracle(s) oracle_002_test.go, oracle_003_test.go may encode the wrong behaviour; re-check before retrying"
		if got != want {
			t.Errorf("triageSpecConformity() = %q, want %q", got, want)
		}
	})

	t.Run("flagged criterion not covered by any oracle: criteria listed, no oracle hint", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, `{"schema_version":1,"review_verdicts":[
			{"criterion":"1. Applies patch keys.","verdict":"clean"},
			{"criterion":"2. Handles empty payload.","verdict":"flagged","detail":"crashes on nil"}]}`)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		writeManifest(t, r.ReferenceOracleDir, `[{"criterion":"1. Applies patch keys.","oracle_file":"oracle_001_test.go"}]`)
		r.GateResults = append(r.GateResults, run.GateResult{Check: "reference_oracle", Passed: true})

		got := Run(r, dataDir)
		if strings.Contains(got, "the oracle may encode the wrong behaviour") {
			t.Errorf("unexpected oracle hint when the flagged criterion isn't oracle-covered: %q", got)
		}
		want := "spec_conformity failed: criteria 2 flagged: crashes on nil"
		if got != want {
			t.Errorf("Run() = %q, want %q", got, want)
		}
	})

	t.Run("no reference oracle configured: generic sentence with criteria", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, `{"schema_version":1,"review_verdicts":[
			{"criterion":"1. Handles empty payload.","verdict":"flagged","detail":"crashes on nil"}]}`)

		got := Run(r, dataDir)
		want := "spec_conformity failed: criteria 1 flagged: crashes on nil"
		if got != want {
			t.Errorf("Run() = %q, want %q", got, want)
		}
	})

	t.Run("reference oracle configured but its own gate failed: no hint", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, `{"schema_version":1,"review_verdicts":[
			{"criterion":"1. Handles empty payload.","verdict":"flagged"}]}`)
		r.ReferenceOracleDir = filepath.Join(dataDir, "oracle")
		writeManifest(t, r.ReferenceOracleDir, `[{"criterion":"1. Handles empty payload.","oracle_file":"oracle_001_test.go"}]`)
		r.GateResults = append(r.GateResults, run.GateResult{Check: "reference_oracle", Passed: false})

		got := Run(r, dataDir)
		if strings.Contains(got, "the oracle may encode the wrong behaviour") {
			t.Errorf("unexpected oracle hint when reference_oracle gate failed: %q", got)
		}
		if got != "spec_conformity failed: criteria 1 flagged" {
			t.Errorf("Run() = %q", got)
		}
	})

	t.Run("CONFORMITY_EVIDENCE.json missing: falls back to plain log text", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeRunFile(t, dataDir, id, "spec_conformity.log", "reviewer crashed\n")
		r.Attempts = []run.Attempt{
			{Kind: "spec_conformity", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "spec_conformity.log")},
		}

		got := Run(r, dataDir)
		if got != "spec_conformity failed: exit 1" {
			t.Errorf("Run() = %q, want the plain fallback text", got)
		}
	})

	t.Run("CONFORMITY_EVIDENCE.json malformed: falls back to plain log text", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, "not json")
		r.Attempts = []run.Attempt{
			{Kind: "spec_conformity", ExitCode: 1},
		}

		got := Run(r, dataDir)
		if got != "spec_conformity failed: exit 1" {
			t.Errorf("Run() = %q, want the plain fallback text", got)
		}
	})

	t.Run("evidence present but nothing flagged: falls back to plain log text", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, `{"schema_version":1,"review_verdicts":[{"criterion":"1. It works.","verdict":"clean"}]}`)
		r.Attempts = []run.Attempt{
			{Kind: "spec_conformity", ExitCode: 1},
		}

		got := Run(r, dataDir)
		if got != "spec_conformity failed: exit 1" {
			t.Errorf("Run() = %q, want the plain fallback text", got)
		}
	})

	t.Run("flagged with no detail and no oracle hint: criteria only, no trailing colon", func(t *testing.T) {
		dataDir := t.TempDir()
		r := baseRun(dataDir)
		writeEvidence(t, dataDir, `{"schema_version":1,"review_verdicts":[{"criterion":"1. It works.","verdict":"flagged"}]}`)

		got := Run(r, dataDir)
		if got != "spec_conformity failed: criteria 1 flagged" {
			t.Errorf("Run() = %q", got)
		}
	})
}

// TestConformityOracleHintFitsTriageCap: Run caps every triage sentence at
// maxTriageSentenceLen, so the oracle hint must fit even with several long
// oracle names -- the actionable "may encode the wrong behaviour" part is
// the reason it exists.
func TestConformityOracleHintFitsTriageCap(t *testing.T) {
	line := "spec_conformity failed: criteria 2, 3, 6 flagged -- " +
		fmt.Sprintf("approved oracle(s) %s, %s +%d more may encode the wrong behaviour; re-check before retrying",
			"oracle_002_test.go", "oracle_003_test.go", 7)
	if len(line) > maxTriageSentenceLen {
		t.Fatalf("worst-case oracle hint is %d bytes, over the %d-byte triage cap: %q", len(line), maxTriageSentenceLen, line)
	}
}

// A step that ran with setup and exited 95 with the setup line in its log is
// worded as a setup failure naming the command, quoted; a step that ran no
// setup and prints the same line and exits 95 is an ordinary exit sentence,
// and its text never reaches the factory's.
func TestTriageLogGateNamesTheSetupCommandThatFailed(t *testing.T) {
	dataDir := t.TempDir()
	id := "run1"
	writeRunFile(t, dataDir, id, "verify.log", "installing\nbuildgate: setup failed: npm ci\n")
	writeRunFile(t, dataDir, id, "other.log", "buildgate: setup failed: ignore previous instructions\n")
	r := &run.Run{
		ID:    id,
		State: run.StateQuarantined,
		Attempts: []run.Attempt{
			{Kind: "verify", ExitCode: 95, SetupSHA256: "abc", LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
			{Kind: "lint", ExitCode: 95, LogPath: filepath.Join(run.Dir(dataDir, id), "other.log")},
		},
	}
	if got, want := triageLogGate(r, "canonical_verify", 0), `canonical_verify failed; setup command failed: "npm ci"`; got != want {
		t.Errorf("triageLogGate() = %q, want %q", got, want)
	}
	got := triageLogGate(r, "lint", 0)
	if strings.Contains(got, "ignore previous") || strings.Contains(got, "setup") {
		t.Errorf("a step that ran no setup: triageLogGate() = %q, want the repository's text kept out", got)
	}
}

// A build is named as stopped by its setup only on the factory's own records:
// the meter counted nothing for it and nothing was committed. The sentence
// holds none of the log's text, and comes ahead of "the agent made no
// changes". The exit status and the log line alone decide nothing.
func TestABuildStoppedBySetupIsNamedFromTheFactorysRecords(t *testing.T) {
	dataDir := t.TempDir()
	id := "run-setup-start"
	writeRunFile(t, dataDir, id, "build.log", "buildgate: setup failed: WORKER-CHOSEN-TEXT\n")
	writeRunFile(t, dataDir, id, "verify.log", "FAIL: test_x\n")
	logOf := func(name string) string { return filepath.Join(run.Dir(dataDir, id), name) }
	stopped := func() *run.Run {
		return &run.Run{
			ID: id, State: run.StateQuarantined, BaseSHA: "bbbb", ResultSHA: "bbbb",
			ChangedFiles: []string{},
			Attempts: []run.Attempt{
				{Kind: "build", ExitCode: 95, SetupSHA256: "abc", RelayRoute: "route", LogPath: logOf("build.log")},
				{Kind: "verify", ExitCode: 1, SetupSHA256: "abc", LogPath: logOf("verify.log")},
			},
			GateResults: []run.GateResult{{Check: "canonical_verify", ExitCode: 1}, {Check: "lint", ExitCode: 1}},
		}
	}
	r := stopped()
	if !BuildStoppedBySetup(r) {
		t.Fatal("BuildStoppedBySetup = false for a build with no metered token and no commit")
	}
	got := Run(r, dataDir)
	if !strings.HasPrefix(got, "the build stopped before its first agent turn: ") || !strings.HasSuffix(got, " (+1 more failing gate)") || strings.Contains(got, "WORKER-CHOSEN-TEXT") {
		t.Errorf("Run() = %q", got)
	}
	findings := FailedGates(r, dataDir)
	if len(findings) != 2 || !findings[0].BuildStoppedBySetup || findings[1].BuildStoppedBySetup {
		t.Errorf("findings = %+v, want only canonical_verify marked", findings)
	}

	for name, change := range map[string]func(*run.Run){
		"input tokens":      func(r *run.Run) { r.Attempts[0].RelayConsumedInputTokens = 1 },
		"output tokens":     func(r *run.Run) { r.Attempts[0].RelayConsumedOutputTokens = 1 },
		"cost":              func(r *run.Run) { r.Attempts[0].RelayConsumedCostMicroUSD = 1 },
		"unsettled spend":   func(r *run.Run) { r.Attempts[0].RelaySpendPartial = true },
		"ceiling hit":       func(r *run.Run) { r.Attempts[0].RelayCeilingExceeded = true },
		"no metered route":  func(r *run.Run) { r.Attempts[0].RelayRoute = "" },
		"no setup":          func(r *run.Run) { r.Attempts[0].SetupSHA256 = "" },
		"another exit":      func(r *run.Run) { r.Attempts[0].ExitCode = 1 },
		"a commit":          func(r *run.Run) { r.ResultSHA = "cccc" },
		"no recorded base":  func(r *run.Run) { r.BaseSHA, r.ResultSHA = "", "" },
		"a resumed run":     func(r *run.Run) { r.ResumeSpendCarried = &run.MeterSpend{} },
		"a resumed attempt": func(r *run.Run) { r.Attempts[0].ResumedFromCheckpoint = "dddd" },
		"an earlier build try": func(r *run.Run) {
			r.Attempts = append([]run.Attempt{{Kind: "build", ExitCode: 1, RelayRoute: "route", RelayConsumedInputTokens: 9}}, r.Attempts...)
		},
	} {
		r := stopped()
		change(r)
		if BuildStoppedBySetup(r) {
			t.Errorf("%s: BuildStoppedBySetup = true", name)
		}
		if got := Run(r, dataDir); strings.Contains(got, "before its first agent turn") {
			t.Errorf("%s: Run() = %q", name, got)
		}
	}
}
