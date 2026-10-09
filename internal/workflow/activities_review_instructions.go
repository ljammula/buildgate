package workflow

import (
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// Every model review of a run reads the repository's instruction files as
// the base commit holds them (SC-019). RunReviewStepActivity prepares that
// before the review's argv is built (prepareReviewInstructions), launches
// with the snapshot's masks (withWorkspaceMasks) and removes the mountpoint
// stubs a mask over an absent path needed (removeReviewStubs).

const (
	// reviewStubDirName is the directory under the data directory that holds
	// one manifest per worktree, listing the stubs a review launch needs.
	// It is written before the first stub is created so a worker that dies
	// mid-review leaves a record that the next Activity attempt, or a build
	// resumed in a new run, sweeps: the manifest is keyed by the worktree,
	// not by the run, because a resuming run has another id and another
	// checkpoint directory.
	reviewStubDirName = "review-stubs"
	// ReviewInstructionsFailureMessage is the fixed message of a
	// ReviewInstructionsFailure. The cause is never in it: it reaches the
	// operator through the attempt's ReviewInstructionsError only.
	ReviewInstructionsFailureMessage = "the repository's instruction files could not be prepared for review; see this review attempt's record"
	// maxReviewRecordedPaths caps each path list an attempt records.
	maxReviewRecordedPaths = 64
	// maxReviewInstructionsErrorBytes caps the cleaned cause an attempt records.
	maxReviewInstructionsErrorBytes = 500
)

var reviewFullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reviewInstructionsFunc prepares the base-commit instruction files for a
// review of the commit checked out at workDir, as the commit instructionBase
// (any ref or commit id) holds them, under dst. Activities.snapshotReviewInstructions
// replaces it in tests that have no real base commit.
type reviewInstructionsFunc func(ctx context.Context, workDir, instructionBase, dst string) (sandbox.ReviewInstructionSnapshot, error)

// snapshotFn is the seam's value: nil means the real snapshot.
func (a *Activities) snapshotFn() reviewInstructionsFunc {
	if a.snapshotReviewInstructions != nil {
		return a.snapshotReviewInstructions
	}
	return snapshotReviewInstructionsOfWorktree
}

// snapshotReviewInstructionsOfWorktree resolves the full commit ids the
// snapshot requires (HEAD is the result; the instruction base may be a ref),
// requires the base to be an ancestor of HEAD and calls
// sandbox.SnapshotReviewInstructions.
func snapshotReviewInstructionsOfWorktree(ctx context.Context, workDir, diffBase, dst string) (sandbox.ReviewInstructionSnapshot, error) {
	resultSHA, err := runner.GitRevParseHEAD(workDir)
	if err != nil {
		return sandbox.ReviewInstructionSnapshot{}, err
	}
	baseSHA, err := runner.GitRevParseRef(workDir, diffBase+"^{commit}")
	if err != nil {
		return sandbox.ReviewInstructionSnapshot{}, err
	}
	if !reviewFullSHA.MatchString(resultSHA) || !reviewFullSHA.MatchString(baseSHA) {
		return sandbox.ReviewInstructionSnapshot{}, fmt.Errorf("the result %q or the base %q is not a full commit id", resultSHA, baseSHA)
	}
	ancestor, err := runner.GitIsAncestor(workDir, baseSHA, resultSHA)
	if err != nil {
		return sandbox.ReviewInstructionSnapshot{}, err
	}
	if !ancestor {
		return sandbox.ReviewInstructionSnapshot{}, fmt.Errorf("the instruction base %s is not an ancestor of the result %s", baseSHA, resultSHA)
	}
	return sandbox.SnapshotReviewInstructions(ctx, workDir, baseSHA, resultSHA, dst)
}

type workspaceMasksKey struct{}

// withWorkspaceMasks marks ctx so the launch it reaches binds masks read-only
// over the worktree, besides the `.factory/` mask runSandboxWithRetries adds
// to every launch that is not a review's. Only a review's launch is ever
// marked with masks.
func withWorkspaceMasks(ctx context.Context, masks []sandbox.WorkspaceMask) context.Context {
	if len(masks) == 0 {
		return ctx
	}
	return context.WithValue(ctx, workspaceMasksKey{}, append([]sandbox.WorkspaceMask(nil), masks...))
}

func workspaceMasksFrom(ctx context.Context) []sandbox.WorkspaceMask {
	masks, _ := ctx.Value(workspaceMasksKey{}).([]sandbox.WorkspaceMask)
	return append([]sandbox.WorkspaceMask(nil), masks...)
}

// reviewStub is one mountpoint a mask over an absent path needs.
type reviewStub struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir,omitempty"`
}

// reviewInstructions is what a review launch carries from its preparation.
type reviewInstructions struct {
	Snapshot sandbox.ReviewInstructionSnapshot
	Dst      string
	WorkDir  string
	// Manifest is the stub manifest of WorkDir (reviewStubManifestPath), ""
	// when the Activity has no data directory.
	Manifest string
}

// reviewStubManifestPath is where the stubs of a review of workDir are
// listed: <data dir>/review-stubs/<hex sha256 of the worktree's absolute
// path>.json, a host-only location keyed by the worktree. dataDir is the
// run's (input.DataDir, else the Worker's); "" gives "".
func reviewStubManifestPath(dataDir, workDir string) string {
	if dataDir == "" || workDir == "" {
		return ""
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = filepath.Clean(workDir)
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(dataDir, reviewStubDirName, hex.EncodeToString(sum[:])+".json")
}

// reviewInstructionsFailure is the error that halts the run before the
// review launches: a fixed message, the cleaned cause only in the attempt.
func reviewInstructionsFailure(kind string, cause error) error {
	return reviewInstructionsFailureOf(kind, cause, nil)
}

// reviewInstructionsFailureOf is reviewInstructionsFailure for a failure after
// a launch started: the cleaned cause goes on the last of attempts, or on a
// new attempt when there is none.
func reviewInstructionsFailureOf(kind string, cause error, attempts []run.Attempt) error {
	text := sanitize.Line(cause.Error())
	if len(text) > maxReviewInstructionsErrorBytes {
		text = strings.ToValidUTF8(text[:maxReviewInstructionsErrorBytes], "")
	}
	attempts = append([]run.Attempt(nil), attempts...)
	if len(attempts) == 0 {
		attempts = []run.Attempt{{Kind: kind, ExitCode: -1, Role: run.AttemptRoleReview}}
	}
	attempts[len(attempts)-1].ReviewInstructionsError = text
	return temporal.NewNonRetryableApplicationError(ReviewInstructionsFailureMessage, ReviewInstructionsFailureType, nil, attempts)
}

// reviewInstructionsCause is the error among a review launch's runErr and its
// cleanup's finishErr whose origin is the masks, the stubs or the cleanup
// (nil otherwise): a mask the launch rejected, or a cleanup that failed after
// a launch that did not fail otherwise. An unconfirmed container cleanup stays
// the infrastructure failure it is.
func reviewInstructionsCause(runErr, finishErr error) error {
	switch {
	case runErr != nil && errors.Is(runErr, sandbox.ErrWorkspaceMask) && !errors.Is(runErr, sandbox.ErrCleanupUnconfirmed):
		return runErr
	case runErr == nil && finishErr != nil:
		return finishErr
	}
	return nil
}

// prepareReviewInstructions requires a clean worktree, takes the snapshot
// and creates the stubs its masks need. An error is returned ready to hand
// back from the Activity. Stubs already created are removed by the caller
// through removeReviewStubs, which the manifest makes safe to call whenever.
func (a *Activities) prepareReviewInstructions(ctx context.Context, input ReviewStepInput, kind, dst string) (reviewInstructions, error) {
	prep := reviewInstructions{Dst: dst, WorkDir: input.WorkspacePath, Manifest: reviewStubManifestPath(a.dataDirFor(input.RunWorkflowInput), input.WorkspacePath)}
	clean, err := runner.GitIsClean(input.WorkspacePath)
	if err != nil {
		return prep, temporal.NewApplicationErrorWithCause("check the worktree before the review", InfrastructureFailureType, err)
	}
	if !clean {
		return prep, temporal.NewApplicationError("the worktree is not clean before the review; refusing to launch a reviewer on it", InfrastructureFailureType)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return prep, reviewInstructionsFailure(kind, err)
	}
	snap, err := a.snapshotFn()(ctx, input.WorkspacePath, input.instructionBase(), dst)
	if err != nil {
		return prep, reviewInstructionsFailure(kind, err)
	}
	prep.Snapshot = snap
	if err := createReviewStubs(input.WorkspacePath, prep.Manifest, snap.Masks); err != nil {
		return prep, reviewInstructionsFailure(kind, err)
	}
	return prep, nil
}

// launchContext is ctx plus what the review launch needs of the preparation:
// the mark that it is a review's launch (so it gets no `.factory/` mount), the
// masks, and the diff file staged beside the spec.
func (p reviewInstructions) launchContext(ctx context.Context) context.Context {
	ctx = withWorkspaceMasks(forReviewLaunch(ctx), p.Snapshot.Masks)
	if p.Snapshot.DiffPath != "" {
		ctx = withExtraRunInputs(ctx, p.Snapshot.DiffPath)
	}
	return ctx
}

// args adds the diff flag to a review script's argv when the build changed an
// instruction path.
func (p reviewInstructions) args(args []string) []string {
	if p.Snapshot.DiffPath == "" {
		return args
	}
	return append(args, "--instructions-diff", p.Snapshot.DiffPath)
}

// finish removes the stubs and everything under Dst but the diff file. A
// removal failure is an infrastructure failure of the Activity.
func (p reviewInstructions) finish(ctx context.Context) error {
	kept, err := removeReviewStubs(p.WorkDir, p.Manifest)
	if len(kept) > 0 {
		activity.GetLogger(ctx).Warn("review instruction stubs left in place because they are no longer empty", "paths", kept)
	}
	if err != nil {
		return err
	}
	if p.Dst == "" {
		return nil
	}
	return removeAllBut(p.Dst, p.Snapshot.DiffPath)
}

// record puts the preparation's evidence on a review attempt.
func (p reviewInstructions) record(attempt *run.Attempt) {
	attempt.ReviewInstructionsSHA256 = p.Snapshot.SHA256
	attempt.ReviewMaskedPaths = cappedCleanPaths(p.Snapshot.Paths)
	attempt.ReviewRemovedPaths = cappedCleanPaths(p.Snapshot.Removed)
}

// cappedCleanPaths makes each path one line and keeps maxReviewRecordedPaths
// of them, then one "... and N more" entry.
func cappedCleanPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(paths), maxReviewRecordedPaths+1))
	for i, p := range paths {
		if i == maxReviewRecordedPaths {
			out = append(out, fmt.Sprintf("... and %d more", len(paths)-maxReviewRecordedPaths))
			break
		}
		out = append(out, sanitize.Line(p))
	}
	return out
}

// removeAllBut deletes everything under dir except keep (a file directly in
// dir, or ""). Mode bits are widened first so a read-only snapshot can go.
func removeAllBut(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the review instruction snapshot: %w", err)
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if keep != "" && path == filepath.Clean(keep) {
			continue
		}
		_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove the review instruction snapshot: %w", err)
		}
	}
	return nil
}

// createReviewStubs writes the manifest, then creates a mountpoint for each
// mask whose path the result leaves absent. Parents must already be real
// directories (the snapshot guarantees it); if one is not, nothing is created.
func createReviewStubs(workDir, manifest string, masks []sandbox.WorkspaceMask) error {
	var stubs []reviewStub
	for _, m := range masks {
		if m.AbsentInWorktree {
			stubs = append(stubs, reviewStub{Path: m.Target, Dir: m.Dir})
		}
	}
	if len(stubs) == 0 {
		return nil
	}
	for _, s := range stubs {
		if !filepath.IsLocal(filepath.FromSlash(s.Path)) {
			return fmt.Errorf("mask target %q is not inside the workspace", s.Path)
		}
		if err := requireRealParents(workDir, s.Path); err != nil {
			return err
		}
	}
	if err := writeReviewStubManifest(manifest, stubs); err != nil {
		return err
	}
	for _, s := range stubs {
		target := filepath.Join(workDir, filepath.FromSlash(s.Path))
		if s.Dir {
			if err := os.Mkdir(target, 0o755); err != nil {
				return fmt.Errorf("create the mountpoint for %q: %w", s.Path, err)
			}
			continue
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("create the mountpoint for %q: %w", s.Path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("create the mountpoint for %q: %w", s.Path, err)
		}
	}
	return nil
}

// requireRealParents checks that every directory above rel, below workDir,
// exists and is a directory itself (not a link to one).
func requireRealParents(workDir, rel string) error {
	dir := workDir
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("the parent of mask target %q is missing or not a directory", rel)
		}
	}
	return nil
}

func writeReviewStubManifest(final string, stubs []reviewStub) error {
	if final == "" {
		return errors.New("write the review stub manifest: the Activity has no data directory")
	}
	data, err := json.Marshal(stubs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return fmt.Errorf("write the review stub manifest: %w", err)
	}
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write the review stub manifest: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("write the review stub manifest: %w", err)
	}
	return nil
}

// removeReviewStubs removes each worktree path the manifest lists that is
// still an empty regular file or an empty directory by Lstat and is not a
// path in HEAD's tree, then deletes the manifest. A path that is anything
// else is left alone and returned in kept. A missing manifest is not an
// error.
func removeReviewStubs(workDir, manifest string) (kept []string, err error) {
	if manifest == "" {
		return nil, nil
	}
	data, err := os.ReadFile(manifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the review stub manifest: %w", err)
	}
	var stubs []reviewStub
	if err := json.Unmarshal(data, &stubs); err != nil {
		return nil, fmt.Errorf("parse the review stub manifest: %w", err)
	}
	for _, s := range stubs {
		removed, err := removeReviewStub(workDir, s)
		if err != nil {
			return kept, err
		}
		if !removed {
			kept = append(kept, s.Path)
		}
	}
	if err := os.Remove(manifest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return kept, fmt.Errorf("remove the review stub manifest: %w", err)
	}
	return kept, nil
}

// removeReviewStub reports false when the path is there but is not an empty
// stub, or is a tracked path of HEAD. An absent path counts as removed.
func removeReviewStub(workDir string, s reviewStub) (bool, error) {
	if !filepath.IsLocal(filepath.FromSlash(s.Path)) {
		return false, nil
	}
	target := filepath.Join(workDir, filepath.FromSlash(s.Path))
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect the review stub %q: %w", s.Path, err)
	}
	empty := (info.Mode().IsRegular() && info.Size() == 0) || info.IsDir()
	if !empty {
		return false, nil
	}
	tracked, err := pathInHead(workDir, s.Path)
	if err != nil {
		return false, err
	}
	if tracked {
		return false, nil
	}
	// The parents were real when the stub was created; a review's worker had
	// the worktree since, so check again at the moment of removal.
	if err := requireRealParents(workDir, s.Path); err != nil {
		return false, err
	}
	if err := os.Remove(target); err != nil {
		if info.IsDir() {
			// A directory that is no longer empty is not a stub.
			if entries, readErr := os.ReadDir(target); readErr == nil && len(entries) > 0 {
				return false, nil
			}
		}
		return false, fmt.Errorf("remove the review stub %q: %w", s.Path, err)
	}
	return true, nil
}

// pathInHead reports whether HEAD's tree holds rel (a file, link or directory).
func pathInHead(workDir, rel string) (bool, error) {
	out, err := exec.Command("git", "-C", workDir, "ls-tree", "-z", "--name-only", "HEAD", "--", rel).Output()
	if err != nil {
		return false, fmt.Errorf("list HEAD's tree for the review stub %q: %w", rel, err)
	}
	return len(out) > 0, nil
}

// sweepReviewStubs is removeReviewStubs for the top of an Activity that may
// follow a worker that died mid-review. An error is an infrastructure failure.
func (a *Activities) sweepReviewStubs(ctx context.Context, input RunWorkflowInput) error {
	kept, err := removeReviewStubs(input.WorkspacePath, reviewStubManifestPath(a.dataDirFor(input), input.WorkspacePath))
	if len(kept) > 0 {
		activity.GetLogger(ctx).Warn("a review instruction stub from an earlier attempt is not empty and was left in place", "paths", kept)
	}
	if err != nil {
		return temporal.NewApplicationErrorWithCause("sweep the review instruction stubs an earlier attempt left", InfrastructureFailureType, err)
	}
	return nil
}
