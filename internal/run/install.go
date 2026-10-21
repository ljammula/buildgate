package run

// InstallAttemptKind is Attempt.Kind of the launch that ran a repository's
// approved sandbox install on a scratch worktree to build its install image.
// A run that reused a cached image records none.
const InstallAttemptKind = "install"

// Install image scopes (InstallImage.Scope).
const (
	// InstallScopeShared is an image built from the commit .factory.yml was
	// read from, which later runs with the same key reuse.
	InstallScopeShared = "shared"
	// InstallScopeRunPrefix starts the scope of an image only its own run
	// uses ("run:<run id>"): its starting commit's install inputs differ
	// from the trusted commit's, so no other run looks it up, and it is
	// removed when the run ends.
	InstallScopeRunPrefix = "run:"
)

// InstallInput is one file the install image's key covers: its path at the
// commit the image was built from and its git blob id there.
type InstallInput struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}

// InstallImage is the evidence of a run's install image: the image every
// repository-command sandbox of the run launched from, with the repository's
// approved install restored into /workspace before its command.
type InstallImage struct {
	// Key is sandbox.InstallImageKey of every input that decides what the
	// install writes.
	Key string `json:"key"`
	// Scope is InstallScopeShared or InstallScopeRunPrefix plus the run id.
	Scope string `json:"scope"`
	// Image is the install image, pinned by digest.
	Image string `json:"image"`
	// CacheHit is true when the image was already built by an earlier run.
	CacheHit bool `json:"cache_hit"`
	// BuiltByRunID is the run whose install built the image, from its label.
	BuiltByRunID string `json:"built_by_run_id,omitempty"`
	// Commit is the commit the install ran on.
	Commit string `json:"commit"`
	// SandboxSHA256 is the approved sandbox block's hash.
	SandboxSHA256 string `json:"sandbox_sha256"`
	// ApprovedBy and ApprovedAt are the approval the worker found for it.
	ApprovedBy string `json:"approved_by,omitempty"`
	ApprovedAt string `json:"approved_at,omitempty"`
	// Inputs are the files the key covers, sorted by path.
	Inputs []InstallInput `json:"inputs,omitempty"`
	// DurationMs is how long the install launch, capture and image build
	// took; zero on a cache hit.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// LogPath is the install launch's log; empty on a cache hit.
	LogPath string `json:"log_path,omitempty"`
}

// Shared reports whether the image may be reused by another run.
func (i *InstallImage) Shared() bool { return i != nil && i.Scope == InstallScopeShared }

// InstallNotRunMessage is the Command of the canonical_verify result recorded,
// with exit -1, for an accepted run whose repository has an approved sandbox
// block but whose result carries no install image record: a worker older
// than the install image judged the run. It is the operator's.
const InstallNotRunMessage = "buildgate: this worker did not prepare the repository's install image (it predates it): restart the worker with `factoryd restart`"

// InstallNotRun reports whether g is the result recorded for a run whose
// install image was never prepared (InstallNotRunMessage).
func (g GateResult) InstallNotRun() bool {
	return g.Check == "canonical_verify" && !g.Passed && g.ExitCode == -1 && len(g.Command) == 1 && g.Command[0] == InstallNotRunMessage
}
