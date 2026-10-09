package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
)

// Root AGENTS.md carries a fenced memory section that only a memory request's
// run may change, and then only to the text the factory rendered when it
// proposed the change. The host reads the file from git objects at the run's
// diff base and result commit, records what it found as run.MemoryEdit, and
// the release policy (release.MergePolicyCheck) denies from that record. The
// worktree is never read: a build can write anything there.

const (
	agentsFile         = "AGENTS.md"
	maxAgentsBlobBytes = 1 << 20
	memoryGitTimeout   = 30 * time.Second
	// maxAgentsVariants bounds the root case variants read; more is treated
	// as a section change outright.
	maxAgentsVariants = 8
)

var (
	// errNotRegularBlob: the path is a symlink or a submodule, not a file.
	errNotRegularBlob = errors.New("not a regular file")
	// errBlobTooLarge: the file is over maxAgentsBlobBytes.
	errBlobTooLarge  = errors.New("file is too large to check")
	commitHexPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// blobAtCommit is hostBoundary.blobAtCommit's real body: path's bytes as of
// commit in repoDir, read from git objects (ls-tree for the mode, cat-file for
// the content). A missing path is (nil, false, nil); a symlink or submodule is
// errNotRegularBlob and a file over maxAgentsBlobBytes is errBlobTooLarge.
func (impl realHost) blobAtCommit(ctx context.Context, repoDir, commit, path string) ([]byte, bool, error) {
	if !commitHexPattern.MatchString(commit) {
		return nil, false, fmt.Errorf("%q is not a full commit id", commit)
	}
	out, err := gitObjectOutput(ctx, repoDir, "--literal-pathspecs", "ls-tree", "-z", "-l", commit, "--", path)
	if err != nil {
		return nil, false, err
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	meta, _, _ := strings.Cut(string(out), "\t")
	fields := strings.Fields(meta) // mode type object size
	if len(fields) != 4 {
		return nil, false, fmt.Errorf("unexpected ls-tree output %q", meta)
	}
	if fields[0] != "100644" && fields[0] != "100755" {
		return nil, true, errNotRegularBlob
	}
	if size, convErr := strconv.Atoi(fields[3]); convErr != nil || size > maxAgentsBlobBytes {
		return nil, true, errBlobTooLarge
	}
	blob, err := gitObjectOutput(ctx, repoDir, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, true, err
	}
	return blob, true, nil
}

func gitObjectOutput(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("git %s: %w: %s", args[len(args)-1], err, msg)
	}
	return out, nil
}

// agentsBlob is one root AGENTS.md (or case variant) at one commit.
type agentsBlob struct {
	data      []byte
	exists    bool
	irregular bool // a symlink, submodule or oversize file: changed and unreadable
}

func readAgentsBlob(ctx context.Context, dp *deps, repoDir, commit, path string) (agentsBlob, error) {
	data, exists, err := dp.host.blobAtCommit(ctx, repoDir, commit, path)
	switch {
	case errors.Is(err, errNotRegularBlob), errors.Is(err, errBlobTooLarge):
		return agentsBlob{exists: true, irregular: true}, nil
	case err != nil:
		return agentsBlob{}, err
	}
	return agentsBlob{data: data, exists: exists}, nil
}

// fencedSection is a file split around its fenced block (markers included).
type fencedSection struct {
	block, before, after string
	present              bool
}

// parseFenced splits data around its fenced block; an error means a damaged
// fence.
func parseFenced(data []byte) (fencedSection, error) {
	s, err := memory.ParseSection(data)
	if err != nil || !s.Present {
		return fencedSection{}, err
	}
	text := string(data)
	return fencedSection{block: text[len(s.Before) : len(text)-len(s.After)], before: s.Before, after: s.After, present: true}, nil
}

// sectionDiffers reports whether the fenced block differs between base and
// result: other bytes, added, removed, or moved with the rest of the file
// unchanged. An unreadable side, or a fence that does not parse, counts as a
// change unless the two files are byte-identical.
func sectionDiffers(base, result agentsBlob) bool {
	if base.irregular || result.irregular {
		return true
	}
	if base.exists == result.exists && bytes.Equal(base.data, result.data) {
		return false
	}
	b, bErr := parseFenced(base.data)
	r, rErr := parseFenced(result.data)
	switch {
	case bErr != nil || rErr != nil:
		return true
	case b.present != r.present || b.block != r.block:
		return true
	}
	// The same block at another place: only the text around it was rearranged.
	return b.present && b.before != r.before && b.before+b.after == r.before+r.after
}

func hasMemoryMarker(b agentsBlob) bool {
	return b.irregular || bytes.Contains(b.data, []byte(memory.BeginMarker)) || bytes.Contains(b.data, []byte(memory.EndMarker))
}

// rootAgentsNames splits a changed-file inventory into whether root AGENTS.md
// itself is in it, the root names that fold to it (agents.md, Agents.MD: a
// case-insensitive worktree makes them the same file), and every other path.
func rootAgentsNames(changed []string) (exact bool, variants, others []string) {
	for _, c := range changed {
		switch {
		case c == agentsFile:
			exact = true
		case !strings.Contains(c, "/") && strings.EqualFold(c, agentsFile):
			variants = append(variants, c)
			others = append(others, c)
		default:
			others = append(others, c)
		}
	}
	return exact, variants, others
}

// memoryProposal loads the proposal of r's request. has is true when a file
// exists; err means a request id or project that is not a safe path
// component, or a file that is present but unusable.
func memoryProposal(r *run.Run, dataDir string) (p memory.Proposal, has bool, err error) {
	if r.RequestID == "" {
		return memory.Proposal{}, false, nil
	}
	path, err := memory.ProposalPath(dataDir, release.ProjectOf(r), r.RequestID)
	if err != nil {
		return memory.Proposal{}, false, err
	}
	return memory.LoadProposal(path)
}

// computeMemoryEdit is the evidence for an accepted run with a result commit,
// or nil when the run neither changed a root AGENTS.md nor has a proposal (no
// git call is made then). repoDir is a checkout whose object store holds both
// commits. Computed before the release decision.
func computeMemoryEdit(ctx context.Context, dp *deps, r *run.Run, dataDir, repoDir string) *run.MemoryEdit {
	if r.State != run.StateAccepted || r.ResultSHA == "" || r.ChangedFiles == nil {
		return nil
	}
	exact, variants, others := rootAgentsNames(r.ChangedFiles)
	proposal, has, propErr := memoryProposal(r, dataDir)
	if !exact && len(variants) == 0 && !has && propErr == nil {
		return nil
	}
	edit := &run.MemoryEdit{Proposal: has, OtherFilesChanged: others}
	defer edit.Clean()
	failClosed := has || propErr != nil || exact || len(variants) > 0
	fail := func(err error) *run.MemoryEdit {
		edit.Error, edit.FailClosed = err.Error(), failClosed
		return edit
	}
	if propErr != nil {
		return fail(fmt.Errorf("proposal: %w", propErr))
	}
	base := r.DiffBaseSHA
	if base == "" {
		base = r.BaseSHA
	}
	if base == "" {
		return fail(errors.New("run has no base commit"))
	}
	if err := fillMemoryEdit(ctx, dp, edit, r, repoDir, base, exact, variants); err != nil {
		return fail(err)
	}
	edit.Matches = has && edit.ResultSHA256 != "" && edit.ResultSHA256 == proposal.ExpectedSHA256
	return edit
}

// fillMemoryEdit reads root AGENTS.md and its variants and sets
// SectionChanged and the two hashes.
func fillMemoryEdit(ctx context.Context, dp *deps, edit *run.MemoryEdit, r *run.Run, repoDir, base string, exact bool, variants []string) error {
	result, err := readAgentsBlob(ctx, dp, repoDir, r.ResultSHA, agentsFile)
	if err != nil {
		return fmt.Errorf("read %s at result: %w", agentsFile, err)
	}
	edit.ResultSHA256 = hashOfBlob(result)
	edit.BaseSHA256 = edit.ResultSHA256 // git lists the file as unchanged
	if exact {
		before, err := readAgentsBlob(ctx, dp, repoDir, base, agentsFile)
		if err != nil {
			return fmt.Errorf("read %s at base: %w", agentsFile, err)
		}
		edit.BaseSHA256 = hashOfBlob(before)
		edit.SectionChanged = sectionDiffers(before, result)
	}
	if len(variants) > maxAgentsVariants {
		edit.SectionChanged = true
		return nil
	}
	for _, v := range variants {
		changed, err := variantCarriesMarker(ctx, dp, repoDir, base, r.ResultSHA, v)
		if err != nil {
			return err
		}
		edit.SectionChanged = edit.SectionChanged || changed
	}
	return nil
}

func hashOfBlob(b agentsBlob) string {
	if !b.exists || b.irregular {
		return ""
	}
	return memory.HashHex(b.data)
}

// variantCarriesMarker: a root name folding to AGENTS.md was changed and
// holds a memory marker (or cannot be read as text) at either commit.
func variantCarriesMarker(ctx context.Context, dp *deps, repoDir, base, result, name string) (bool, error) {
	for _, commit := range []string{result, base} {
		b, err := readAgentsBlob(ctx, dp, repoDir, commit, name)
		if err != nil {
			return false, fmt.Errorf("read %s at %.12s: %w", name, commit, err)
		}
		if hasMemoryMarker(b) {
			return true, nil
		}
	}
	return false, nil
}

// recordMemoryEdit sets r.MemoryEdit for an accepted run, before its release
// decision is evaluated. r is the caller's record: it is saved by the caller.
func recordMemoryEdit(dp *deps, r *run.Run, dataDir, repoDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), memoryGitTimeout)
	defer cancel()
	r.MemoryEdit = computeMemoryEdit(ctx, dp, r, dataDir, repoDir)
	if e := r.MemoryEdit; e != nil && e.Error != "" {
		log.Printf("run %s: memory section check: %s (fail closed: %v)", r.ID, e.Error, e.FailClosed)
	}
}

// ensureMemoryEdit computes the evidence for a run recorded before the field
// existed (MemoryEdit nil) on a pull-request open retry, before the release
// decision is evaluated again, and persists it when there is any. loaded is
// the caller's copy; the run's own checkout holds the commits.
func ensureMemoryEdit(dp *deps, dataDir, runID string, loaded *run.Run) {
	if loaded.MemoryEdit != nil {
		return
	}
	recordMemoryEdit(dp, loaded, dataDir, loaded.WorkspacePath)
	if loaded.MemoryEdit == nil {
		return
	}
	edit := loaded.MemoryEdit
	if err := run.WithLock(dataDir, runID, func() error {
		fresh, err := run.Load(dataDir, runID)
		if err != nil {
			return err
		}
		if fresh.MemoryEdit == nil {
			fresh.MemoryEdit = edit
			return fresh.Persist(dataDir)
		}
		return nil
	}); err != nil {
		log.Printf("run %s: record memory section check: %v", runID, err)
	}
}
