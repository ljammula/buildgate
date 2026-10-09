package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
)

// A root instruction name is a root-level path that folds to AGENTS.md
// (run.RootInstructionName). Two rules hold at release, and the host records
// what it read from git objects as run.MemoryEdit for release.MergePolicyCheck
// to deny from. The worktree is never read: a build can write anything there.
//
//   - A memory run (its request has a proposal file) is released only when the
//     result tree holds exactly one root instruction name, spelled AGENTS.md, a
//     regular file of mode 100644 whose bytes are the proposal's expected
//     text, and no other file changed.
//   - Any other run: when a root instruction file at its diff base holds
//     "buildgate:memory", it may not change a root instruction name at all;
//     when none does, it may change or create one, but the result may hold only
//     one such name, none of them may hold the token (plainly or as HTML
//     character references), and none it changed may be a symlink, a submodule
//     or a directory.
//
// No git call is made for a run with no proposal whose changed files name no
// root instruction name. Any failure past that point denies the release.

const (
	agentsFile         = run.RootInstructionFile
	maxAgentsBlobBytes = 1 << 20
	memoryGitTimeout   = 30 * time.Second
	// maxAgentsVariants bounds the root instruction names read at one
	// commit; more is an error, which denies.
	maxAgentsVariants = 8
	// memoryMarkerToken is in both fence markers. A root instruction file
	// holding it, in any letter case, has (or imitates) a memory section.
	memoryMarkerToken = "buildgate:memory"
	gitModeFile       = "100644"
	gitModeExecutable = "100755"
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

// rootTreeAtCommit is hostBoundary.rootTreeAtCommit's real body: the entries
// of commit's root tree, from git objects.
func (impl realHost) rootTreeAtCommit(ctx context.Context, repoDir, commit string) ([]gitTreeEntry, error) {
	if !commitHexPattern.MatchString(commit) {
		return nil, fmt.Errorf("%q is not a full commit id", commit)
	}
	out, err := gitObjectOutput(ctx, repoDir, "ls-tree", "-z", commit)
	if err != nil {
		return nil, err
	}
	var entries []gitTreeEntry
	for _, record := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta) // mode type object
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree output %q", meta)
		}
		entries = append(entries, gitTreeEntry{name: name, mode: fields[0], kind: fields[1], object: fields[2]})
	}
	return entries, nil
}

// gitTreeEntry is one entry of a commit's root tree.
type gitTreeEntry struct {
	name   string
	mode   string // 100644, 100755, 120000 (symlink), 160000 (submodule), 040000 (directory)
	kind   string // blob, commit, tree
	object string
}

func (e gitTreeEntry) regularFile() bool {
	return e.kind == "blob" && (e.mode == gitModeFile || e.mode == gitModeExecutable)
}

// rootInstructionEntries is the root instruction names of commit.
func rootInstructionEntries(ctx context.Context, dp *deps, repoDir, commit string) ([]gitTreeEntry, error) {
	all, err := dp.host.rootTreeAtCommit(ctx, repoDir, commit)
	if err != nil {
		return nil, fmt.Errorf("list the root of %.12s: %w", commit, err)
	}
	var names []gitTreeEntry
	for _, e := range all {
		if run.RootInstructionName(e.name) != "" {
			names = append(names, e)
		}
	}
	if len(names) > maxAgentsVariants {
		return nil, fmt.Errorf("%.12s spells %s more than %d ways at its root", commit, agentsFile, maxAgentsVariants)
	}
	return names, nil
}

// holdsMemoryMarker reports whether data holds memoryMarkerToken in any
// letter case, as written or after HTML character references are decoded
// once. NUL and format characters (a BOM, zero-width and direction marks) are
// dropped first, so a token split by one, or a UTF-16 file, still counts.
func holdsMemoryMarker(data []byte) bool {
	text := strings.Map(func(r rune) rune {
		if r == 0 || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, string(data))
	if strings.Contains(strings.ToLower(text), memoryMarkerToken) {
		return true
	}
	return strings.Contains(strings.ToLower(html.UnescapeString(text)), memoryMarkerToken)
}

// instructionFile is what one root instruction name holds at one commit.
type instructionFile struct {
	entry  gitTreeEntry
	marker bool   // a regular file holding the marker token
	sha256 string // of a regular file's bytes
}

// readInstructionFiles reads every regular file among entries. A file too
// large to check is an error.
func readInstructionFiles(ctx context.Context, dp *deps, repoDir, commit string, entries []gitTreeEntry) ([]instructionFile, error) {
	files := make([]instructionFile, 0, len(entries))
	for _, e := range entries {
		f := instructionFile{entry: e}
		if e.regularFile() {
			data, exists, err := dp.host.blobAtCommit(ctx, repoDir, commit, e.name)
			if err != nil || !exists {
				return nil, fmt.Errorf("read %q at %.12s: %w", e.name, commit, errors.Join(err, missingIf(!exists)))
			}
			f.marker, f.sha256 = holdsMemoryMarker(data), memory.HashHex(data)
		}
		files = append(files, f)
	}
	return files, nil
}

func missingIf(missing bool) error {
	if missing {
		return errors.New("listed in the tree but not found")
	}
	return nil
}

// changedInstructionNames is the names whose entry differs between the two
// commits, or that only one of them has.
func changedInstructionNames(base, result []gitTreeEntry) []string {
	before := map[string]gitTreeEntry{}
	for _, e := range base {
		before[e.name] = e
	}
	var changed []string
	for _, e := range result {
		if old, had := before[e.name]; !had || old != e {
			changed = append(changed, e.name)
		}
		delete(before, e.name)
	}
	for _, e := range base {
		if _, gone := before[e.name]; gone {
			changed = append(changed, e.name)
		}
	}
	return changed
}

func mergeNames(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// otherThanAgentsFile is every changed path except root AGENTS.md itself.
func otherThanAgentsFile(changed []string) []string {
	var others []string
	for _, c := range changed {
		if c != agentsFile {
			others = append(others, c)
		}
	}
	return others
}

// memoryProposal loads the proposal of r's request. has is true when a file
// exists; err means a request id or project that is not a safe path
// component, or a file that is present but unusable (not a regular file,
// recorded for another request, damaged).
func memoryProposal(r *run.Run, dataDir string) (p memory.Proposal, has bool, err error) {
	if r.RequestID == "" {
		return memory.Proposal{}, false, nil
	}
	return memory.LoadProposalFor(dataDir, release.ProjectOf(r), r.RequestID)
}

// computeMemoryEdit is the evidence for an accepted run with a result commit,
// or nil when the run neither changed a root instruction name nor has a
// proposal (no git call is made then). repoDir is a checkout whose object
// store holds both commits. Computed before the release decision.
func computeMemoryEdit(ctx context.Context, dp *deps, r *run.Run, dataDir, repoDir string) *run.MemoryEdit {
	if r.State != run.StateAccepted || r.ResultSHA == "" || r.ChangedFiles == nil {
		return nil
	}
	listed := run.ChangedRootInstructionNames(r.ChangedFiles)
	proposal, has, propErr := memoryProposal(r, dataDir)
	if len(listed) == 0 && !has && propErr == nil {
		return nil
	}
	edit := &run.MemoryEdit{Proposal: has, ChangedRootNames: listed, OtherFilesChanged: otherThanAgentsFile(r.ChangedFiles)}
	defer edit.Clean()
	if propErr != nil {
		edit.Error = fmt.Sprintf("proposal: %v", propErr)
		return edit
	}
	base := r.DiffBaseSHA
	if base == "" {
		base = r.BaseSHA
	}
	if base == "" {
		edit.Error = "run has no base commit"
		return edit
	}
	if err := fillMemoryEdit(ctx, dp, edit, repoDir, base, r.ResultSHA, proposal.ExpectedSHA256); err != nil {
		edit.Error = err.Error()
	}
	return edit
}

// fillMemoryEdit reads the root instruction names at both commits and sets
// everything the policy decides on.
func fillMemoryEdit(ctx context.Context, dp *deps, edit *run.MemoryEdit, repoDir, base, result, expectedSHA256 string) error {
	baseEntries, err := rootInstructionEntries(ctx, dp, repoDir, base)
	if err != nil {
		return err
	}
	resultEntries, err := rootInstructionEntries(ctx, dp, repoDir, result)
	if err != nil {
		return err
	}
	treeChanged := changedInstructionNames(baseEntries, resultEntries)
	edit.ChangedRootNames = mergeNames(edit.ChangedRootNames, treeChanged)
	baseFiles, err := readInstructionFiles(ctx, dp, repoDir, base, baseEntries)
	if err != nil {
		return err
	}
	for _, f := range baseFiles {
		edit.BaseHasSection = edit.BaseHasSection || f.marker
		if f.entry.name == agentsFile {
			edit.BaseSHA256 = f.sha256
		}
	}
	resultFiles, err := readInstructionFiles(ctx, dp, repoDir, result, resultEntries)
	if err != nil {
		return err
	}
	changed := map[string]bool{}
	for _, name := range treeChanged {
		changed[name] = true
	}
	for _, f := range resultFiles {
		edit.ResultRootNames = append(edit.ResultRootNames, f.entry.name)
		if f.entry.name == agentsFile {
			edit.ResultSHA256 = f.sha256
		}
		if f.marker {
			edit.MarkerIn = append(edit.MarkerIn, f.entry.name)
		}
		if !f.entry.regularFile() && changed[f.entry.name] {
			edit.NotRegularFile = append(edit.NotRegularFile, f.entry.name)
		}
	}
	edit.Matches = edit.Proposal && len(resultFiles) == 1 && matchesApprovedFile(resultFiles[0], expectedSHA256)
	return nil
}

// matchesApprovedFile: f is root AGENTS.md by that spelling, a plain file of
// mode 100644, with the approved bytes.
func matchesApprovedFile(f instructionFile, expectedSHA256 string) bool {
	e := f.entry
	return e.name == agentsFile && e.kind == "blob" && e.mode == gitModeFile && f.sha256 != "" && f.sha256 == expectedSHA256
}

// recordMemoryEdit sets r.MemoryEdit for an accepted run, before its release
// decision is evaluated. r is the caller's record: it is saved by the caller.
func recordMemoryEdit(dp *deps, r *run.Run, dataDir, repoDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), memoryGitTimeout)
	defer cancel()
	r.MemoryEdit = computeMemoryEdit(ctx, dp, r, dataDir, repoDir)
	if e := r.MemoryEdit; e != nil && e.Error != "" {
		log.Printf("run %s: memory section check: %s (the release is denied)", r.ID, e.Error)
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
