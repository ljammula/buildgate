package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// launchGate observes the launches of a fake reproduction Activity: how many
// were in flight at once, and, when a wave's size is given, holds each launch
// of the wave until the whole wave has started, so a launch scheduled only
// after another finished times out instead of passing.
type launchGate struct {
	mu          sync.Mutex
	arrived     map[string]int
	inFlight    int
	maxInFlight int
	timedOut    []string
}

func newLaunchGate() *launchGate { return &launchGate{arrived: map[string]int{}} }

// enter registers a launch of the wave named wave; want is the wave's size
// (0 for none: the launch only lingers, so an overlap would show).
func (g *launchGate) enter(wave string, want int) {
	g.mu.Lock()
	g.arrived[wave]++
	g.inFlight++
	g.maxInFlight = max(g.maxInFlight, g.inFlight)
	g.mu.Unlock()
	if want == 0 {
		time.Sleep(15 * time.Millisecond)
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.Lock()
		n := g.arrived[wave]
		g.mu.Unlock()
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			g.mu.Lock()
			g.timedOut = append(g.timedOut, wave)
			g.mu.Unlock()
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *launchGate) leave() {
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
}

func threeTestsRedFiles() TestsRedHoldResult {
	var held []run.HeldReproduction
	for i, name := range []string{"a", "b", "c"} {
		held = append(held, run.HeldReproduction{Finding: i, Path: "pkg/" + name + "_test.go", SHA256: "h" + name, HostPath: "/fixture/held/" + name, TestIDs: []string{"TestA"}, Runner: "go-test-json"})
	}
	return TestsRedHoldResult{Held: held}
}

// testsRedLaunch answers a launch by the file's finding and pass.
func testsRedLaunch(gate *launchGate, wave func(pass string) int, verdicts map[string]ReproductionRunResult, failing string) func(context.Context, ReproductionRunInput) (ReproductionRunResult, error) {
	return func(_ context.Context, in ReproductionRunInput) (ReproductionRunResult, error) {
		gate.enter(in.Pass, wave(in.Pass))
		defer gate.leave()
		key := fmt.Sprintf("%d-%s", in.Held.Finding, in.Pass)
		if key == failing {
			return ReproductionRunResult{}, temporal.NewNonRetryableApplicationError("the sandbox is gone", InfrastructureFailureType, errors.New("gone"))
		}
		res := verdicts[key]
		res.Run.Pass, res.Run.Commit = in.Pass, in.Commit
		res.Attempts = []run.Attempt{{Kind: "tests_red_on_base", Command: []string{key}}}
		return res, nil
	}
}

func testsRedAttemptCommands(r RunWorkflowResult) []string {
	var out []string
	for _, a := range r.Attempts {
		if a.Kind == "tests_red_on_base" {
			out = append(out, a.Command[0])
		}
	}
	return out
}

// checkTestsRedProof is the proof and attempt order the three files must have.
func checkTestsRedProof(t *testing.T, p *testsRedPhase, wantVerdict string, wantRuns []int, wantAttempt []string) {
	t.Helper()
	proof := p.result.TestsRedOnBase
	if proof == nil || proof.Verdict != wantVerdict || len(proof.Files) != 3 {
		t.Fatalf("proof = %+v, want verdict %s over 3 files", proof, wantVerdict)
	}
	for i, f := range proof.Files {
		if f.SHA256 != "h"+string(rune('a'+i)) || len(f.Runs) != wantRuns[i] || f.Runs[0].Pass != run.TestsRedPassResult || f.Runs[0].Commit != "result-sha" {
			t.Errorf("file %d = %+v, want hash h%c, %d runs, the result run first on result-sha", i, f, 'a'+i, wantRuns[i])
		}
		if wantRuns[i] == 2 && (f.Runs[1].Pass != run.TestsRedPassBase || f.Runs[1].Commit != "fixture-base-sha") {
			t.Errorf("file %d second run = %+v, want the base run on fixture-base-sha", i, f.Runs[1])
		}
	}
	if got := testsRedAttemptCommands(p.result); !slices.Equal(got, wantAttempt) {
		t.Errorf("attempts = %v, want %v (file order, result before base)", got, wantAttempt)
	}
}

// The result launches of every held file start together, then the base
// launches of the files whose result did not fail start together; the proof
// is the one the serial order reaches.
func TestTestsRedLaunchesRunInWavesWithTheSerialVerdicts(t *testing.T) {
	pass := testsRedKinds(reproduction.OutcomePassed, reproduction.KindPass)
	fail := testsRedKinds(reproduction.OutcomeFailed, reproduction.KindFail)
	for _, tc := range []struct {
		name        string
		verdicts    map[string]ReproductionRunResult
		wantBases   []string
		wantRuns    []int
		wantVerdict string
		wantAttempt []string
	}{
		{
			"all red on the base",
			map[string]ReproductionRunResult{"0-result": pass, "1-result": pass, "2-result": pass, "0-base": fail, "1-base": fail, "2-base": fail},
			[]string{"tests-red-0-base", "tests-red-1-base", "tests-red-2-base"}, []int{2, 2, 2}, run.TestsRedProved,
			[]string{"0-result", "0-base", "1-result", "1-base", "2-result", "2-base"},
		},
		{
			"one result fails: its base is not launched",
			map[string]ReproductionRunResult{"0-result": pass, "1-result": fail, "2-result": pass, "0-base": fail, "2-base": fail},
			[]string{"tests-red-0-base", "tests-red-2-base"}, []int{2, 1, 2}, run.TestsRedProved,
			[]string{"0-result", "0-base", "1-result", "2-result", "2-base"},
		},
		{
			"one file passes on the base too, the others prove red",
			map[string]ReproductionRunResult{"0-result": pass, "1-result": pass, "2-result": pass, "0-base": fail, "1-base": pass, "2-base": fail},
			[]string{"tests-red-0-base", "tests-red-1-base", "tests-red-2-base"}, []int{2, 2, 2}, run.TestsRedProved,
			[]string{"0-result", "0-base", "1-result", "1-base", "2-result", "2-base"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newLaunchGate()
			wave := func(pass string) int {
				if pass == run.TestsRedPassResult {
					return 3
				}
				return len(tc.wantBases)
			}
			p := runTestsRedPhaseWith(t, testsRedInput(), threeTestsRedFiles(), 0, true, false, testsRedLaunch(gate, wave, tc.verdicts, ""))
			if p.err != nil {
				t.Fatalf("workflow error: %v", p.err)
			}
			if len(gate.timedOut) != 0 || gate.maxInFlight != max(3, len(tc.wantBases)) {
				t.Errorf("timed-out waves %v, most launches at once %d; want none, %d", gate.timedOut, gate.maxInFlight, max(3, len(tc.wantBases)))
			}
			wantCalls := append([]string{"tests-red-hold", "tests-red-0-result", "tests-red-1-result", "tests-red-2-result"}, tc.wantBases...)
			if len(p.calls) != len(wantCalls) || p.calls[0] != "tests-red-hold" {
				t.Fatalf("calls = %v, want %v in some order within a wave", p.calls, wantCalls)
			}
			gotResults, gotBases := slices.Clone(p.calls[1:4]), slices.Clone(p.calls[4:])
			slices.Sort(gotResults)
			slices.Sort(gotBases)
			if !slices.Equal(gotResults, wantCalls[1:4]) || !slices.Equal(gotBases, tc.wantBases) {
				t.Errorf("result launches %v and base launches %v, want %v then %v: every base after every result", gotResults, gotBases, wantCalls[1:4], tc.wantBases)
			}
			checkTestsRedProof(t, p, tc.wantVerdict, tc.wantRuns, tc.wantAttempt)
			proof := p.result.TestsRedOnBase
			serial := runTestsRedPhaseWith(t, testsRedInput(), threeTestsRedFiles(), 0, true, true, testsRedLaunch(newLaunchGate(), func(string) int { return 0 }, tc.verdicts, ""))
			if serial.err != nil || !reflect.DeepEqual(serial.result.TestsRedOnBase, proof) || !slices.Equal(testsRedAttemptCommands(serial.result), tc.wantAttempt) {
				t.Errorf("serial proof %+v (err %v) differs from the waves' %+v", serial.result.TestsRedOnBase, serial.err, proof)
			}
		})
	}
}

// A history recorded before the change keeps one launch at a time, each file's
// result then its base, in file order.
func TestHistoryRecordedBeforeParallelReproductionLaunchesRunsTestsRedSerially(t *testing.T) {
	pass := testsRedKinds(reproduction.OutcomePassed, reproduction.KindPass)
	fail := testsRedKinds(reproduction.OutcomeFailed, reproduction.KindFail)
	verdicts := map[string]ReproductionRunResult{"0-result": pass, "1-result": pass, "2-result": fail, "0-base": fail, "1-base": fail}
	gate := newLaunchGate()
	p := runTestsRedPhaseWith(t, testsRedInput(), threeTestsRedFiles(), 0, true, true, testsRedLaunch(gate, func(string) int { return 0 }, verdicts, ""))
	want := []string{"tests-red-hold", "tests-red-0-result", "tests-red-0-base", "tests-red-1-result", "tests-red-1-base", "tests-red-2-result"}
	if p.err != nil || !slices.Equal(p.calls, want) || gate.maxInFlight != 1 {
		t.Errorf("err %v, calls %v, most at once %d; want %v one at a time", p.err, p.calls, gate.maxInFlight, want)
	}
}

// A launch that fails is the run's failure; the wave's other launches have been
// started and finished before the run ends, and no base launch follows.
func TestTestsRedWaveWaitsForEveryLaunchBeforeItsFirstErrorEndsTheRun(t *testing.T) {
	pass := testsRedKinds(reproduction.OutcomePassed, reproduction.KindPass)
	verdicts := map[string]ReproductionRunResult{"0-result": pass, "1-result": pass, "2-result": pass}
	gate := newLaunchGate()
	wave := func(pass string) int {
		if pass == run.TestsRedPassResult {
			return 3
		}
		return 0
	}
	p := runTestsRedPhaseWith(t, testsRedInput(), threeTestsRedFiles(), 0, true, false, testsRedLaunch(gate, wave, verdicts, "1-result"))
	if p.err == nil || !strings.Contains(p.err.Error(), "the sandbox is gone") {
		t.Fatalf("workflow error = %v, want the launch's error", p.err)
	}
	if len(p.calls) != 4 || len(gate.timedOut) != 0 || gate.maxInFlight != 3 || gate.inFlight != 0 {
		t.Errorf("calls %v, timed out %v, most at once %d, in flight %d; want the hold and three result launches, none left running", p.calls, gate.timedOut, gate.maxInFlight, gate.inFlight)
	}
}

// --- challenge ---

// challengeJobFiles are three runner-covered files and two that no runner
// covers, which run together as the group.
func challengeJobFiles() ChallengerJobResult {
	return ChallengerJobResult{Files: []run.ChallengeFile{
		challengeFile("p/a_test.go", "AC-1", "go-test-json"),
		challengeFile("p/b_test.go", "AC-2", "go-test-json"),
		challengeFile("p/c_test.go", "AC-3", "go-test-json"),
		challengeFile("a_test.rb", "AC-4", ""),
		challengeFile("b_test.rb", "AC-5", ""),
	}}
}

// challengeLaunch answers a launch by unit and pass: outcomes maps
// "<finding>-<pass>" to red ("fail") or anything else (green).
func challengeLaunch(gate *launchGate, waves map[string]int, red map[string]bool) func(ReproductionRunInput) ReproductionRunResult {
	return func(in ReproductionRunInput) ReproductionRunResult {
		gate.enter(in.Pass, waves[in.Pass])
		defer gate.leave()
		key := fmt.Sprintf("%d-%s", in.Held.Finding, in.Pass)
		res := passAll(in)
		if red[key] {
			res = failAll(in)
		}
		res.Run.Pass, res.Run.Commit = in.Pass, in.Commit
		res.Attempts = []run.Attempt{{Kind: "challenge", Command: []string{"challenge-run-" + key}}}
		return res
	}
}

func challengeAttemptCommands(r RunWorkflowResult) []string {
	var out []string
	for _, a := range r.Attempts {
		if a.Kind == "challenge" && len(a.Command) > 0 && strings.HasPrefix(a.Command[0], "challenge-run-") {
			out = append(out, a.Command[0])
		}
	}
	return out
}

func challengeLaunchPasses(c *run.ChallengeRecord) []string {
	var out []string
	for _, l := range c.Launches {
		out = append(out, l.Pass)
	}
	return out
}

// Every unit's first pass starts together, then the second pass of each red
// first pass together; the record is the one the serial order writes.
func TestChallengeLaunchesRunInWavesWithTheSerialRecord(t *testing.T) {
	// unit 0: red, red; 1: red, then green; 2: green; group (-1): green.
	redRed := map[string]bool{"0-challenge1": true, "0-challenge2": true, "1-challenge1": true}
	allRed := map[string]bool{"0-challenge1": true, "0-challenge2": true, "1-challenge1": true, "1-challenge2": true, "2-challenge1": true, "2-challenge2": true, "-1-challenge1": true, "-1-challenge2": true}
	for _, tc := range []struct {
		name         string
		red          map[string]bool
		launchErr    func(ReproductionRunInput) error
		waves        map[string]int
		wantVerdicts []string
		wantVerdict  string
		wantPasses   []string
		wantAttempts []string
		wantNote     string
	}{
		{
			"red-red, red-then-green, green and a green group",
			redRed, nil, map[string]int{challengePass1: 4, challengePass2: 2},
			[]string{run.ChallengeRed, "advice", run.ChallengeGreen, run.ChallengeGreen, run.ChallengeGreen}, run.ChallengeRed,
			[]string{challengePass1, challengePass2, challengePass1, challengePass2, challengePass1, challengePass1},
			[]string{"challenge-run-0-challenge1", "challenge-run-0-challenge2", "challenge-run-1-challenge1", "challenge-run-1-challenge2", "challenge-run-2-challenge1", "challenge-run--1-challenge1"},
			"a challenge file was red once and then not: p/b_test.go",
		},
		{
			"every unit red twice, the group too",
			allRed, nil, map[string]int{challengePass1: 4, challengePass2: 4},
			[]string{run.ChallengeRed, run.ChallengeRed, run.ChallengeRed, run.ChallengeRed, run.ChallengeRed}, run.ChallengeRed,
			[]string{challengePass1, challengePass2, challengePass1, challengePass2, challengePass1, challengePass2, challengePass1, challengePass2},
			[]string{"challenge-run-0-challenge1", "challenge-run-0-challenge2", "challenge-run-1-challenge1", "challenge-run-1-challenge2", "challenge-run-2-challenge1", "challenge-run-2-challenge2", "challenge-run--1-challenge1", "challenge-run--1-challenge2"},
			"",
		},
		{
			"all green",
			nil, nil, map[string]int{challengePass1: 4},
			[]string{run.ChallengeGreen, run.ChallengeGreen, run.ChallengeGreen, run.ChallengeGreen, run.ChallengeGreen}, run.ChallengeGreen,
			[]string{challengePass1, challengePass1, challengePass1, challengePass1},
			[]string{"challenge-run-0-challenge1", "challenge-run-1-challenge1", "challenge-run-2-challenge1", "challenge-run--1-challenge1"},
			"",
		},
		{
			"a launch error is that unit's no verdict",
			map[string]bool{"0-challenge1": true, "0-challenge2": true},
			func(in ReproductionRunInput) error {
				if in.Held.Finding == 1 {
					return temporal.NewNonRetryableApplicationError("the sandbox is gone", InfrastructureFailureType, errors.New("gone"))
				}
				return nil
			},
			map[string]int{challengePass1: 3, challengePass2: 1}, // the failing launch never reaches the gate
			[]string{run.ChallengeRed, "", run.ChallengeGreen, run.ChallengeGreen, run.ChallengeGreen}, run.ChallengeRed,
			[]string{challengePass1, challengePass2, challengePass1, challengePass1},
			[]string{"challenge-run-0-challenge1", "challenge-run-0-challenge2", "challenge-run-2-challenge1", "challenge-run--1-challenge1"},
			"the factory's run of a challenge file gave no verdict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newLaunchGate()
			fx := &challengeFixture{launchErr: tc.launchErr}
			p := runChallengePhaseWith(t, challengeInput(), challengeJobFiles(), challengeLaunch(gate, tc.waves, tc.red), 1, nil, fx)
			c := p.result.Challenge
			if p.err != nil || c == nil || p.result.State != run.StateAccepted {
				t.Fatalf("err %v, challenge %+v, state %s", p.err, c, p.result.State)
			}
			var got []string
			for _, f := range c.Files {
				got = append(got, f.Verdict)
			}
			if !slices.Equal(got, tc.wantVerdicts) || c.Verdict != tc.wantVerdict {
				t.Errorf("file verdicts %q, record %q; want %q, %q", got, c.Verdict, tc.wantVerdicts, tc.wantVerdict)
			}
			if passes := challengeLaunchPasses(c); !slices.Equal(passes, tc.wantPasses) {
				t.Errorf("recorded launches %v, want %v (unit order, pass 1 before pass 2)", passes, tc.wantPasses)
			}
			if a := challengeAttemptCommands(p.result); !slices.Equal(a, tc.wantAttempts) {
				t.Errorf("attempts %v, want %v", a, tc.wantAttempts)
			}
			if tc.wantNote != "" && !slices.ContainsFunc(c.Notes, func(n string) bool { return strings.Contains(n, tc.wantNote) }) {
				t.Errorf("notes %q, want one containing %q", c.Notes, tc.wantNote)
			}
			wantMax := max(tc.waves[challengePass1], tc.waves[challengePass2])
			if len(gate.timedOut) != 0 || gate.maxInFlight != wantMax || gate.inFlight != 0 {
				t.Errorf("timed-out waves %v, most at once %d, left running %d; want none, %d, 0", gate.timedOut, gate.maxInFlight, gate.inFlight, wantMax)
			}
			wantIDs := make([]string, 0, len(tc.wantAttempts))
			for _, a := range tc.wantAttempts {
				wantIDs = append(wantIDs, strings.TrimPrefix(a, "challenge-run-"))
			}
			if tc.launchErr != nil {
				wantIDs = append(wantIDs, "1-challenge1")
			}
			var gotIDs []string
			for _, id := range p.launches {
				gotIDs = append(gotIDs, strings.TrimPrefix(id, "challenge-run-"))
			}
			slices.Sort(wantIDs)
			slices.Sort(gotIDs)
			if !slices.Equal(gotIDs, wantIDs) {
				t.Errorf("Activities launched %v, want %v", gotIDs, wantIDs)
			}
			serial := runChallengePhaseWith(t, challengeInput(), challengeJobFiles(), challengeLaunch(newLaunchGate(), nil, tc.red), 1, nil, &challengeFixture{launchErr: tc.launchErr, serialLaunches: true})
			if serial.err != nil || !reflect.DeepEqual(serial.result.Challenge, c) || !slices.Equal(challengeAttemptCommands(serial.result), tc.wantAttempts) {
				t.Errorf("serial record %+v (err %v) differs from the waves' %+v", serial.result.Challenge, serial.err, c)
			}
		})
	}
}

// A history recorded before the change launches one unit at a time, each
// unit's second pass straight after its first.
func TestHistoryRecordedBeforeParallelReproductionLaunchesRunsTheChallengeSerially(t *testing.T) {
	gate := newLaunchGate()
	red := map[string]bool{"0-challenge1": true, "0-challenge2": true, "1-challenge1": true}
	p := runChallengePhaseWith(t, challengeInput(), challengeJobFiles(), challengeLaunch(gate, nil, red), 1, nil, &challengeFixture{serialLaunches: true})
	want := []string{"challenge-run-0-challenge1", "challenge-run-0-challenge2", "challenge-run-1-challenge1", "challenge-run-1-challenge2", "challenge-run-2-challenge1", "challenge-run--1-challenge1"}
	if p.err != nil || !slices.Equal(p.launches, want) || gate.maxInFlight != 1 {
		t.Errorf("err %v, launches %v, most at once %d; want %v one at a time", p.err, p.launches, gate.maxInFlight, want)
	}
}

// A run with compose services keeps one launch at a time in the new version too:
// its services' project and network are the run's, not the launch's.
func TestComposeServicesRunsLaunchOneAtATime(t *testing.T) {
	gate := newLaunchGate()
	input := challengeInput()
	input.ComposeServicesEnabled = true
	p := runChallengePhaseWith(t, input, challengeJobFiles(), challengeLaunch(gate, nil, nil), 1, nil, &challengeFixture{})
	if p.err != nil || len(p.launches) != 4 || gate.maxInFlight != 1 {
		t.Errorf("err %v, launches %v, most at once %d; want four launches one at a time", p.err, p.launches, gate.maxInFlight)
	}
}
