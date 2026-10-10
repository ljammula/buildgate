package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// factorydPackageSource concatenates every non-test .go source file in
// this package (cmd/factoryd), for a static-source-guard test that
// locates a specific top-level function or statement by name/pattern.
// Reading a single hardcoded file (an earlier version of every caller
// below read only "main.go") breaks the moment that function moves to a
// different file in the same package -- exactly what a same-package,
// multiple-file modularization does routinely, on purpose, without
// changing any behavior these guards actually care about. Concatenating
// the whole package instead means a guard keeps working regardless of
// which file its target function currently lives in.
func factorydPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/factoryd directory: %v", err)
	}
	var text strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text.Write(b)
		text.WriteByte('\n')
	}
	return text.String()
}

// TestNeverReadsAgentAuthoredEvidence is a static guard for the plan's
// flagship invariant: "No stage may advance on an agent's self-report."
// BUILD_REPORT.md is agent-authored prose and main.go must only ever
// mention its path for a human-facing print line, never open/read it to
// decide anything. This is cheap and directly protects the invariant the
// rest of this test suite otherwise only protects indirectly.
func TestNeverReadsAgentAuthoredEvidence(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	readCalls := regexp.MustCompile(`os\.(ReadFile|Open)\([^)]*BUILD_REPORT`)
	if readCalls.MatchString(text) {
		t.Fatal("main.go reads BUILD_REPORT.md directly — agent prose must never decide pass/fail")
	}

	// At most one legitimate occurrence: a single fmt.Printf line that both
	// names the file and joins its path for display — two textual
	// occurrences of the literal, both inside that one print call.
	occurrences := strings.Count(text, "BUILD_REPORT.md")
	if occurrences > 2 {
		t.Fatalf("BUILD_REPORT.md referenced %d times; verify every reference is still print-only, not read as a signal", occurrences)
	}
}

// TestEvidenceReadOccursAfterFinalStateAssignment is a static guard for
// BUILD_EVIDENCE.json's non-gating role: even though factoryd may read this
// structured harness output (via the shared loadAgentEvidence helper, called
// from applyRunWorkflowResult, which every Temporal-routed run converges on
// — found live in review: the Temporal path originally never called it at
// all, leaving r.AgentEvidence always empty), each call must remain structurally downstream of
// every terminal-state decision in its own calling function, and the helper
// itself must never assign r.State.
func TestEvidenceReadOccursAfterFinalStateAssignment(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	funcStarts := regexp.MustCompile(`(?m)^func (\w+)\(`).FindAllStringSubmatchIndex(text, -1)
	if len(funcStarts) == 0 {
		t.Fatal("cmd/factoryd package has no top-level func declarations; static evidence-order guard cannot locate function bodies")
	}
	// enclosingFunc returns the name and [start,end) source range of the
	// top-level function containing source offset off.
	enclosingFunc := func(off int) (name string, start, end int) {
		end = len(text)
		for i, m := range funcStarts {
			if m[0] > off {
				break
			}
			start, name = m[0], text[m[2]:m[3]]
			if i+1 < len(funcStarts) {
				end = funcStarts[i+1][0]
			} else {
				end = len(text)
			}
		}
		return name, start, end
	}

	stateAssignments := regexp.MustCompile(`(?m)^\s*r\.State\s*=`).FindAllStringIndex(text, -1)
	if len(stateAssignments) == 0 {
		t.Fatal("cmd/factoryd package has no r.State assignments; static evidence-order guard cannot locate the gate decision")
	}

	// Call sites only, not the "func loadAgentEvidence(r *run.Run, ..."
	// definition itself, which always declares r's type right after it.
	callSites := regexp.MustCompile(`loadAgentEvidence\(r,`).FindAllStringIndex(text, -1)
	if len(callSites) < 1 {
		t.Fatalf("expected loadAgentEvidence to be called from applyRunWorkflowResult, found %d call site(s)", len(callSites))
	}
	seenFuncs := map[string]bool{}
	for _, call := range callSites {
		fnName, fnStart, fnEnd := enclosingFunc(call[0])
		var lastStateInFunc = -1
		for _, sa := range stateAssignments {
			if sa[0] >= fnStart && sa[0] < fnEnd {
				lastStateInFunc = sa[0]
			}
		}
		if lastStateInFunc == -1 {
			t.Fatalf("function %q calls loadAgentEvidence but has no r.State assignment to order it against", fnName)
		}
		if call[0] <= lastStateInFunc {
			t.Fatalf("loadAgentEvidence call in %q at offset %d must occur after its last r.State assignment at offset %d", fnName, call[0], lastStateInFunc)
		}
		seenFuncs[fnName] = true
	}
	if len(seenFuncs) < 1 {
		t.Fatalf("expected loadAgentEvidence calls ordered correctly in at least one function, verified in %v", seenFuncs)
	}

	defLoc := regexp.MustCompile(`(?m)^func loadAgentEvidence\(`).FindStringIndex(text)
	if defLoc == nil {
		t.Fatal("cmd/factoryd package has no loadAgentEvidence definition; static evidence-order guard cannot locate the shared helper")
	}
	_, defStart, defEnd := enclosingFunc(defLoc[0])
	if regexp.MustCompile(`r\.State\s*=`).MatchString(text[defStart:defEnd]) {
		t.Fatal("loadAgentEvidence must not assign r.State — it is best-effort agent evidence attachment, never the gate")
	}
}

// TestDaemonMainWritesHeartbeatBeforeStartingWorkers is the regression for
// a real P1 finding from a ninth round of GitHub Codex review of PR #37:
// an earlier version of daemonMain wrote its first heartbeat only after
// w.Start() and the initial reclaimAbandonedRunQueues() scan — both of
// which can themselves take real time (Temporal round trips). A first fix
// attempt only hoisted the write above those two, still leaving it after
// the initial sandbox-reconciliation call and the Temporal dial; a
// further Opus-assisted review round found that gap and moved the write
// above everything in daemonMain that could itself take real time. During
// a daemon restart with a changed -sandbox-docker/DOCKER_HOST/
// DOCKER_CONTEXT, a submitter's own launch-time re-check
// (runSandboxWithRetries) could read the *previous* daemon's still-fresh
// heartbeat during that whole window and approve a launch against a
// Docker configuration this replacement daemon no longer uses — defeating
// the very check that heartbeat exists to make authoritative.
//
// Proven via source inspection (the same technique
// TestDaemonMainReconcilesSandboxOrphansOnEveryReclaimTick and
// TestRunViaRepositoryOwnerCallsCheckSandboxDockerAgainstDaemonHeartbeat
// above use for a static ordering/wiring fact) rather than a real-timing
// integration test: the bug is about relative ordering of two operations
// within one function execution, not an externally observable race a
// black-box test could reliably catch — a real Temporal server test
// reading the heartbeat file "as fast as possible" after daemon restart
// has no guarantee of landing inside a window this fix might narrow to
// microseconds.
func TestDaemonMainWritesHeartbeatBeforeStartingWorkers(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	// daemonMain runs four stages in turn; the ordering under test spans
	// them, so body is their source in the order daemonMain calls them.
	stages := []string{"loadConfig", "startHeartbeat", "startWorker", "serviceRepository"}
	mainLoc := regexp.MustCompile(`(?m)^func daemonMain\(`).FindStringIndex(text)
	if mainLoc == nil {
		t.Fatal("cmd/factoryd package has no daemonMain function — moved or renamed; update this static guard")
	}
	mainBody := text[mainLoc[0]:]
	if next := regexp.MustCompile(`(?m)^func `).FindStringIndex(mainBody[1:]); next != nil {
		mainBody = mainBody[:next[0]+1]
	}
	var body string
	lastCall := -1
	for _, stage := range stages {
		call := strings.Index(mainBody, "d."+stage+"()")
		if call <= lastCall {
			t.Fatalf("daemonMain no longer calls d.%s() after the stage before it — update this static guard", stage)
		}
		lastCall = call
		loc := regexp.MustCompile(`(?m)^func \(d \*daemonRun\) ` + stage + `\(\) error \{$`).FindStringIndex(text)
		if loc == nil {
			t.Fatalf("cmd/factoryd package has no daemonRun.%s stage — moved or renamed; update this static guard", stage)
		}
		stageBody := text[loc[0]:]
		if next := regexp.MustCompile(`(?m)^func `).FindStringIndex(stageBody[1:]); next != nil {
			stageBody = stageBody[:next[0]+1]
		}
		body += stageBody
	}

	// Anchored to the exact single-tab-indented statement form — not a
	// bare `writeHeartbeat\(\)` substring search — because that call also
	// appears a second time, differently indented (and now inside its own
	// `if err := ...; err != nil` warning branch, round 10's own fatal-
	// initial-write change), inside the heartbeat ticker's own goroutine
	// further down. Found via an Opus-assisted review of an earlier
	// version of this test: FindStringIndex returning the *first* match
	// of an unanchored pattern is a false-pass trap — deleting only the
	// synchronous call (leaving just the ticker's own, async, call) would
	// silently make the unanchored regex match that second occurrence
	// instead and still report "before w.Start()", never catching the
	// exact regression (the heartbeat no longer written synchronously at
	// startup at all) this test exists to guard.
	heartbeatLoc := regexp.MustCompile(`(?m)^\tif err := writeHeartbeat\(\); err != nil \{$`).FindStringIndex(body)
	if heartbeatLoc == nil {
		t.Fatal("daemonMain no longer has a top-level, synchronous, fatal-on-error writeHeartbeat() call — update this static guard")
	}
	// startLoc/reconcileLoc/dialLoc: also anchored to a single-tab indent
	// where the same literal text otherwise recurs elsewhere in this
	// function at a different indent (reconcileSandboxOrphans(signalCtx)
	// is called a second time, four tabs deep, from the periodic
	// reclaimTicker case below) — same false-pass trap as writeHeartbeat()
	// above, and the same fix (found via a further Opus-assisted review,
	// after round 9 also moved the heartbeat write above the initial
	// sandbox-reconciliation call and the Temporal dial, not just above
	// w.Start()): an unanchored first-match search would silently start
	// matching the wrong occurrence if the right one were ever deleted.
	for _, check := range []struct {
		name    string
		pattern string
	}{
		{"the initial reconcileSandboxOrphans(signalCtx) call", `(?m)^\td\.reconcileSandboxOrphans\(d\.signalCtx\)$`},
		{"the Temporal dial", `client\.Dial\(client\.Options\{HostPort: \*d\.temporalAddress\}\)`},
		{"w.Start()", `w\.Start\(\)`},
	} {
		loc := regexp.MustCompile(check.pattern).FindStringIndex(body)
		if loc == nil {
			t.Fatalf("daemonMain no longer contains %s — update this static guard", check.name)
		}
		if heartbeatLoc[0] > loc[0] {
			t.Fatalf("the synchronous writeHeartbeat() call is at offset %d, after %s at offset %d, within daemonMain — the first heartbeat write must happen before anything else that could itself take real time", heartbeatLoc[0], check.name, loc[0])
		}
	}
}

// TestDaemonMainFailsFastWhenInitialHeartbeatWriteFails is the regression
// for a real P1 finding from a tenth round of GitHub Codex review of PR
// #37: every submission-time and launch-time sandbox-divergence check
// treats a missing heartbeat as "unknown", not "diverges" — deliberately
// conservative for a standalone deployment with no daemon at all, but
// that same conservatism means a daemon whose *initial* write silently
// failed (a temporarily read-only -data-dir, a permissions problem) used
// to start up looking healthy while quietly disabling this whole
// protection mechanism: every check would keep seeing no heartbeat and
// allow any Docker configuration through. The initial write failing must
// now be fatal to daemon startup. A -data-dir that already exists as a
// regular file (not a directory) makes daemonheartbeat.Write's own
// MkdirAll fail deterministically and portably, without needing a real
// permissions/ownership setup — and, since the heartbeat write now
// happens before this function's sandbox reconciliation call (proven by
// the test above), this returns before ever invoking a real `docker`
// binary, so no live Docker daemon is required either.
func TestDaemonMainFailsFastWhenInitialHeartbeatWriteFails(t *testing.T) {
	dp := newTestDeps(t)
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(dataDir, []byte("blocks MkdirAll"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := daemonMain(dp, []string{
		"-temporal-address", "127.0.0.1:1",
		"-repository", "fixture/repo",
		"-data-dir", dataDir,
	})
	if err == nil {
		t.Fatal("daemonMain with an unwritable -data-dir (initial heartbeat write must fail): want error, got nil")
	}
	if !strings.Contains(err.Error(), "heartbeat") {
		t.Fatalf("error = %q, want it to mention the heartbeat write failure", err.Error())
	}
}

// TestDaemonMainRejectsEmptySandboxDockerFlag is the regression for a real
// P2 finding from a fifth round of GitHub Codex review of PR #37:
// -sandbox-docker has a non-empty default ("docker"), but flag parsing
// still accepted an explicit -sandbox-docker="". That empty value would
// then flow into activities.SandboxDocker, silently disabling
// runSandboxWithRetries' own divergence guard (gated on
// a.SandboxDocker != "") for every sandboxed request this daemon services,
// on top of every reconciliation call itself failing against an empty
// executable. Rejected outright at startup instead, before this daemon
// ever does anything else.
func TestDaemonMainRejectsEmptySandboxDockerFlag(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_docker: \"\"\n")
	err := daemonMain(dp, []string{
		"-temporal-address", "127.0.0.1:1",
		"-repository", "fixture/repo",
		"-data-dir", t.TempDir(),
	})
	if err == nil {
		t.Fatal("daemonMain with sandbox_docker: \"\": want error, got nil")
	}
	if !strings.Contains(err.Error(), "-sandbox-docker must not be empty") {
		t.Fatalf("error = %q, want it to name the empty -sandbox-docker flag", err.Error())
	}
}

// TestDaemonMainRefusesInvalidRolesBlock proves `factoryd daemon` also
// validates roles: once, explicitly, silently (error only -- see
// validateRoles' own doc comment), at process start: an unknown
// model_aliases entry named by a role must refuse the whole invocation.
func TestDaemonMainRefusesInvalidRolesBlock(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "roles:\n  execution:\n    model: does-not-exist\n")
	err := daemonMain(dp, []string{
		"-temporal-address", "127.0.0.1:1",
		"-repository", "fixture/repo",
		"-data-dir", t.TempDir(),
	})
	if err == nil {
		t.Fatal("daemonMain with an invalid roles: block: want error, got nil")
	}
	if !strings.Contains(err.Error(), "roles.execution") {
		t.Fatalf("error = %q, want it to name roles.execution", err.Error())
	}
}

// TestValidateSandboxResourceLimitFlags is the regression test for real P2
// findings from two GitHub Codex App review rounds on PR #52. Round one:
// an empty -sandbox-memory/-sandbox-cpus/-sandbox-tmpfs-size parsed
// successfully as a flag and was then silently replaced by
// workflow.Activities' own zero-value fallback ("4g"/"2"/"256m") -- so an operator's
// actually-invalid configuration appeared to take effect while the worker
// ran with a materially different ceiling than requested, instead of
// failing loudly at startup. Round two: even a non-empty zero-valued
// -sandbox-memory/-sandbox-cpus (e.g. "0" or "0g") passed round one's
// check yet still means "no limit" to Docker itself, so an operator's
// apparently-valid configuration could still launch an unbounded worker.
func TestValidateSandboxResourceLimitFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                    string
		memory, cpus, tmpfsSize string
		wantErrContains         string
	}{
		{name: "valid", memory: "4g", cpus: "2", tmpfsSize: "256m", wantErrContains: ""},
		{name: "valid fractional cpus", memory: "4g", cpus: "0.5", tmpfsSize: "256m", wantErrContains: ""},
		{name: "empty memory", memory: "", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must not be empty"},
		{name: "empty cpus", memory: "4g", cpus: "", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must not be empty"},
		{name: "empty tmpfs size", memory: "4g", cpus: "2", tmpfsSize: "", wantErrContains: "-sandbox-tmpfs-size must not be empty"},
		{name: "zero memory bare", memory: "0", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be a positive value"},
		{name: "zero memory with unit", memory: "0g", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be a positive value"},
		{name: "malformed memory", memory: "not-a-size", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be a positive integer with an optional b/k/m/g suffix"},
		{name: "memory below Docker's 6m minimum", memory: "1m", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be at least 6m"},
		{name: "memory exactly at Docker's 6m minimum", memory: "6m", cpus: "2", tmpfsSize: "256m", wantErrContains: ""},
		{name: "zero cpus", memory: "4g", cpus: "0", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be greater than zero"},
		{name: "negative cpus", memory: "4g", cpus: "-1", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be greater than zero"},
		{name: "malformed cpus", memory: "4g", cpus: "not-a-number", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be a finite decimal number"},
		{name: "NaN cpus", memory: "4g", cpus: "NaN", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be a finite decimal number"},
		{name: "Inf cpus", memory: "4g", cpus: "+Inf", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be a finite decimal number"},
		{name: "zero tmpfs size", memory: "4g", cpus: "2", tmpfsSize: "0", wantErrContains: "-sandbox-tmpfs-size must be a positive value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSandboxResourceLimitFlags("-sandbox", test.memory, test.cpus, test.tmpfsSize)
			if test.wantErrContains == "" {
				if err != nil {
					t.Fatalf("valid limits: want nil error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErrContains)
			}
		})
	}
}

// TestDaemonMainRejectsInvalidSandboxResourceLimit proves the validation above
// is actually wired into factoryd daemon's own startup, not just a pure
// function nothing calls.
func TestDaemonMainRejectsInvalidSandboxResourceLimit(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_cpus: \"0\"\n")
	err := daemonMain(dp, []string{
		"-temporal-address", "127.0.0.1:1",
		"-repository", "fixture/repo",
		"-data-dir", t.TempDir(),
	})
	if err == nil {
		t.Fatal("daemonMain with sandbox_cpus: 0: want error, got nil")
	}
	if !strings.Contains(err.Error(), "-sandbox-cpus must be greater than zero") {
		t.Fatalf("error = %q, want it to name the invalid -sandbox-cpus flag", err.Error())
	}
}

// TestRunMainWithReadyRejectsInvalidSandboxResourceLimit proves the
// direct/Temporal/repository-owner path's own -sandbox-cpus validation is
// wired into runMainWithReady's actual startup, before it ever touches
// -workspace/-spec, so a plainly invalid value fails fast rather than
// surfacing much later as an infrastructure failure deep in a sandboxed
// attempt.
func TestRunMainWithReadyRejectsInvalidSandboxResourceLimit(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_cpus: \"0\"\n")
	// An explicit, offline -build-app-script: routes:/models:/roles: is
	// the only session-config schema and this fixture configures none,
	// so a default, model-backed build would fail the earlier "requires a
	// relay" gate first (relayNeededForExecution) -- irrelevant to what
	// this test actually checks, the -sandbox-cpus validation's own
	// place in startup ordering, which still runs before -workspace/-spec
	// are ever touched either way.
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-build-app-interpreter", "/bin/sh", "-build-app-script", "/does/not/exist/offline_build.py",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "-sandbox-cpus must be greater than zero") {
		t.Fatalf("runMainWithReady with sandbox_cpus: 0: error = %v, want it to name the invalid -sandbox-cpus flag", err)
	}
}

// TestServeMainRejectsInvalidSandboxResourceLimitFlags proves serve's own
// -sandbox-* flags (the ceiling every API-started run gets) are validated
// at startup, before ever binding a listener. The -daemon-sandbox-* pair
// this also used to cover is gone: `factoryd daemon`, the child those were
// forwarded to, no longer defines a sandbox-resource flag at all, so serve
// offering one could only ever produce a child that rejected it -- see
// TestSuperviseDaemonArgsAreAllDefinedOnTheDaemonCommand.
func TestServeMainRejectsInvalidSandboxResourceLimitFlags(t *testing.T) {
	dp := newTestDeps(t)
	if err := serveMain(dp, []string{"-data-dir", t.TempDir(), "-sandbox-cpus", "0"}); err == nil || !strings.Contains(err.Error(), "-sandbox-cpus must be greater than zero") {
		t.Fatalf("serveMain with -sandbox-cpus=0: error = %v, want it to name the invalid -sandbox-cpus flag", err)
	}
}

// TestDaemonMainReconcilesSandboxOrphansBeforeDialingTemporal is the
// regression test for a Docker-worker gap: a Temporal-routed sandboxed
// run's orphaned container is found even when no `-sandbox-image` invocation against the same -data-dir happened to run
// reconciliation afterward — `factoryd daemon` itself never reconciled at
// startup. This proves daemonMain now calls sandbox.ReconcileOrphans
// against its own -data-dir before it ever contacts Temporal, using a
// fake `docker` executable so no real Docker
// daemon is required, and an address client.Dial is guaranteed to reject
// (client.Dial connects eagerly) so the test doesn't need — or wait on — a
// real Temporal server.
func TestDaemonMainReconcilesSandboxOrphansBeforeDialingTemporal(t *testing.T) {
	dp := newTestDeps(t)
	// not parallel: redirects the shared log package output via log.SetOutput.
	dataDir := t.TempDir()
	terminal := run.Run{ID: "run-terminal", State: run.StateHalted, HaltConfirmed: true}
	if err := terminal.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"ps) printf 'container-terminal\\trun-terminal\\n' ;;\n" +
		"rm) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	// Reserve then immediately release a local port: connecting to it right
	// after Close() reliably gets "connection refused" without depending on
	// nothing-listening-here assumptions about a hardcoded port number.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	unreachable := l.Addr().String()
	l.Close()

	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	writeSessionConfig(t, isolateSessionConfig(t), "sandbox_docker: "+docker+"\n")
	err = daemonMain(dp, []string{
		"-temporal-address", unreachable,
		"-repository", "fixture/repo",
		"-data-dir", dataDir,
	})
	if err == nil {
		t.Fatal("daemonMain unexpectedly succeeded against an unreachable Temporal address")
	}

	if !strings.Contains(logBuf.String(), "removed 1 orphaned container") || !strings.Contains(logBuf.String(), "container-terminal") {
		t.Fatalf("expected daemonMain to reconcile the terminal run's orphaned container before dialing Temporal; log = %q", logBuf.String())
	}
}

// TestDaemonMainCanonicalizesSymlinkedDataDirBeforeReconciling is the
// regression for a real P1 finding from codex review of the change above:
// daemonMain originally resolved -data-dir with a plain filepath.Abs, not
// canonicalPath — the same symlink-resolving helper every submitter path
// (runMainWithReady/runViaRepositoryOwner) already uses before computing a
// sandbox container's data-dir label. A daemon started with a symlinked
// -data-dir would then hash a different label than the one a container was
// actually launched under, silently reconciling nothing. This proves the
// docker `ps --filter` call daemonMain's reconciliation issues carries the
// label for the *resolved* real directory, not the unresolved symlink path,
// by computing the expected label the same way sandbox.dataDirLabel does
// (sha256 hex of the absolute path) and asserting it appears in the actual
// `docker ps` invocation captured by a fake docker script.
func TestDaemonMainCanonicalizesSymlinkedDataDirBeforeReconciling(t *testing.T) {
	dp := newTestDeps(t)
	// not parallel: writes to the isolated session config via t.Setenv.
	realDir := t.TempDir()
	parent := t.TempDir()
	symlinkedDataDir := filepath.Join(parent, "data-symlink")
	if err := os.Symlink(realDir, symlinkedDataDir); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	psArgsPath := filepath.Join(t.TempDir(), "ps-args")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"ps) printf '%s\\n' \"$*\" > " + psArgsPath + " ;;\n" +
		"rm) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	unreachable := l.Addr().String()
	l.Close()

	writeSessionConfig(t, isolateSessionConfig(t), "sandbox_docker: "+docker+"\n")
	err = daemonMain(dp, []string{
		"-temporal-address", unreachable,
		"-repository", "fixture/repo",
		"-data-dir", symlinkedDataDir,
	})
	if err == nil {
		t.Fatal("daemonMain unexpectedly succeeded against an unreachable Temporal address")
	}

	psArgs, err := os.ReadFile(psArgsPath)
	if err != nil {
		t.Fatalf("read captured ps args (docker ps was never invoked): %v", err)
	}
	// canonicalPath, not a bare sha256(realDir): t.TempDir() can itself
	// return a path with its own symlink component (e.g. macOS's
	// /tmp -> /private/tmp), so the true expected label is whatever
	// canonicalPath resolves realDir to, computed the same way daemonMain
	// itself now does — not the raw string this test happened to start
	// from.
	wantDir, err := canonicalPath(realDir)
	if err != nil {
		t.Fatalf("canonicalPath(%q): %v", realDir, err)
	}
	sum := sha256.Sum256([]byte(wantDir))
	wantLabel := hex.EncodeToString(sum[:])
	if !strings.Contains(string(psArgs), wantLabel) {
		t.Fatalf("docker ps filter = %q, want it to contain the label for the resolved real directory %q (%s) — not the unresolved symlink path", psArgs, wantDir, wantLabel)
	}
}

// TestDaemonMainReconcilesSandboxOrphansOnEveryReclaimTick is the
// regression for a real P1 finding from a third round of GitHub Codex
// review of PR #37: sandbox.ReconcileOrphans deliberately leaves alone a
// container whose owner heartbeat is still fresh at scan time (see its own
// doc comment on ownerHeartbeatStale) — correct for any single scan, but
// daemonMain used to call it only once, at startup. A container abandoned
// by a crash-during-restart (heartbeat fresh at that exact startup
// instant, never refreshed again since its own attempt is truly gone)
// would then never be reconsidered once that heartbeat went stale, for the
// rest of the daemon's lifetime — the periodic reclaimTicker loop only
// ever called reclaimAbandonedRunQueues, a different concern (abandoned
// run *queues*, not sandbox containers).
//
// Proven via source inspection — the same technique
// TestNeverReadsAgentAuthoredEvidence and
// TestEvidenceReadOccursAfterFinalStateAssignment above use for a static
// wiring fact — rather than a 30+ second real-timing test waiting out
// daemonHeartbeatInterval against a live Temporal server. Also checks the
// periodic call passes signalCtx, not context.Background() (a round-5
// codex-review fix): reconcileSandboxOrphans' first, one-off startup call
// legitimately still passes context.Background() (signalCtx doesn't exist
// yet that early in daemonMain, and there's nothing to interrupt a one-off
// pre-Temporal-dial call with anyway), but the periodic call must pass
// signalCtx so a SIGTERM arriving mid-scan actually interrupts
// ReconcileOrphans' own staleness-debounce wait instead of leaving
// daemonMain's shutdown blocked on it.
func TestDaemonMainReconcilesSandboxOrphansOnEveryReclaimTick(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	caseLoc := regexp.MustCompile(`case <-reclaimTicker\.C:`).FindStringIndex(text)
	if caseLoc == nil {
		t.Fatal("cmd/factoryd package has no `case <-reclaimTicker.C:` — the periodic reclaim tick moved or was renamed; update this static guard")
	}
	// The case block is a handful of statements before the next `case` (or
	// the select's closing brace) — 200 bytes comfortably covers it without
	// risking a match against a later, unrelated call to either function.
	end := caseLoc[1] + 200
	if end > len(text) {
		end = len(text)
	}
	block := text[caseLoc[1]:end]
	if !strings.Contains(block, "reclaimAbandonedRunQueues()") {
		t.Fatalf("reclaimTicker case block = %q, want it to still call reclaimAbandonedRunQueues()", block)
	}
	if !strings.Contains(block, "d.reconcileSandboxOrphans(d.signalCtx)") {
		t.Fatalf("reclaimTicker case block = %q, want it to also call reconcileSandboxOrphans(signalCtx) on every periodic tick — signalCtx specifically, not context.Background(), so shutdown can interrupt an in-flight scan", block)
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat proves
// checkSandboxDockerAgainstDaemonHeartbeat never blocks a submission over
// an absent daemon heartbeat — a standalone `-repository` submission with
// no daemon yet running for this repository is a legitimate, pre-existing
// deployment shape, so this is informational only.
func TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	// Empty directory: "docker" never resolves via $PATH, so
	// ResolveSandboxDocker's own fallback keeps this test's behavior
	// independent of whether a real docker binary happens to be
	// installed and on $PATH wherever this test runs.
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err != nil {
		t.Fatalf("checkSandboxDockerAgainstDaemonHeartbeat with no heartbeat file: want nil, got %v", err)
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsStaleHeartbeat proves a
// heartbeat that predates the daemon's own staleness window is treated the
// same as no heartbeat at all — the daemon it once proved is running may
// no longer be, so it can no longer prove anything about that daemon's
// current -sandbox-docker.
func TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsStaleHeartbeat(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", t.TempDir()) // "docker" never resolves — see the sibling test's own comment
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: "/usr/local/bin/podman",
		UpdatedAt:     time.Now().Add(-2 * daemonheartbeat.SandboxStaleAfter).Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err != nil {
		t.Fatalf("checkSandboxDockerAgainstDaemonHeartbeat with a stale heartbeat: want nil, got %v", err)
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsNonSandboxedRequest
// proves the check never fires for a non-sandboxed request (sandboxImage
// == ""), regardless of what the heartbeat says — there is no container to
// ever go missing for a request that never launches one.
func TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsNonSandboxedRequest(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", t.TempDir()) // "docker" never resolves — see TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat's own comment
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: "/usr/local/bin/podman",
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "", "docker")
	if err != nil {
		t.Fatalf("checkSandboxDockerAgainstDaemonHeartbeat for a non-sandboxed request: want nil, got %v", err)
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsPredatingHeartbeat
// proves an older heartbeat with no SandboxDocker recorded at all (written
// before that field existed) is treated as "unknown", not "matches" or
// "diverges" — never a rejection.
func TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsPredatingHeartbeat(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", t.TempDir()) // "docker" never resolves — see TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat's own comment
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository: "fixture/repo",
		UpdatedAt:  time.Now().Format(time.RFC3339Nano),
		// SandboxDocker deliberately left unset.
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err != nil {
		t.Fatalf("checkSandboxDockerAgainstDaemonHeartbeat with an empty heartbeat SandboxDocker: want nil, got %v", err)
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatRejectsFreshDivergence is the
// regression for the actual finding: a fresh heartbeat whose own
// SandboxDocker genuinely diverges from this sandboxed request's own must
// be rejected before ever signaling — proof by construction (see the
// function's own doc comment) that a container this request launches
// would be invisible to the daemon actually servicing this repository.
func TestCheckSandboxDockerAgainstDaemonHeartbeatRejectsFreshDivergence(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", t.TempDir()) // "docker" never resolves — see TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat's own comment
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: "/usr/local/bin/podman",
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err == nil {
		t.Fatal("checkSandboxDockerAgainstDaemonHeartbeat with a fresh, diverging heartbeat: want error, got nil")
	}
	if !strings.Contains(err.Error(), "docker") || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("error = %q, want it to name both diverging executables", err.Error())
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatRejectsMatchingExecutableButDivergingEndpoint
// is the regression for a real P1 finding from a further round of GitHub
// Codex review of PR #37: a matching -sandbox-docker string is not proof
// of a matching Docker endpoint — internal/sandbox's own dockerClientEnv()
// passes the full host environment through, so the identical executable
// can still resolve to two different real Docker daemons when DOCKER_HOST
// differs. This must be rejected even though the executable string itself
// (checked first) matches.
func TestCheckSandboxDockerAgainstDaemonHeartbeatRejectsMatchingExecutableButDivergingEndpoint(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "tcp://submitter-host:2376")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", t.TempDir()) // "docker" never resolves — see TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat's own comment
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: "docker",
		DockerHost:    "tcp://daemon-host:2376",
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err == nil {
		t.Fatal("checkSandboxDockerAgainstDaemonHeartbeat with a matching -sandbox-docker but diverging DOCKER_HOST: want error, got nil")
	}
	if !strings.Contains(err.Error(), "submitter-host") || !strings.Contains(err.Error(), "daemon-host") {
		t.Fatalf("error = %q, want it to name both diverging DOCKER_HOST values", err.Error())
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMatchingEndpoint proves
// the flip side: identical -sandbox-docker and identical DOCKER_HOST/
// DOCKER_CONTEXT must not be rejected — the ordinary, unremarkable case.
func TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMatchingEndpoint(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "tcp://shared-host:2376")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", t.TempDir()) // "docker" never resolves — see TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsMissingHeartbeat's own comment
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: "docker",
		DockerHost:    "tcp://shared-host:2376",
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err != nil {
		t.Fatalf("checkSandboxDockerAgainstDaemonHeartbeat with a fully matching executable and endpoint: want nil, got %v", err)
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatRejectsPathResolutionDivergence
// is the regression for a real P1 finding from a ninth round of GitHub
// Codex review of PR #37: two processes both configured with the
// identical bare -sandbox-docker="docker" can still resolve to two
// different real executables when their own $PATH values differ — a
// plain string comparison of the unresolved value can never catch this.
// Two real (fake) "docker" binaries in two different directories, with
// this process's own $PATH pointed at one of them, stand in for a
// submitter and daemon whose PATH differs.
func TestCheckSandboxDockerAgainstDaemonHeartbeatRejectsPathResolutionDivergence(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	submitterDir := t.TempDir()
	submitterDocker := filepath.Join(submitterDir, "docker")
	if err := os.WriteFile(submitterDocker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	daemonDir := t.TempDir()
	daemonDocker := filepath.Join(daemonDir, "docker")
	if err := os.WriteFile(daemonDocker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The heartbeat records what the daemon's own ResolveSandboxDocker
	// would have written for its own environment: its own resolved path,
	// not the bare "docker" string.
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: daemonDocker,
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	// This process's own $PATH resolves "docker" to the submitter's own
	// binary, not the daemon's.
	t.Setenv("PATH", submitterDir)

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err == nil {
		t.Fatal("checkSandboxDockerAgainstDaemonHeartbeat with a $PATH-diverging \"docker\": want error, got nil")
	}
	if !strings.Contains(err.Error(), submitterDocker) || !strings.Contains(err.Error(), daemonDocker) {
		t.Fatalf("error = %q, want it to name both resolved executable paths", err.Error())
	}
}

// TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsPathResolutionMatch
// proves the flip side: when $PATH resolves "docker" to the *same*
// executable the heartbeat's own SandboxDocker already names, this must
// not be rejected merely because the configured strings started out
// identical bare names that happened to need resolving.
func TestCheckSandboxDockerAgainstDaemonHeartbeatAllowsPathResolutionMatch(t *testing.T) {
	// not parallel: uses t.Setenv (DOCKER_HOST/DOCKER_CONTEXT/PATH), which is process-wide state.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	sharedDir := t.TempDir()
	sharedDocker := filepath.Join(sharedDir, "docker")
	if err := os.WriteFile(sharedDocker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		Repository:    "fixture/repo",
		SandboxDocker: sharedDocker,
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	t.Setenv("PATH", sharedDir)

	err := checkSandboxDockerAgainstDaemonHeartbeat(dataDir, ownerID, "run-1", "fixture/repo", "factory-worker:test@sha256:deadbeef", "docker")
	if err != nil {
		t.Fatalf("checkSandboxDockerAgainstDaemonHeartbeat with a $PATH resolving to the same executable: want nil, got %v", err)
	}
}

// TestRunViaRepositoryOwnerCallsCheckSandboxDockerAgainstDaemonHeartbeat is
// a static wiring guard, the same technique
// TestDaemonMainReconcilesSandboxOrphansOnEveryReclaimTick above uses:
// checkSandboxDockerAgainstDaemonHeartbeat's own unit tests above prove its
// logic in isolation, but say nothing about whether runViaRepositoryOwner
// actually calls it before ever signaling the repository owner — this
// closes that gap without needing a real Temporal server.
func TestRunViaRepositoryOwnerCallsCheckSandboxDockerAgainstDaemonHeartbeat(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	fnLoc := regexp.MustCompile(`(?m)^func runViaRepositoryOwner\(`).FindStringIndex(text)
	if fnLoc == nil {
		t.Fatal("cmd/factoryd package has no runViaRepositoryOwner function — moved or renamed; update this static guard")
	}
	signalLoc := regexp.MustCompile(`SignalWithStartWorkflow`).FindStringIndex(text[fnLoc[0]:])
	if signalLoc == nil {
		t.Fatal("runViaRepositoryOwner has no SignalWithStartWorkflow call — update this static guard")
	}
	body := text[fnLoc[0] : fnLoc[0]+signalLoc[0]]
	if !strings.Contains(body, "checkSandboxDockerAgainstDaemonHeartbeat(") {
		t.Fatal("runViaRepositoryOwner does not call checkSandboxDockerAgainstDaemonHeartbeat before its first SignalWithStartWorkflow")
	}
}

func testHeldIsolationLock(t *testing.T, repoDir string) *wsisolation.DirectLock {
	t.Helper()
	lock, err := wsisolation.AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("acquire test repository lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

func TestReconcileIsolationMarkersReapsNonterminalDirectRun(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "direct-crash", "direct")
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir, WorkspacePath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save nonterminal run: %v", err)
	}
	if err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil); err != nil {
		t.Fatalf("reconcileIsolationMarkers returned an error for a clean reap: %v", err)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists after reconciliation: %v", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); !os.IsNotExist(err) {
		t.Fatalf("marker still exists after reconciliation: %v", err)
	}
	// Reconciliation is idempotent when the marker has already been removed.
	if err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil); err != nil {
		t.Fatalf("reconcileIsolationMarkers returned an error on an already-clean idempotent pass: %v", err)
	}
}

// TestReconcileIsolationMarkersPropagatesReapFailure proves the returned
// error surfaces a genuine reap failure (found via review: the standalone
// `factoryd reconcile` command used to unconditionally return nil, so an
// operator or script could never tell a real cleanup failure from
// success). The worktree is re-registered under a different branch than
// the marker declares after Prepare creates it, the same
// "worktree exists but points somewhere unexpected" condition
// ReapIsolationMarker's own safety check refuses, deterministically
// forcing the reap this marker otherwise qualifies for to fail.
func TestReconcileIsolationMarkersPropagatesReapFailure(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "reap-failure", "direct")
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir, WorkspacePath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save nonterminal run: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "remove", "--force", marker.WorktreePath).CombinedOutput(); err != nil {
		t.Fatalf("detach worktree: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "reap-failure-different-branch", marker.WorktreePath).CombinedOutput(); err != nil {
		t.Fatalf("re-register worktree under a different branch: %v: %s", err, out)
	}
	err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil)
	if err == nil {
		t.Fatal("expected reconcileIsolationMarkers to return an error for a forced reap failure")
	}
	if !strings.Contains(err.Error(), "reap marker") {
		t.Errorf("error = %q, want it to name the reap failure", err)
	}
	if _, statErr := os.Stat(marker.WorktreePath); statErr != nil {
		t.Errorf("worktree should still exist after a refused reap: %v", statErr)
	}
}

// TestReconcileIsolationMarkersPreservesWhenSandboxDockerUnresolvable
// proves an inability to check for a live sandbox container preserves the
// marker rather than assuming none exists (found via review, round 2: an
// earlier version treated -sandbox-docker not being resolvable on this
// machine as "sandboxing was never in play" and skipped the check
// entirely -- but a prior sandboxed run's container can still be alive
// and holding the checkout even if this invocation's own environment
// can't currently reach Docker to prove it, and there is no reliable
// signal, for a run with no completed attempt yet, that it was never
// sandboxed to begin with). Passes a nonexistent Docker executable name,
// not "" -- "" is the operator's own explicit, deliberate opt-out (see
// reconcileMain's own flag help) and is exercised by every other
// reconcile test in this file via -sandbox-docker=""; this test is
// specifically about the unresolvable-but-configured case.
func TestReconcileIsolationMarkersPreservesWhenSandboxDockerUnresolvable(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "docker-unresolvable", "direct")
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir, WorkspacePath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save nonterminal run: %v", err)
	}
	// No live runner process for this marker either (liveRunnerProcessPID
	// finds no pid file, correctly returning "not running"), so the
	// sandbox-container check below is the only thing standing between
	// this marker and a reap -- isolating exactly what this test means to
	// prove.
	err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "definitely-not-a-real-docker-binary-xyz", lock, nil)
	if err != nil {
		t.Fatalf("reconcileIsolationMarkers returned an error for a preserved (not reaped) marker: %v", err)
	}
	if _, statErr := os.Stat(marker.WorktreePath); statErr != nil {
		t.Errorf("worktree should still exist -- Docker liveness was inconclusive, not proof of absence: %v", statErr)
	}
	if _, statErr := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); statErr != nil {
		t.Errorf("marker should still exist -- Docker liveness was inconclusive, not proof of absence: %v", statErr)
	}
}

func TestReconcileIsolationMarkersRequiresHeldRepositoryLock(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "lock-required", "direct")
	reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", nil, nil)
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Fatalf("reconciliation without lock removed worktree: %v", err)
	}
	lock := testHeldIsolationLock(t, repoDir)
	reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil)
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("reconciliation with lock did not reap worktree: %v", err)
	}
}

func TestReconcileIsolationMarkersPreservesAcceptedAndCompletedTemporalWork(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	accepted := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "accepted-run", "direct")
	if err := (&run.Run{ID: accepted.RunID, State: run.StateAccepted, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save accepted run: %v", err)
	}
	quarantined := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "quarantined-run", "direct")
	if err := (&run.Run{ID: quarantined.RunID, State: run.StateQuarantined, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save quarantined run: %v", err)
	}
	temporal := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-complete", "temporal")
	if err := wsisolation.RemoveIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, temporal.RunID)); err != nil {
		t.Fatalf("remove temporary marker path: %v", err)
	}
	temporal.RunID = "durable-temporal-complete"
	temporal.WorkflowID = "workflow-temporal-complete"
	temporal.ActivityRunID = "activity-run"
	temporal.ActivityID = "prepare-activity"
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, temporal.RunID), temporal); err != nil {
		t.Fatalf("write durable temporal marker: %v", err)
	}
	if err := (&run.Run{ID: temporal.RunID, State: run.StateSliceRunning, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save temporal run: %v", err)
	}
	checkpointDir := temporal.CheckpointDir
	if err := os.MkdirAll(filepath.Join(checkpointDir, "activity-checkpoints"), 0o750); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s", len(temporal.WorkflowID), temporal.WorkflowID, len(temporal.ActivityRunID), temporal.ActivityRunID, len(temporal.ActivityID), temporal.ActivityID)))
	checkpointPath := filepath.Join(checkpointDir, "activity-checkpoints", fmt.Sprintf("%x.json", key))
	checkpoint := `{"completed":true,"schema_version":2,"workflow_id":"` + temporal.WorkflowID + `","run_id":"` + temporal.ActivityRunID + `","activity_id":"` + temporal.ActivityID + `","result":{"worktree_path":"` + temporal.WorktreePath + `","branch":"` + temporal.Branch + `"}}`
	if err := os.WriteFile(checkpointPath, []byte(checkpoint), 0o600); err != nil {
		t.Fatalf("write prepare checkpoint: %v", err)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	for _, marker := range []wsisolation.IsolationMarker{accepted, quarantined, temporal} {
		if _, err := os.Stat(marker.WorktreePath); err != nil {
			t.Errorf("preserved worktree %s missing: %v", marker.RunID, err)
		}
		if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); err != nil {
			t.Errorf("preserved marker %s missing: %v", marker.RunID, err)
		}
		if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
			t.Errorf("cleanup %s: %v", marker.RunID, err)
		}
	}
}

// TestReconcileIsolationMarkersRevokesWorkerGroupWriteOnPreservedTerminalRun
// is the regression test for a real gap found via GitHub Codex App review of
// PR #62 (P2): runSandboxWithRetries's own defer is what normally revokes
// EnableWorkerGroupWrite's grant once a run reaches a terminal state and its
// worktree is kept rather than rolled back, but a factoryd process killed
// after the terminal run.json save and before that defer runs left the
// grant active forever -- reconciliation on restart previously preserved a
// terminal run's worktree unconditionally, without ever revoking it, so
// every process sharing factoryd's own GID kept write access to a
// host-owned worktree indefinitely.
func TestReconcileIsolationMarkersRevokesWorkerGroupWriteOnPreservedTerminalRun(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	accepted := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "accepted-ungranted", "direct")
	if err := (&run.Run{ID: accepted.RunID, State: run.StateAccepted, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save accepted run: %v", err)
	}
	if err := wsisolation.EnableWorkerGroupWrite(accepted.WorktreePath, os.Getgid()); err != nil {
		t.Fatalf("simulate un-revoked grant: %v", err)
	}
	info, err := os.Stat(accepted.WorktreePath)
	if err != nil {
		t.Fatalf("stat worktree before reconciliation: %v", err)
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Fatalf("precondition failed: worktree %s is not group-writable before reconciliation", accepted.WorktreePath)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	info, err = os.Stat(accepted.WorktreePath)
	if err != nil {
		t.Fatalf("stat preserved worktree: %v", err)
	}
	if info.Mode().Perm()&0o070 != 0 {
		t.Errorf("reconciliation left worktree %s group-accessible (mode %o), want group revoked", accepted.WorktreePath, info.Mode().Perm())
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, accepted.RunID)); err != nil {
		t.Errorf("preserved marker missing: %v", err)
	}
}

func TestReconcileIsolationMarkersPreservesMalformedAndLegacyEvidence(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "malformed-marker", "direct")
	marker.ParentDir = t.TempDir()
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, marker.RunID), marker); err != nil {
		t.Fatalf("write escaped marker: %v", err)
	}
	legacy := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "legacy-marker", "direct")
	legacy.Version = 99
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, legacy.RunID), legacy); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil)
	for _, marker := range []wsisolation.IsolationMarker{marker, legacy} {
		if _, err := os.Stat(marker.WorktreePath); err != nil {
			t.Errorf("preserved worktree %s missing: %v", marker.RunID, err)
		}
		if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
			t.Errorf("cleanup %s: %v", marker.RunID, err)
		}
	}
}

func TestReconcileIsolationMarkersReapsTemporalIntentWithoutCheckpoint(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-intent", "temporal")
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("Temporal intent worktree still exists after reconciliation: %v", err)
	}
}

func TestReconcileIsolationMarkersLeavesTemporalIntentWithoutWorktree(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-missing", "temporal")
	if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
		t.Fatalf("remove prepared worktree: %v", err)
	}
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, marker.RunID), marker); err != nil {
		t.Fatalf("restore marker: %v", err)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); err != nil {
		t.Fatalf("missing-worktree marker was not preserved: %v", err)
	}
}

func TestReconcileIsolationMarkersPreservesCheckpointPathMismatch(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-mismatch", "temporal")
	marker.ActivityRunID = "activity-run"
	marker.ActivityID = "prepare-activity"
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save temporal run: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(marker.CheckpointDir, "activity-checkpoints"), 0o750); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s", len(marker.WorkflowID), marker.WorkflowID, len(marker.ActivityRunID), marker.ActivityRunID, len(marker.ActivityID), marker.ActivityID)))
	checkpointPath := filepath.Join(marker.CheckpointDir, "activity-checkpoints", fmt.Sprintf("%x.json", key))
	checkpoint := `{"completed":true,"schema_version":2,"workflow_id":"` + marker.WorkflowID + `","run_id":"` + marker.ActivityRunID + `","activity_id":"` + marker.ActivityID + `","result":{"worktree_path":"wrong","branch":"wrong"}}`
	if err := os.WriteFile(checkpointPath, []byte(checkpoint), 0o600); err != nil {
		t.Fatalf("write conflicting checkpoint: %v", err)
	}
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, marker.RunID), marker); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Fatalf("conflicting checkpoint worktree was removed: %v", err)
	}
	if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestDirectRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError is
// the missing regression test for a real bug found live and fixed in PR #45:
// when less than sandboxAttemptTeardownMargin remains before the run's own
// deadline, but the deadline itself hasn't passed, ctx.Err() is nil -- so
// returning it unconditionally returned (zero-value Result, nil), which
// reads to every caller as a successful, empty attempt. Found empirically
// with -timeout 10s: a caller two levels up then hashed the zero-value
// LogPath as verify evidence and crashed on "open : no such file or
// directory" instead of halting with a legible cause. That fix landed with
// no test pinning it; this is that test. It never contacts Docker -- with no
// script and no spec to stage, the deadline check is reached before any
// launch. Its internal/workflow counterpart (the same bug, unfixed there
// until now) is
// TestRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError.
func TestDirectRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	logPath := func(int) string { return filepath.Join(logDir, "build.log") }

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(sandboxAttemptTeardownMargin/2))
	defer cancel()

	res, err := runSandboxWithRetries(ctx, t.TempDir(), "", "", "", logPath, 1, "factory-worker:test@sha256:deadbeef", "docker", "", sandbox.DefaultWorkerUID, "run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{}, "", "", nil, nil, "sh", "-c", "true")
	if err == nil {
		t.Fatalf("runSandboxWithRetries with less than the teardown margin left: want an error, got nil (result %+v)", res)
	}
	if !strings.Contains(err.Error(), "insufficient time remaining for a sandboxed attempt") {
		t.Fatalf("error = %q, want it to name the exhausted teardown margin", err.Error())
	}
}

// TestDirectRunSandboxWithRetriesDoesNotPadMarginForADisabledComposeSpec
// is the regression for a real bug found live while wiring compose
// services into this function: -compose-services defaults on, so a
// composeSpec is non-nil for nearly every call regardless of whether the
// target repo has a compose file at all, and an earlier version of the
// margin math padded margin by 2*ComposeServicesCleanupTimeout+
// ComposeServicesReadyTimeout (over 4 minutes at the package defaults)
// whenever composeSpec was merely non-nil -- not only when compose
// actually launched anything. That silently shrank every short-timeout
// caller's own effective attempt window repo-wide (this repo's own
// integration suite started halting on "insufficient time remaining"
// across dozens of otherwise-unrelated tests the moment -compose-services
// defaulted on). A composeSpec with no ComposeYAML (BeginComposeServices
// Lifecycle's own documented "disabled, not failed" outcome) must add
// nothing to margin.
func TestDirectRunSandboxWithRetriesDoesNotPadMarginForADisabledComposeSpec(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	logPath := func(int) string { return filepath.Join(logDir, "build.log") }

	// Inside sandboxAttemptTeardownMargin alone, but with plenty of room
	// for the padded (bugged) compose margin, this deadline must still
	// let the attempt run to completion rather than being refused as
	// having insufficient time remaining.
	ctx, cancel := context.WithTimeout(context.Background(), sandboxAttemptTeardownMargin*3)
	defer cancel()

	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	composeSpec := &sandbox.ComposeServicesSpec{} // empty ComposeYAML: disabled, matching a target repo with no compose file at all.
	res, err := runSandboxWithRetries(ctx, t.TempDir(), "", "", "", logPath, 1, "factory-worker:test@sha256:deadbeef", docker, sandboxTestUser(), sandbox.DefaultWorkerUID, "run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, composeSpec, sandbox.ComposeServicesHooks{}, "", "", nil, nil, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("runSandboxWithRetries with a disabled composeSpec = %v, want nil (result %+v)", err, res)
	}
}
