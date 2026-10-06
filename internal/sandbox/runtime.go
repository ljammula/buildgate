package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"buildgate/internal/run"
)

// Runtime is the sandbox runtime a worker is launched through: the OpenShell
// gateway, which creates the worker and its supervisor. Every method reaches
// the gateway or Docker, so callers hold it as a dependency and tests pass a
// fake.
//
// A sandbox is addressed by the name the run recorded before Create
// (RecordSandbox); the gateway's own id comes back in SandboxRef and names
// the run's meter ledger file.
type Runtime interface {
	// Create starts the sandbox and returns once the worker container exists.
	Create(ctx context.Context, req SandboxRequest) (SandboxRef, error)
	// Wait blocks until the sandbox's command exits.
	Wait(ctx context.Context, name string) (SandboxExit, error)
	// Status reports whether the sandbox exists and when its worker
	// container last started. A gateway that cannot be asked is an error,
	// never "absent".
	Status(ctx context.Context, name string) (SandboxState, error)
	// Delete removes the sandbox through the gateway, falls back to removing
	// its containers, and returns nil only when none is left.
	Delete(ctx context.Context, name string) error
	// ListByRun returns the names of the run's sandboxes that still exist:
	// each recorded name the gateway reports or a container still carries.
	ListByRun(ctx context.Context, dataDir, runID string) ([]string, error)
	// PushCredential stores a route's current credential in the gateway's
	// provider store. A sandbox reads it at start only, so it is pushed
	// before each Create.
	PushCredential(ctx context.Context, cred RouteCredential) error
}

// SandboxRequest is one worker launch, built by LaunchSpec.SandboxRequest.
type SandboxRequest struct {
	Name    string
	DataDir string
	RunID   string
	Image   string
	Command []string
	// Environment is KEY=VALUE entries; a later entry replaces an earlier
	// one with the same key. The gateway stores every value.
	Environment []string
	Memory      string
	CPUs        string
	Mounts      []SandboxMount
	// ReadOnlyPaths and ReadWritePaths are the container paths the
	// filesystem policy opens to the worker.
	ReadOnlyPaths  []string
	ReadWritePaths []string
	// GuardDir and OutputDir are the host directories mounted at
	// WorkerGuardMount and WorkerOutputMount.
	GuardDir  string
	OutputDir string
	Timeout   time.Duration
	// Route is the worker's model route, nil for a step that calls no
	// model: such a sandbox reaches nothing. MeterConfig is the route's
	// meter configuration (RoutePolicy.MeterConfig), required with Route.
	Route       *RouteAccess
	MeterConfig map[string]any
	// Sidecars are the factory's own containers the worker may reach, each
	// by address: the worker joins no Docker network and resolves no alias.
	Sidecars []SidecarEndpoint
}

// SidecarEndpoint is one container of the factory's (the registry proxy, a
// compose service) as a worker launched through a Runtime reaches it: its
// address on the factory-owned network and the ports it serves.
type SidecarEndpoint struct {
	Name  string
	IP    string
	Ports []int
}

// SandboxMount is a bind of a host path, or a size-capped tmpfs when Tmpfs
// is set (Source is then empty).
type SandboxMount struct {
	Tmpfs     bool
	Source    string
	Target    string
	ReadOnly  bool
	SizeBytes int64
	Mode      uint32
	Options   []string
}

// SandboxRef identifies a created sandbox.
type SandboxRef struct {
	Name string
	// ID is the gateway's id for the sandbox.
	ID string
}

// SandboxExit is how a sandbox's command ended.
type SandboxExit struct {
	ExitCode int
	Phase    string
}

// SandboxState is a sandbox as the gateway and Docker report it now.
type SandboxState struct {
	Present bool
	// StartedAt is the worker container's Docker State.StartedAt, verbatim.
	// It changes when the runtime starts the command again.
	StartedAt string
}

// RouteCredential is the secret values of one route's provider, built by
// RoutePolicy.RouteCredential. Like RouteSecret it refuses to be
// serialized and prints redacted; only a Runtime reads Secrets, to push them.
type RouteCredential struct {
	Provider string
	// ExpiresAt is zero for a credential with no known expiry.
	ExpiresAt time.Time
	values    map[string]string
}

// Secrets maps each environment name the worker holds a placeholder for to
// the secret the supervisor substitutes.
func (c RouteCredential) Secrets() map[string]string {
	out := make(map[string]string, len(c.values))
	for name, value := range c.values {
		out[name] = value
	}
	return out
}

func (c RouteCredential) String() string {
	return fmt.Sprintf("{provider %s, %d values [REDACTED]}", c.Provider, len(c.values))
}

func (c RouteCredential) GoString() string { return c.String() }

func (c RouteCredential) MarshalJSON() ([]byte, error) {
	return nil, errRelayCredentialNotSerializable
}

// maxSandboxNameLength is the gateway's limit on a sandbox name.
const maxSandboxNameLength = 19

var (
	sandboxNamePattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	sandboxNameDisallowed = regexp.MustCompile(`[^a-z0-9]+`)
)

// SandboxName returns the gateway name of one launch: "bg-" and 16 hex
// characters of a hash of the data directory, the run id and the nonce. The
// gateway allows 19 characters, too few to spell a run id, so the name
// identifies nothing by itself: the run's record (RecordSandbox) maps it back.
func SandboxName(dataDir, runID, nonce string) (string, error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if runID == "" || nonce == "" {
		return "", fmt.Errorf("sandbox name: run id and nonce are required (got run id %q, nonce %q)", runID, nonce)
	}
	sum := sha256.Sum256([]byte(absDataDir + "\x00" + runID + "\x00" + nonce))
	return "bg-" + hex.EncodeToString(sum[:])[:16], nil
}

// SandboxRecord is one line of a run's sandbox record.
type SandboxRecord struct {
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	// Ledger is the sandbox's meter ledger file, set for a launch with a
	// model route. The run's spend is the sum of the ledgers it recorded.
	Ledger string `json:"ledger,omitempty"`
}

const sandboxRecordFileName = "sandboxes.jsonl"

// RecordSandbox appends rec to the run's sandbox record and syncs it. A run
// writes the name before Create, so a sandbox the gateway starts is always
// findable from the run's own directory, and again with the id and start
// time once the worker is up.
func RecordSandbox(dataDir, runID string, rec SandboxRecord) error {
	if dataDir == "" || runID == "" {
		return fmt.Errorf("record sandbox: runID and dataDir are required (got runID=%q dataDir=%q)", runID, dataDir)
	}
	if len(rec.Name) > maxSandboxNameLength || !sandboxNamePattern.MatchString(rec.Name) {
		return fmt.Errorf("record sandbox: invalid sandbox name %q", rec.Name)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("record sandbox: %w", err)
	}
	dir := run.Dir(dataDir, runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("record sandbox: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, sandboxRecordFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("record sandbox: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("record sandbox: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("record sandbox: %w", err)
	}
	return f.Close()
}

// RecordedSandboxes returns the run's sandboxes in the order first recorded,
// one entry per name, a later line's non-empty fields replacing an earlier
// line's. A run with no record has none. A line that does not parse is an
// error: a name lost from the record is a sandbox no sweep would look for.
func RecordedSandboxes(dataDir, runID string) ([]SandboxRecord, error) {
	if dataDir == "" || runID == "" {
		return nil, fmt.Errorf("read sandbox record: runID and dataDir are required (got runID=%q dataDir=%q)", runID, dataDir)
	}
	path := filepath.Join(run.Dir(dataDir, runID), sandboxRecordFileName)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sandbox record: %w", err)
	}
	defer f.Close()
	var records []SandboxRecord
	index := map[string]int{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var rec SandboxRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil || rec.Name == "" {
			return nil, fmt.Errorf("read sandbox record %s: unreadable line %q", path, scanner.Text())
		}
		i, seen := index[rec.Name]
		if !seen {
			index[rec.Name] = len(records)
			records = append(records, rec)
			continue
		}
		if rec.ID != "" {
			records[i].ID = rec.ID
		}
		if rec.StartedAt != "" {
			records[i].StartedAt = rec.StartedAt
		}
		if rec.Ledger != "" {
			records[i].Ledger = rec.Ledger
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sandbox record %s: %w", path, err)
	}
	return records, nil
}

// DataDirLabel is the label value that identifies a data directory on a
// resource the factory created: a hash of its absolute path.
func DataDirLabel(dataDir string) (string, error) {
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	return dataDirLabel(absDataDir), nil
}
