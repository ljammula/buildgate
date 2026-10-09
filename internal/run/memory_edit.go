package run

import (
	"strings"

	"buildgate/internal/sanitize"
)

const (
	maxMemoryEditOtherFiles = 20
	maxMemoryEditRootNames  = 8
	maxMemoryEditPathLen    = 400
	maxMemoryEditErrorLen   = 300
	// RootInstructionFile is the one spelling of the root instruction file.
	RootInstructionFile = "AGENTS.md"
)

// RootInstructionName returns the root-level name a changed path belongs to
// when that name folds to AGENTS.md, and "" otherwise. The fold is
// strings.EqualFold, the one internal/release uses for the factory's own
// directories on a case-insensitive worktree (simple Unicode folding, so the
// long s U+017F matches S), applied after dropping the code points HFS+
// ignores in a file name. "agents.md" and "Agents.MD/x" both belong to a
// root instruction name; "docs/AGENTS.md" does not.
func RootInstructionName(path string) string {
	first, _, _ := strings.Cut(path, "/")
	if strings.EqualFold(strings.Map(dropIgnorable, first), RootInstructionFile) {
		return first
	}
	return ""
}

// dropIgnorable removes the code points HFS+ ignores when it compares names.
func dropIgnorable(r rune) rune {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return -1
	}
	return r
}

// ChangedRootInstructionNames is the distinct root instruction names among
// changed, in order.
func ChangedRootInstructionNames(changed []string) []string {
	var names []string
	seen := map[string]bool{}
	for _, c := range changed {
		if name := RootInstructionName(c); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// MemoryEdit is what the host read from git objects about the root
// instruction names (AGENTS.md in any letter case) when a run was accepted.
// The release policy reads it and does no I/O. It decides on whether a list
// is empty, never on a name in it.
type MemoryEdit struct {
	// Proposal: a proposal exists for the run's request (a memory run).
	Proposal bool `json:"proposal"`
	// Matches: the proposal exists, the result tree holds exactly one root
	// instruction name, it is spelled AGENTS.md, it is a regular file of
	// mode 100644 and its SHA-256 equals the proposal's expected_sha256.
	Matches bool `json:"matches"`
	// SwitchedOff: the run has a proposal and its repository's memory store
	// held the off marker (`factoryd memory off`) when the host looked.
	SwitchedOff bool `json:"switched_off,omitempty"`
	// BaseMoved: the run has a proposal and root AGENTS.md at the diff base
	// is not the file the proposal was rendered from (its SHA-256 differs
	// from the proposal's base_blob_sha256, or one has a file and the other
	// none).
	BaseMoved bool `json:"base_moved,omitempty"`
	// BaseHasSection: a root instruction file at the run's diff base holds
	// the text "buildgate:memory" in any letter case.
	BaseHasSection bool `json:"base_has_section"`
	// ChangedRootNames is every root instruction name the run changed:
	// content, mode, type, added, deleted, either side of a move.
	ChangedRootNames []string `json:"changed_root_names,omitempty"`
	// ResultRootNames is every root instruction name in the result tree.
	ResultRootNames []string `json:"result_root_names,omitempty"`
	// MarkerIn is the result root instruction names that hold
	// "buildgate:memory", plainly or as HTML character references. Read
	// only for a run with no proposal on a base with no section.
	MarkerIn []string `json:"marker_in,omitempty"`
	// NotRegularFile is the root instruction names the run changed into a
	// symlink, a submodule or a directory. Read as MarkerIn is.
	NotRegularFile []string `json:"not_regular_file,omitempty"`
	// OtherFilesChanged is every changed file except root AGENTS.md, capped.
	OtherFilesChanged []string `json:"other_files_changed,omitempty"`
	// BaseSHA256 and ResultSHA256 hash root AGENTS.md at the diff base and
	// at the result commit; "" when it is absent or not a regular file.
	BaseSHA256   string `json:"base_sha256,omitempty"`
	ResultSHA256 string `json:"result_sha256,omitempty"`
	// Error is why the host could not compute the rest (cleaned, capped).
	// Evidence is computed only for a run that has a proposal or changed a
	// root instruction name, so an error always denies the release.
	Error string `json:"error,omitempty"`
}

// Clean bounds the recorded paths and the error text, as other recorded
// paths are bounded. A list that held a name still holds one afterwards.
func (m *MemoryEdit) Clean() {
	if m == nil {
		return
	}
	m.OtherFilesChanged = cleanNames(m.OtherFilesChanged, maxMemoryEditOtherFiles)
	m.ChangedRootNames = cleanNames(m.ChangedRootNames, maxMemoryEditRootNames)
	m.ResultRootNames = cleanResultNames(m.ResultRootNames)
	m.MarkerIn = cleanNames(m.MarkerIn, maxMemoryEditRootNames)
	m.NotRegularFile = cleanNames(m.NotRegularFile, maxMemoryEditRootNames)
	hadError := m.Error != ""
	m.Error = clipRunes(sanitize.Line(m.Error), maxMemoryEditErrorLen)
	if hadError && m.Error == "" {
		m.Error = "(error with no printable characters)"
	}
}

const unprintableName = "(path with no printable characters)"

// cleanNames must never turn "a name is listed" into "none is".
func cleanNames(in []string, maxItems int) []string {
	out := cleanLines(in, maxItems, maxMemoryEditPathLen)
	if len(in) > 0 && len(out) == 0 {
		return []string{unprintableName}
	}
	return out
}

// cleanResultNames keeps "more than one name" true: the policy counts it.
func cleanResultNames(in []string) []string {
	out := cleanNames(in, maxMemoryEditRootNames)
	for len(in) > 1 && len(out) < 2 {
		out = append(out, unprintableName)
	}
	return out
}
