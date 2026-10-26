package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"buildgate/internal/evidence"
	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// reviewSpecSnapshotName is -review-spec's snapshot in the run's directory.
// Its name differs from the ticket spec's snapshot: the review launch
// stages both in one directory.
const reviewSpecSnapshotName = "approved-spec.snapshot.md"

// Caps on the files the review-reproduction flags name.
const (
	maxReviewSpecBytes      = 1 << 20
	maxRecheckManifestBytes = 1 << 20
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// snapshotReviewInputs snapshots -review-spec into the run's directory and
// records its hash, and checks -recheck-reproductions (loadRecheckManifest),
// before anything is built. A manifest needs -review-reproduction.
func (tr *ticketRun) snapshotReviewInputs() error {
	if *tr.rf.reviewSpec != "" {
		spec, err := evidence.ReadHostileFile(*tr.rf.reviewSpec, maxReviewSpecBytes)
		if err != nil {
			return fmt.Errorf("-review-spec: %w", err)
		}
		snapshot, err := filepath.Abs(filepath.Join(run.Dir(*tr.dataDir, tr.id), reviewSpecSnapshotName))
		if err != nil {
			return fmt.Errorf("resolve the approved spec's snapshot path: %w", err)
		}
		if err := os.WriteFile(snapshot, spec, 0o600); err != nil {
			return fmt.Errorf("snapshot the approved spec: %w", err)
		}
		sum := sha256.Sum256(spec)
		tr.reviewSpecSnapshot, tr.reviewSpecSHA256 = snapshot, hex.EncodeToString(sum[:])
	}
	if *tr.rf.recheckReproductions == "" {
		return nil
	}
	if !*tr.rf.reviewReproduction {
		return errors.New("-recheck-reproductions needs -review-reproduction")
	}
	rechecks, err := loadRecheckManifest(*tr.dataDir, *tr.rf.recheckReproductions)
	if err != nil {
		return fmt.Errorf("-recheck-reproductions: %w", err)
	}
	tr.rechecks = rechecks
	return nil
}

// loadRecheckManifest reads a run.RecheckManifest and checks it: for each
// reproduction a valid run id of a run of the manifest's request, and a held
// copy (no refusal) whose host path
// is a regular file, reached through no link, under that run's review-repro
// directory in dataDir, with a SHA-256, a runner the registry has and
// between one and reproduction.MaxTestIDs test IDs. The copies' content is
// checked against the hash by the workflow before each launch.
func loadRecheckManifest(dataDir, path string) ([]run.RecheckEntry, error) {
	data, err := evidence.ReadHostileFile(path, maxRecheckManifestBytes)
	if err != nil {
		return nil, err
	}
	var m run.RecheckManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(m.Reproductions) == 0 {
		return nil, errors.New("it names no reproduction")
	}
	for i, e := range m.Reproductions {
		if err := checkRecheckRun(dataDir, m.RequestID, e); err != nil {
			return nil, fmt.Errorf("reproduction %d: %w", i, err)
		}
	}
	return m.Reproductions, nil
}

// checkRecheckRun checks e's run: a valid id of a run of request requestID
// (the request the build is a ticket of), and its held copy
// (checkRecheckEntry) against that run's review-repro directory.
func checkRecheckRun(dataDir, requestID string, e run.RecheckEntry) error {
	if !run.ValidID(e.RunID) {
		return fmt.Errorf("run id %q is not valid", e.RunID)
	}
	held, err := run.Load(dataDir, e.RunID)
	if err != nil {
		return fmt.Errorf("run %s: %w", e.RunID, err)
	}
	if requestID == "" || held.RequestID != requestID {
		return fmt.Errorf("run %s is not a run of request %q", e.RunID, requestID)
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, e.RunID)))
	if err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("run %s holds no reproductions: %w", e.RunID, err)
	}
	return checkRecheckEntry(dir, realDir, e.HeldReproduction)
}

// checkRecheckEntry is loadRecheckManifest's check of one entry.
func checkRecheckEntry(dir, realDir string, h run.HeldReproduction) error {
	switch {
	case h.Refused != "" || h.Path == "":
		return errors.New("it is not a held reproduction")
	case !reproduction.IsCleanRelative(h.Path):
		return fmt.Errorf("its path %q is not a clean relative path", h.Path)
	case !sha256Hex.MatchString(h.SHA256):
		return errors.New("its sha256 is not a SHA-256")
	case len(h.TestIDs) == 0 || len(h.TestIDs) > reproduction.MaxTestIDs:
		return errors.New("it names no test IDs, or too many")
	}
	if _, ok := reproduction.ByID(h.Runner); !ok {
		return fmt.Errorf("runner %q is not a reproduction runner", h.Runner)
	}
	if !within(dir, h.HostPath) {
		return fmt.Errorf("host path %q is not under %s", h.HostPath, dir)
	}
	info, err := os.Lstat(h.HostPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("host path %q is not a regular file", h.HostPath)
	}
	resolved, err := filepath.EvalSymlinks(h.HostPath)
	if err != nil {
		return err
	}
	if !within(realDir, resolved) {
		return fmt.Errorf("host path %q leaves %s through a link", h.HostPath, dir)
	}
	return nil
}

// within reports whether p is a clean absolute path strictly below dir.
func within(dir, p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mergeReviewReproductions copies what the factory decided about each
// finding's reproduction onto the run record's finding of the same index,
// when that finding carries a reproduction at the same path, and records a
// corrective build's rechecks. The decisions come from the workflow's
// result; the findings from the retained evidence file.
func mergeReviewReproductions(r *run.Run, decided []run.FindingReproduction, rechecks []run.ReviewReproduction) {
	r.ReviewReproductionRechecks = rechecks
	if r.CodeReview == nil {
		return
	}
	for _, d := range decided {
		if d.Finding < 0 || d.Finding >= len(r.CodeReview.Findings) {
			continue
		}
		f := &r.CodeReview.Findings[d.Finding]
		if f.Reproduction == nil || f.Reproduction.Path != d.Reproduction.Path {
			continue
		}
		rep := d.Reproduction
		f.Reproduction, f.Disposition = &rep, d.Disposition
	}
}
