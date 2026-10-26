package requestdriver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/run"
)

// recheckFileName is a build's -recheck-reproductions manifest, written
// beside the other files of the round that build is.
const recheckFileName = "recheck.json"

// reproductionPasses is the verdict a recheck records for a reproduction
// that passed on every test ID on the result (reproduction.VerdictPasses).
const reproductionPasses = "passes"

// needsRecheck reports whether a build that continues on previous's commits
// must run reviewer reproductions again (SC-022): a reproduction refused
// previous, or previous itself ran rechecks, passing or not (the build must
// not undo a fix earlier in the ticket's chain).
func needsRecheck(previous *run.Run) bool {
	return refusedByAReproduction(previous) || len(previous.ReviewReproductionRechecks) > 0
}

// recheckManifestFor is the run.RecheckManifest a build that follows
// previous is given: every recheck previous ran (the entry it ran, under the
// review-repro directory of the run that held it), and each of previous's own
// blocking findings' held copies, without repeats (by hash and path). Each
// entry's file is on disk with the hash recorded for it. An error says why
// none can be made when needsRecheck(previous) and an entry cannot be found
// again, or nothing is left to run.
func recheckManifestFor(dataDir string, previous *run.Run) (run.RecheckManifest, error) {
	var m run.RecheckManifest
	seen := map[string]bool{}
	add := func(e run.RecheckEntry) {
		if key := e.SHA256 + " " + e.Path; !seen[key] {
			seen[key] = true
			m.Reproductions = append(m.Reproductions, e)
		}
	}
	for i, rc := range previous.ReviewReproductionRechecks {
		if rc.Recheck == nil || !heldCopyOnDisk(dataDir, *rc.Recheck) {
			return run.RecheckManifest{}, fmt.Errorf("run %s's recheck %d names no held copy that is still on disk with its hash", previous.ID, i)
		}
		add(*rc.Recheck)
	}
	own, err := blockingCopies(dataDir, previous)
	if err != nil {
		return run.RecheckManifest{}, err
	}
	for _, e := range own {
		add(e)
	}
	if len(m.Reproductions) == 0 {
		return run.RecheckManifest{}, fmt.Errorf("run %s holds no reviewer reproduction to run again", previous.ID)
	}
	return m, nil
}

// blockingCopies is the recheck entry of each of r's blocking findings: its
// held copy under r's own review-repro directory.
func blockingCopies(dataDir string, r *run.Run) ([]run.RecheckEntry, error) {
	if r.CodeReview == nil {
		return nil, nil
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, r.ID)))
	if err != nil {
		return nil, err
	}
	var out []run.RecheckEntry
	for i, f := range r.CodeReview.Findings {
		if f.Disposition != run.DispositionBlocks || f.Reproduction == nil {
			continue
		}
		e, ok := heldBlockingCopy(r.ID, dir, i, f)
		if !ok || !heldCopyOnDisk(dataDir, e) {
			return nil, fmt.Errorf("run %s's blocking finding %d has no held copy on disk with its hash", r.ID, i)
		}
		out = append(out, e)
	}
	return out, nil
}

// heldBlockingCopy is the recheck entry of finding i's reproduction, held by
// run runID under dir (its review-repro directory).
func heldBlockingCopy(runID, dir string, i int, f run.CodeReviewFinding) (run.RecheckEntry, bool) {
	rep := f.Reproduction
	if rep.SHA256 == "" || rep.Runner == "" || len(rep.TestIDs) == 0 || !filepath.IsLocal(filepath.FromSlash(rep.Path)) {
		return run.RecheckEntry{}, false
	}
	held := run.HeldReproduction{Finding: i, Path: rep.Path, SHA256: rep.SHA256, HostPath: filepath.Join(dir, strconv.Itoa(i), filepath.FromSlash(rep.Path)), TestIDs: rep.TestIDs, Runner: rep.Runner}
	if f.SpecLine != nil {
		held.SpecLine = f.SpecLine.Number
	}
	return run.RecheckEntry{RunID: runID, HeldReproduction: held}, true
}

// heldCopyOnDisk reports whether e's host path is a file under its run's
// review-repro directory in dataDir with the hash e records. factoryd checks
// every entry again, links included, before the build starts.
func heldCopyOnDisk(dataDir string, e run.RecheckEntry) bool {
	if !run.ValidID(e.RunID) || e.SHA256 == "" {
		return false
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, e.RunID)))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, e.HostPath)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	sum, err := evidence.SHA256File(e.HostPath)
	return err == nil && sum == e.SHA256
}

// aRecheckStillFails reports whether a recheck r ran is still failing. The
// recheck runs once per automatic correction: a corrective build that left a
// reproduction failing is not followed by another automatic one (a
// corrective round, a PR-review round's fix attempt). An operator's retry,
// resume or a later PR-review round may follow it, and rechecks again.
func aRecheckStillFails(r *run.Run) bool {
	for _, rc := range r.ReviewReproductionRechecks {
		if rc.Verdict != reproductionPasses {
			return true
		}
	}
	return false
}

// automaticCorrectionMayFollow reports whether an automatic corrective
// build may follow quarantined on its commits: a build may be given its
// record (handoffForABuild), no recheck it ran is still failing
// (aRecheckStillFails), and the recheck that build must run can be made
// (recheckManifestFor).
func automaticCorrectionMayFollow(dataDir string, quarantined *run.Run) bool {
	if _, ok := handoffForABuild(dataDir, quarantined); !ok || aRecheckStillFails(quarantined) {
		return false
	}
	if !needsRecheck(quarantined) {
		return true
	}
	_, err := recheckManifestFor(dataDir, quarantined)
	return err == nil
}

// refusedByAReproduction reports whether a reviewer's reproduction refused
// runRecord (its review_reproduction check failed).
func refusedByAReproduction(runRecord *run.Run) bool {
	return failedGate(runRecord, policy.ReviewReproductionCheck)
}

// onlyAReproductionRefused reports whether review_reproduction is the only
// check runRecord failed.
func onlyAReproductionRefused(runRecord *run.Run) bool {
	for _, g := range runRecord.GateResults {
		if !g.Passed && g.Check != policy.ReviewReproductionCheck {
			return false
		}
	}
	return refusedByAReproduction(runRecord)
}

// writeRecheckManifest writes m as dir's recheckFileName and returns its
// path. The file is the factory's, outside any workspace; factoryd checks
// every entry again before the build starts.
func writeRecheckManifest(dir string, m run.RecheckManifest) (string, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the recheck manifest: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, recheckFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// withRecheck appends -recheck-reproductions to args when a build that
// continues on previous's commits must run reviewer reproductions again
// (needsRecheck): the manifest (recheckManifestFor) is written in dir. The
// error says why such a build may not start (SC-022): no manifest can be
// made, or args lack -review-reproduction, so it would run none. It is the
// one rule every build that follows a run goes through (a corrective round,
// a retry, a resume, a PR-review round); none is built without the recheck.
func withRecheck(dataDir, dir string, previous *run.Run, args []string) ([]string, error) {
	if !needsRecheck(previous) {
		return args, nil
	}
	m, err := recheckManifestFor(dataDir, previous)
	if err != nil {
		return args, fmt.Errorf("%w: %w", errNoRecheck, err)
	}
	if !hasFlag(args, "-review-reproduction") {
		return args, fmt.Errorf("%w: run %s needs its reviewer reproductions run again and the build runs none", errNoRecheck, previous.ID)
	}
	path, err := writeRecheckManifest(dir, m)
	if err != nil {
		return args, err
	}
	return append(args, "-recheck-reproductions", path), nil
}

// errNoRecheck is in the chain of withRecheck's refusals: a build that
// would continue on a run's commits without the reproductions that run must
// pass again.
var errNoRecheck = errors.New("not built without a recheck of the reviewer's reproductions")

// hasFlag reports whether flag is one of argv's arguments.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}
