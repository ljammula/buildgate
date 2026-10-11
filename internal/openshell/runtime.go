package openshell

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"

	"buildgate/internal/sandbox"
)

// Containers is what the runtime asks Docker about the containers the gateway
// created for a sandbox: the gateway labels the worker and its supervisor
// with the sandbox's name.
type Containers interface {
	// WorkerStartedAt is the worker container's State.StartedAt, "" when the
	// sandbox has no worker container.
	WorkerStartedAt(ctx context.Context, sandboxName string) (string, error)
	// Present reports whether any container of the sandbox exists.
	Present(ctx context.Context, sandboxName string) (bool, error)
	// Remove force-removes every container of the sandbox.
	Remove(ctx context.Context, sandboxName string) error
}

// DefaultWorkspace is the gateway workspace buildgate's sandboxes live in.
const DefaultWorkspace = "default"

// Runtime is a sandbox.Runtime over a gateway client and Docker.
type Runtime struct {
	Client     v1.ClientInterface
	Containers Containers
	// Readiness is asked before Create returns a sandbox with a
	// credentialed model route.
	Readiness RouteReadiness
	// Workspace defaults to DefaultWorkspace; PollEvery, Wait's interval, to
	// two seconds.
	Workspace string
	PollEvery time.Duration
	// Sleep waits out the supervisor's first settings poll
	// (waitFirstSettingsPoll); nil is a timer.
	Sleep func(ctx context.Context, d time.Duration) error

	// pushed is what PushCredential last stored in each provider.
	pushedMu sync.Mutex
	pushed   map[string]pushedCredential
}

// pushedCredential is one stored credential as this process knows it: its
// digest, and the revision the gateway gave the provider when it took it.
type pushedCredential struct {
	digest   string
	revision uint64
}

var _ sandbox.Runtime = (*Runtime)(nil)

func (r *Runtime) workspace() string {
	if r.Workspace == "" {
		return DefaultWorkspace
	}
	return r.Workspace
}

// Create asks the gateway for the sandbox and waits until it is ready and
// its supervisor has rebuilt its proxy: when its route has a credential,
// until that credential is installed (waitRouteReady); when it reaches the
// network with no credential, for the first settings poll
// (waitFirstSettingsPoll). A failure can leave a sandbox behind: the caller
// deletes by the name it recorded.
func (r *Runtime) Create(ctx context.Context, req sandbox.SandboxRequest) (sandbox.SandboxRef, error) {
	spec, err := sandboxSpec(req)
	if err != nil {
		return sandbox.SandboxRef{}, err
	}
	dataDirLabel, err := sandbox.DataDirLabel(req.DataDir)
	if err != nil {
		return sandbox.SandboxRef{}, err
	}
	labels := map[string]string{LabelRun: labelValue(req.RunID), LabelDataDir: labelValue(dataDirLabel)}
	template, err := workloadTemplate(req, spec)
	if err != nil {
		return sandbox.SandboxRef{}, err
	}
	if _, err := r.Client.SandboxTemplates().Create(ctx, r.workspace(), template); err != nil {
		return sandbox.SandboxRef{}, fmt.Errorf("openshell: create workload template %s: %w", req.Name, err)
	}
	created, err := r.Client.CreateSandboxFromTemplate(ctx, r.workspace(), req.Name, template.Name, spec, labels)
	if err != nil {
		return sandbox.SandboxRef{}, fmt.Errorf("openshell: create sandbox %s: %w", req.Name, err)
	}
	// The template has done its work once the sandbox exists. Removing it
	// now leaves the sandbox as the only gateway object of this launch, so
	// removing the sandbox's containers is enough for the gateway to forget
	// the launch. Delete removes it again in case this fails.
	_, _ = r.Client.SandboxTemplates().Delete(ctx, r.workspace(), template.Name, v1.DeleteOptions{AllowMissing: true})
	ready, err := r.Client.Sandboxes().WaitReady(ctx, r.workspace(), created.Name)
	if err != nil {
		return sandbox.SandboxRef{}, fmt.Errorf("openshell: sandbox %s did not become ready: %w%s", req.Name, err, r.conditions(ctx, created.Name))
	}
	switch {
	case req.Route != nil && req.Route.Provider != "":
		if err := r.waitRouteReady(ctx, created.Name, req.Route.Provider); err != nil {
			return sandbox.SandboxRef{}, err
		}
	case req.Route != nil || len(req.Sidecars) > 0:
		if err := r.waitFirstSettingsPoll(ctx, created.Name); err != nil {
			return sandbox.SandboxRef{}, err
		}
	}
	return sandbox.SandboxRef{Name: ready.Name, ID: ready.ID}, nil
}

// conditions is the gateway's own account of a sandbox that failed, for an
// error message: "" when there is none to read.
func (r *Runtime) conditions(ctx context.Context, name string) string {
	sb, err := r.Client.Sandboxes().Get(ctx, r.workspace(), name)
	if err != nil {
		return ""
	}
	out := ""
	for _, c := range sb.Status.Conditions {
		if c.Reason != "" || c.Message != "" {
			out += "; " + c.Reason + ": " + c.Message
		}
	}
	return out
}

// Wait polls the gateway until the sandbox's command has exited.
func (r *Runtime) Wait(ctx context.Context, name string) (sandbox.SandboxExit, error) {
	every := r.PollEvery
	if every <= 0 {
		every = 2 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		sb, err := r.Client.Sandboxes().Get(ctx, r.workspace(), name)
		if err != nil {
			return sandbox.SandboxExit{}, fmt.Errorf("openshell: read sandbox %s: %w", name, err)
		}
		if sb.Status.ExitCode != nil {
			return sandbox.SandboxExit{ExitCode: int(*sb.Status.ExitCode), Phase: string(sb.Status.Phase)}, nil
		}
		switch sb.Status.Phase {
		case v1.SandboxCompleted, v1.SandboxError, v1.SandboxStopped:
			return sandbox.SandboxExit{}, fmt.Errorf("openshell: sandbox %s ended in phase %s with no exit code", name, sb.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return sandbox.SandboxExit{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Status asks the gateway whether the sandbox exists and Docker when its
// worker container started.
func (r *Runtime) Status(ctx context.Context, name string) (sandbox.SandboxState, error) {
	present, err := r.present(ctx, name)
	if err != nil || !present {
		return sandbox.SandboxState{}, err
	}
	startedAt, err := r.Containers.WorkerStartedAt(ctx, name)
	if err != nil {
		return sandbox.SandboxState{}, fmt.Errorf("openshell: read worker start of sandbox %s: %w", name, err)
	}
	return sandbox.SandboxState{Present: true, StartedAt: startedAt}, nil
}

// present reports whether the gateway knows the sandbox or Docker still has
// a container of it.
func (r *Runtime) present(ctx context.Context, name string) (bool, error) {
	_, err := r.Client.Sandboxes().Get(ctx, r.workspace(), name)
	switch {
	case err == nil:
		return true, nil
	case !v1.IsNotFound(err):
		return false, fmt.Errorf("openshell: read sandbox %s: %w", name, err)
	}
	present, err := r.Containers.Present(ctx, name)
	if err != nil {
		return false, fmt.Errorf("openshell: list containers of sandbox %s: %w", name, err)
	}
	return present, nil
}

// Delete removes the sandbox through the gateway, removes whatever container
// is left, and confirms. A gateway that cannot be asked does not stop the
// container removal, but the result is then an error: the gateway may still
// hold the sandbox and start it again.
func (r *Runtime) Delete(ctx context.Context, name string) error {
	_, gatewayErr := r.Client.Sandboxes().Delete(ctx, r.workspace(), name, v1.DeleteOptions{AllowMissing: true})
	if v1.IsNotFound(gatewayErr) {
		gatewayErr = nil
	}
	// The launch's workload template shares the sandbox's name.
	if _, err := r.Client.SandboxTemplates().Delete(ctx, r.workspace(), name, v1.DeleteOptions{AllowMissing: true}); err != nil && !v1.IsNotFound(err) {
		gatewayErr = errors.Join(gatewayErr, fmt.Errorf("delete workload template: %w", err))
	}
	removeErr := r.Containers.Remove(ctx, name)
	left, presentErr := r.Containers.Present(ctx, name)
	switch {
	case presentErr != nil:
		return fmt.Errorf("%w: sandbox %s: %v", sandbox.ErrCleanupUnconfirmed, name, errors.Join(gatewayErr, removeErr, presentErr))
	case left:
		return fmt.Errorf("%w: sandbox %s still has a container: %v", sandbox.ErrCleanupUnconfirmed, name, errors.Join(gatewayErr, removeErr))
	case gatewayErr != nil:
		return fmt.Errorf("%w: sandbox %s: the gateway did not confirm the delete: %v", sandbox.ErrCleanupUnconfirmed, name, gatewayErr)
	}
	return nil
}

// ListByRun returns the run's recorded sandboxes that still exist.
func (r *Runtime) ListByRun(ctx context.Context, dataDir, runID string) ([]string, error) {
	records, err := sandbox.RecordedSandboxes(dataDir, runID)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, rec := range records {
		present, err := r.present(ctx, rec.Name)
		if err != nil {
			return nil, err
		}
		if present {
			names = append(names, rec.Name)
		}
	}
	return names, nil
}

// credentialDigest is the SHA-256 of a credential as stored: its profile,
// every name and value, and its expiry.
func credentialDigest(profileID string, secrets map[string]string, expiresAt time.Time) string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	writeField := func(field string) {
		_ = binary.Write(h, binary.BigEndian, uint64(len(field)))
		h.Write([]byte(field))
	}
	writeField(profileID)
	for _, name := range names {
		writeField(name)
		writeField(secrets[name])
	}
	if !expiresAt.IsZero() {
		writeField(expiresAt.UTC().Format(time.RFC3339Nano))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PushCredential stores the credential in the route's provider, creating the
// profile and the provider on first use. A provider this process already
// gave this credential, and that nothing has changed since (its revision is
// the one that push returned), is left as it is: every update gives the
// provider a new revision, and each running sandbox's supervisor drops its
// model streams in flight to load it, so a push per launch cut the long
// requests of every other job on the route (found live, 2026-10-10: three
// reviews of large diffs lost all six tries of a stream to the launches of
// other jobs). The gateway returns no stored value and keeps no metadata an
// update sets, so what was stored is remembered here: a new process stores
// the credential once, and a provider anything else changed is stored again.
func (r *Runtime) PushCredential(ctx context.Context, cred sandbox.RouteCredential) error {
	secrets := cred.Secrets()
	if cred.Provider == "" || len(secrets) == 0 {
		return errors.New("openshell: a credential needs a provider and at least one value")
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	profile := credentialProfile(names)
	profiles := r.Client.Providers().Profiles()
	if _, err := profiles.Get(ctx, r.workspace(), profile.ID); v1.IsNotFound(err) {
		result, err := profiles.Import(ctx, r.workspace(), []v1.ProfileImportItem{{Profile: profile, Source: "buildgate"}})
		switch {
		case v1.IsAlreadyExists(err):
		case err != nil:
			return fmt.Errorf("openshell: import provider profile %s: %w", profile.ID, err)
		case !result.Imported:
			// The gateway reports a profile it will not take as diagnostics,
			// not as an error.
			return fmt.Errorf("openshell: the gateway did not import provider profile %s: %s", profile.ID, diagnostics(result.Diagnostics))
		}
	} else if err != nil {
		return fmt.Errorf("openshell: read provider profile %s: %w", profile.ID, err)
	}

	// ProfileWorkspace: a provider looks its profile up in the platform's
	// scope unless told the profile is the workspace's own.
	spec := v1.ProviderSpec{Credentials: secrets, ProfileWorkspace: r.workspace()}
	if !cred.ExpiresAt.IsZero() {
		spec.CredentialExpiresAt = map[string]time.Time{}
		for name := range secrets {
			spec.CredentialExpiresAt[name] = cred.ExpiresAt
		}
	}
	digest := credentialDigest(profile.ID, secrets, cred.ExpiresAt)
	providers := r.Client.Providers()
	existing, err := providers.Get(ctx, r.workspace(), cred.Provider)
	var stored *v1.Provider
	switch {
	case v1.IsNotFound(err):
		stored, err = providers.Create(ctx, r.workspace(), &v1.Provider{Name: cred.Provider, Type: profile.ID, Spec: spec})
	case err == nil:
		if r.holds(cred.Provider, pushedCredential{digest: digest, revision: existing.ResourceVersion}) {
			return nil
		}
		existing.Type, existing.Spec = profile.ID, spec
		stored, err = providers.Update(ctx, r.workspace(), existing)
	}
	if err != nil {
		// The SDK's errors carry gateway messages, never the request body.
		return fmt.Errorf("openshell: store credential in provider %s: %w", cred.Provider, err)
	}
	var revision uint64
	if stored != nil {
		revision = stored.ResourceVersion
	}
	r.remember(cred.Provider, pushedCredential{digest: digest, revision: revision})
	return nil
}

// holds reports whether the provider was last stored by this process with
// exactly this credential at exactly this revision.
func (r *Runtime) holds(provider string, want pushedCredential) bool {
	r.pushedMu.Lock()
	defer r.pushedMu.Unlock()
	got, ok := r.pushed[provider]
	return ok && got == want
}

// remember records a stored credential. A revision of zero (a gateway that
// reports none) is never matched, so such a provider is stored every time.
func (r *Runtime) remember(provider string, stored pushedCredential) {
	r.pushedMu.Lock()
	defer r.pushedMu.Unlock()
	if stored.revision == 0 {
		delete(r.pushed, provider)
		return
	}
	if r.pushed == nil {
		r.pushed = map[string]pushedCredential{}
	}
	r.pushed[provider] = stored
}

// Connect opens a gateway client authenticated with the client bundle in
// bundleDir (ca.crt, tls.crt, tls.key), the factory's mTLS identity.
func Connect(address, bundleDir string) (*v1.Client, error) {
	client, err := v1.NewClient(v1.Config{
		Address: address,
		TLS: &v1.TLSConfig{
			CAFile:   filepath.Join(bundleDir, "ca.crt"),
			CertFile: filepath.Join(bundleDir, "tls.crt"),
			KeyFile:  filepath.Join(bundleDir, "tls.key"),
		},
		Auth: v1.NoAuth(),
	})
	if err != nil {
		return nil, fmt.Errorf("openshell: connect to the gateway at %s: %w", address, err)
	}
	return client, nil
}

// maxLabelValueLength is the gateway's limit on a label value.
const maxLabelValueLength = 63

var labelValueDisallowed = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// labelValue fits s to what the gateway accepts as a label value: letters,
// digits, '-', '_' and '.', at most maxLabelValueLength long. Labels are for
// a person listing sandboxes; the run's own record is what the factory reads.
func labelValue(s string) string {
	s = labelValueDisallowed.ReplaceAllString(s, "-")
	if len(s) > maxLabelValueLength {
		s = s[:maxLabelValueLength]
	}
	return s
}

func diagnostics(list []v1.ProfileDiagnostic) string {
	if len(list) == 0 {
		return "no reason given"
	}
	out := ""
	for i, d := range list {
		if i > 0 {
			out += "; "
		}
		out += d.Severity + " " + d.Field + ": " + d.Message
	}
	return out
}

// Names lists every sandbox the gateway holds, whichever run made it.
func (r *Runtime) Names(ctx context.Context) ([]string, error) {
	all, err := r.Client.Sandboxes().ListAll(ctx, r.workspace())
	if err != nil {
		return nil, fmt.Errorf("openshell: list sandboxes: %w", err)
	}
	names := make([]string, 0, len(all))
	for _, sb := range all {
		names = append(names, sb.Name)
	}
	return names, nil
}
