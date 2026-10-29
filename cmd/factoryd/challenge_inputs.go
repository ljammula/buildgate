package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"buildgate/internal/evidence"
	"buildgate/internal/harness"
	"buildgate/internal/modelrole"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// challengeSpecSnapshotName is -challenge-spec's snapshot in the run's
// directory.
const challengeSpecSnapshotName = "challenge-spec.snapshot.md"

// Caps on the files the challenge flags name.
const (
	maxChallengeSpecBytes     = 1 << 20
	maxChallengeManifestBytes = 1 << 20
	maxHeldChallengeFiles     = 5
)

var challengeSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// snapshotChallengeInputs snapshots -challenge-spec into the run's directory
// and records its hash (the Activity checks it again before the challenger is
// given the file), and checks -challenge-held (loadChallengeHeld), before
// anything is built. -challenge and -challenge-round need their inputs.
func (tr *ticketRun) snapshotChallengeInputs() error {
	if *tr.rf.challengeRound && *tr.rf.challengeHeld == "" {
		return errors.New("-challenge-round needs -challenge-held")
	}
	if *tr.rf.challenge && *tr.rf.challengeSpec == "" {
		return errors.New("-challenge needs -challenge-spec")
	}
	if *tr.rf.challengeSpec != "" {
		spec, err := evidence.ReadHostileFile(*tr.rf.challengeSpec, maxChallengeSpecBytes)
		if err != nil {
			return fmt.Errorf("-challenge-spec: %w", err)
		}
		snapshot, err := filepath.Abs(filepath.Join(run.Dir(*tr.dataDir, tr.id), challengeSpecSnapshotName))
		if err != nil {
			return fmt.Errorf("resolve the challenge spec's snapshot path: %w", err)
		}
		if err := os.WriteFile(snapshot, spec, 0o600); err != nil {
			return fmt.Errorf("snapshot the challenge spec: %w", err)
		}
		sum := sha256.Sum256(spec)
		tr.challengeSpecSnapshot, tr.challengeSpecSHA256 = snapshot, hex.EncodeToString(sum[:])
	}
	if *tr.rf.challengeHeld == "" {
		return nil
	}
	held, err := loadChallengeHeld(*tr.dataDir, *tr.rf.challengeHeld)
	if err != nil {
		return fmt.Errorf("-challenge-held: %w", err)
	}
	tr.challengeHeld = held
	return nil
}

// loadChallengeHeld reads the JSON list of held challenge files at path and
// checks each: a clean relative path, a SHA-256, and a host path that is under
// some run's challenge directory in dataDir. At most maxHeldChallengeFiles.
// The copies' content is checked against the hash by the workflow before each
// mount.
func loadChallengeHeld(dataDir, path string) ([]run.ChallengeFile, error) {
	data, err := evidence.ReadHostileFile(path, maxChallengeManifestBytes)
	if err != nil {
		return nil, err
	}
	var held []run.ChallengeFile
	if err := json.Unmarshal(data, &held); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(held) == 0 || len(held) > maxHeldChallengeFiles {
		return nil, fmt.Errorf("%d files, want 1 to %d", len(held), maxHeldChallengeFiles)
	}
	runs, err := filepath.Abs(filepath.Join(dataDir, "runs"))
	if err != nil {
		return nil, err
	}
	for _, f := range held {
		if f.Path == "" || filepath.IsAbs(f.Path) || !cleanHeldPath(f.Path) || !challengeSHA256.MatchString(f.SHA256) {
			return nil, fmt.Errorf("file %q is not a clean relative path with a SHA-256", f.Path)
		}
		rel, err := filepath.Rel(runs, f.HostPath)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if err != nil || !filepath.IsAbs(f.HostPath) || len(parts) < 4 || parts[0] == ".." || parts[1] != run.ChallengeDirName {
			return nil, fmt.Errorf("file %q: its host copy is not under a run's %s directory", f.Path, run.ChallengeDirName)
		}
	}
	return held, nil
}

// resolveChallenger resolves roles.challenger's route, thinking and harness
// for a run that launches the challenger job; nothing when the role is unset
// (the job then records "no challenger role configured").
//
// A route that cannot be resolved (an expired credential, a missing model id)
// is a challenger with no verdict, never a build that does not start
// (SC-022): the failure is logged and the job runs as if no role were set.
func (tr *ticketRun) resolveChallenger() error {
	if err := tr.resolveChallengerRoute(); err != nil {
		log.Printf("challenger: no challenge for this build: %v", err)
		tr.challengerSelection, tr.challengerThinking, tr.challengerHarnessName = nil, "", ""
	}
	return nil
}

func (tr *ticketRun) resolveChallengerRoute() error {
	if !*tr.rf.challenge || tr.settings.Roles == nil || tr.settings.Roles.Challenger == nil {
		return nil
	}
	probe := func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	}
	sel, err := modelrole.SelectRoute(tr.settings, modelrole.RoleChallenger, "", "", "", probe)
	if err != nil {
		return fmt.Errorf("roles.challenger: %w", err)
	}
	applyFinalRelayCeilings(tr.settings, nil, &sel)
	tr.challengerSelection = &sel
	logRouteSkips("challenger", tr.challengerSelection)
	tr.challengerThinking = sel.Thinking
	if tr.challengerHarnessName, err = modelrole.RoleHarness(tr.settings, modelrole.RoleChallenger, ""); err != nil {
		return fmt.Errorf("roles.challenger: %w", err)
	}
	d, err := harness.Lookup(tr.challengerHarnessName)
	if err != nil {
		return fmt.Errorf("roles.challenger: %w", err)
	}
	if d.RequiresWorkerModel && sel.Policy.WorkerModelID == "" {
		return fmt.Errorf("roles.challenger: harness %s requires a roles.challenger model with a worker model id; set the entitled model id in session config", tr.challengerHarnessName)
	}
	if tr.relayPolicy != nil {
		if _, err := resolveRouteCredentials(sel.Route); err != nil {
			return fmt.Errorf("roles.challenger: %w", err)
		}
	}
	return nil
}

// cleanHeldPath reports whether p is a relative path inside the worktree with
// no `..` component; a name that merely contains two dots (parse..edge_test.go)
// is clean.
func cleanHeldPath(p string) bool {
	return filepath.IsLocal(filepath.FromSlash(p)) && !slices.Contains(strings.Split(p, "/"), "..")
}
