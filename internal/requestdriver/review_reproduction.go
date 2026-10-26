package requestdriver

import (
	"encoding/json"
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

// recheckManifestFor is the run.RecheckManifest a build that follows
// quarantined is given when a reviewer's reproduction refused quarantined
// (SC-022): each blocking finding's held copy, under quarantined's own
// review-repro directory, to run once on that build's result. ok is false
// when there is nothing a further build may be given: no blocking finding
// whose held copy is on disk with the hash the run recorded, or a recheck
// quarantined itself ran that is still failing (the recheck runs once: a
// corrective build that left a reproduction failing is not followed by
// another).
func recheckManifestFor(dataDir string, quarantined *run.Run) (m run.RecheckManifest, ok bool) {
	for _, rc := range quarantined.ReviewReproductionRechecks {
		if rc.Verdict != reproductionPasses {
			return run.RecheckManifest{}, false
		}
	}
	if quarantined.CodeReview == nil {
		return run.RecheckManifest{}, false
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, quarantined.ID)))
	if err != nil {
		return run.RecheckManifest{}, false
	}
	m.RunID = quarantined.ID
	for i, f := range quarantined.CodeReview.Findings {
		if f.Disposition != run.DispositionBlocks || f.Reproduction == nil {
			continue
		}
		held, ok := heldBlockingCopy(dir, i, f)
		if !ok {
			return run.RecheckManifest{}, false
		}
		m.Reproductions = append(m.Reproductions, held)
	}
	return m, len(m.Reproductions) > 0
}

// heldBlockingCopy is the held copy of finding i's reproduction under dir
// (the run's review-repro directory), when the file there has the hash the
// run recorded for it.
func heldBlockingCopy(dir string, i int, f run.CodeReviewFinding) (run.HeldReproduction, bool) {
	rep := f.Reproduction
	if rep.SHA256 == "" || rep.Runner == "" || len(rep.TestIDs) == 0 || !filepath.IsLocal(filepath.FromSlash(rep.Path)) {
		return run.HeldReproduction{}, false
	}
	host := filepath.Join(dir, strconv.Itoa(i), filepath.FromSlash(rep.Path))
	if sum, err := evidence.SHA256File(host); err != nil || sum != rep.SHA256 {
		return run.HeldReproduction{}, false
	}
	held := run.HeldReproduction{Finding: i, Path: rep.Path, SHA256: rep.SHA256, HostPath: host, TestIDs: rep.TestIDs, Runner: rep.Runner}
	if f.SpecLine != nil {
		held.SpecLine = f.SpecLine.Number
	}
	return held, true
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

// withRecheck appends -recheck-reproductions to args when quarantined was
// refused by a reviewer's reproduction: the manifest (recheckManifestFor) is
// written in dir. A build given no -review-reproduction runs no reproduction,
// so it is given no manifest either. ok is false when quarantined was
// refused by a reproduction and no manifest can be made for it: a build
// that follows it would not run the reproduction again.
func withRecheck(dataDir, dir string, quarantined *run.Run, args []string) (argv []string, ok bool, err error) {
	if !refusedByAReproduction(quarantined) {
		return args, true, nil
	}
	m, ok := recheckManifestFor(dataDir, quarantined)
	if !ok || !hasFlag(args, "-review-reproduction") {
		return args, false, nil
	}
	path, err := writeRecheckManifest(dir, m)
	if err != nil {
		return args, false, err
	}
	return append(args, "-recheck-reproductions", path), true, nil
}

// hasFlag reports whether flag is one of argv's arguments.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}
