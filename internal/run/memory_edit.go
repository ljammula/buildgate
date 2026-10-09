package run

import "buildgate/internal/sanitize"

const (
	maxMemoryEditOtherFiles = 20
	maxMemoryEditPathLen    = 400
	maxMemoryEditErrorLen   = 300
)

// MemoryEdit is what the host read from git objects about root AGENTS.md
// when a run was accepted. The release policy reads it and does no I/O.
type MemoryEdit struct {
	// SectionChanged: the fenced memory block (markers included) differs
	// between the run's diff base and its result, or either side cannot be
	// read as one clean block while the two files differ, or a case variant
	// of AGENTS.md at the root carrying a marker was added or changed.
	SectionChanged bool `json:"section_changed"`
	// Proposal: a proposal exists for the run's request.
	Proposal bool `json:"proposal"`
	// Matches: the proposal exists and the SHA-256 of AGENTS.md at the
	// result commit equals its expected_sha256.
	Matches bool `json:"matches"`
	// OtherFilesChanged is every changed file except root AGENTS.md, capped.
	OtherFilesChanged []string `json:"other_files_changed,omitempty"`
	// BaseSHA256 and ResultSHA256 hash root AGENTS.md at the diff base and
	// at the result commit; "" when the file is absent there.
	BaseSHA256   string `json:"base_sha256,omitempty"`
	ResultSHA256 string `json:"result_sha256,omitempty"`
	// Error is why the host could not compute the rest (cleaned, capped).
	Error string `json:"error,omitempty"`
	// FailClosed: Error is set and the run is one the section rule governs
	// (it has a proposal, or it changed a root AGENTS.md file), so the
	// release policy denies it. A failure for any other run is no evidence.
	FailClosed bool `json:"fail_closed,omitempty"`
}

// Clean bounds the recorded paths and the error text, as other recorded
// paths are bounded.
func (m *MemoryEdit) Clean() {
	if m == nil {
		return
	}
	had := len(m.OtherFilesChanged)
	m.OtherFilesChanged = cleanLines(m.OtherFilesChanged, maxMemoryEditOtherFiles, maxMemoryEditPathLen)
	if had > 0 && len(m.OtherFilesChanged) == 0 {
		// Cleaning must never turn "another file changed" into "none did".
		m.OtherFilesChanged = []string{"(path with no printable characters)"}
	}
	m.Error = clipRunes(sanitize.Line(m.Error), maxMemoryEditErrorLen)
}
