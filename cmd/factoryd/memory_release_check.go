package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
)

// A root instruction name is a root-level path that folds to AGENTS.md
// (run.RootInstructionName). Two rules hold at release, and the host records
// what it read from git objects as run.MemoryEdit for release.MergePolicyCheck
// to deny from. The worktree is never read: a build can write anything there.
//
//   - A memory run (its request is a memory request, with a proposal file under
//     its repository's store key) is released only when the
//     result tree holds exactly one root instruction name, spelled AGENTS.md, a
//     regular file of mode 100644 whose bytes are the proposal's expected
//     text, and no other file changed; and only while root AGENTS.md at its
//     diff base is still the file the proposal was rendered from and the
//     repository's store holds no off marker.
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
	// The one hardening of every host read of a trusted commit.
	cmd := sandbox.HardenedGitCommand(ctx, repoDir, args...)
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

// memoryProposal loads the proposal of r's request when that request is a
// memory request (request.SourceMemory, which only `factoryd memory propose`
// submits); has is false for every other run, with no git call. key is the
// store key of the request's repository, which the proposal is kept under.
// err means a request record that exists and cannot be read, or a memory
// request whose proposal is absent or unusable (not a regular file, recorded
// for another request, damaged): such a run is never released.
func memoryProposal(r *run.Run, dataDir string) (p memory.Proposal, key string, has bool, err error) {
	if r.RequestID == "" {
		return memory.Proposal{}, "", false, nil
	}
	req, err := request.Load(dataDir, r.RequestID)
	if errors.Is(err, os.ErrNotExist) {
		return memory.Proposal{}, "", false, nil
	}
	if err != nil {
		return memory.Proposal{}, "", false, fmt.Errorf("request %s: %w", r.RequestID, err)
	}
	return memoryProposalOfRequest(dataDir, req)
}

// memoryProposalOfRequest is memoryProposal for a loaded request.
func memoryProposalOfRequest(dataDir string, req *request.Request) (p memory.Proposal, key string, has bool, err error) {
	if req.Source.Kind != request.SourceMemory {
		return memory.Proposal{}, "", false, nil
	}
	if req.Workspace == "" {
		return memory.Proposal{}, "", false, fmt.Errorf("memory request %s records no repository", req.ID)
	}
	key = memory.StoreKey(req.Project, release.RepositoryRoot(req.Workspace))
	p, has, err = memory.LoadProposalFor(dataDir, key, req.ID)
	if err == nil && !has {
		err = fmt.Errorf("memory request %s has no proposal file", req.ID)
	}
	return p, key, has, err
}

// memorySwitchedOff reports whether the store under key holds the off marker
// `factoryd memory off` writes. It creates nothing.
func memorySwitchedOff(dataDir, key string) (bool, error) {
	store, err := memory.OpenReadOnly(dataDir, key)
	if err != nil {
		return false, err
	}
	return store.Off()
}

// reasonMemoryDispatchUnchecked halts a memory request whose proposal or
// whose AGENTS.md at HEAD could not be read before its build.
const reasonMemoryDispatchUnchecked = "the memory change could not be checked against AGENTS.md before its build"

// staleMemoryRequest is why the worker does not build req's ticket, "" when
// it may: req is a memory request and root AGENTS.md at its checkout's HEAD
// is no longer the file its proposal was rendered from, so the release check
// would refuse the run (release.ReasonMemoryBaseMoved). A proposal or a HEAD
// that cannot be read refuses too. Every other request gets "", with no git
// call.
func staleMemoryRequest(ctx context.Context, dp *deps, dataDir string, req *request.Request) string {
	proposal, _, has, err := memoryProposalOfRequest(dataDir, req)
	if err == nil && !has {
		return ""
	}
	if err != nil {
		log.Printf("request %s: memory proposal: %v", req.ID, err)
		return reasonMemoryDispatchUnchecked
	}
	hash, err := agentsHashAtHead(ctx, dp, release.RepositoryRoot(req.Workspace))
	if err != nil {
		log.Printf("request %s: %v", req.ID, err)
		return reasonMemoryDispatchUnchecked
	}
	if hash != proposal.BaseBlobSHA256 {
		return release.ReasonMemoryBaseMoved
	}
	return ""
}

// agentsHashAtHead is the SHA-256 of root AGENTS.md at the checkout's HEAD,
// read from git objects; "" when HEAD has no such file.
func agentsHashAtHead(ctx context.Context, dp *deps, repoRoot string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, memoryGitTimeout)
	defer cancel()
	head, err := dp.host.headCommit(ctx, repoRoot)
	if err != nil {
		return "", fmt.Errorf("read HEAD of %s: %w", repoRoot, err)
	}
	data, exists, err := dp.host.blobAtCommit(ctx, repoRoot, head, agentsFile)
	if err != nil {
		return "", fmt.Errorf("read %s at HEAD: %w", agentsFile, err)
	}
	if !exists {
		return "", nil
	}
	return memory.HashHex(data), nil
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
	proposal, key, has, propErr := memoryProposal(r, dataDir)
	if len(listed) == 0 && !has && propErr == nil {
		return nil
	}
	edit := &run.MemoryEdit{Proposal: has, ChangedRootNames: listed, OtherFilesChanged: otherThanAgentsFile(r.ChangedFiles)}
	defer edit.Clean()
	if propErr != nil {
		edit.Error = fmt.Sprintf("proposal: %v", propErr)
		return edit
	}
	if has {
		off, err := memorySwitchedOff(dataDir, key)
		if err != nil {
			edit.Error = fmt.Sprintf("off marker: %v", err)
			return edit
		}
		edit.SwitchedOff = off
	}
	base := r.DiffBaseSHA
	if base == "" {
		base = r.BaseSHA
	}
	if base == "" {
		edit.Error = "run has no base commit"
		return edit
	}
	if err := fillMemoryEdit(ctx, dp, edit, repoDir, base, r.ResultSHA, proposal); err != nil {
		edit.Error = err.Error()
	}
	return edit
}

// fillMemoryEdit reads the root instruction names at both commits and sets
// everything the policy decides on.
func fillMemoryEdit(ctx context.Context, dp *deps, edit *run.MemoryEdit, repoDir, base, result string, proposal memory.Proposal) error {
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
	baseHasFile := false
	for _, f := range baseFiles {
		edit.BaseHasSection = edit.BaseHasSection || f.marker
		if f.entry.name == agentsFile {
			edit.BaseSHA256, baseHasFile = f.sha256, true
		}
	}
	// A base entry that is not a regular file has no hash: it is not the
	// "no file" a proposal with an empty base hash was rendered from.
	edit.BaseMoved = edit.Proposal && (edit.BaseSHA256 != proposal.BaseBlobSHA256 || baseHasFile != (proposal.BaseBlobSHA256 != ""))
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
	edit.Matches = edit.Proposal && len(resultFiles) == 1 && matchesApprovedFile(resultFiles[0], proposal.ExpectedSHA256)
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
