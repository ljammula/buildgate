package openshell

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/fake"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"

	"buildgate/internal/meter"
	"buildgate/internal/sandbox"
)

// fakeContainers is Docker's view of sandboxes, held in memory.
type fakeContainers struct {
	startedAt map[string]string // sandbox name -> worker start; presence means a container exists
	removeErr error
	listErr   error
	sticky    bool // Remove leaves the container
	removed   []string
}

func (f *fakeContainers) WorkerStartedAt(_ context.Context, name string) (string, error) {
	return f.startedAt[name], f.listErr
}

func (f *fakeContainers) Present(_ context.Context, name string) (bool, error) {
	_, ok := f.startedAt[name]
	return ok, f.listErr
}

func (f *fakeContainers) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	if !f.sticky {
		delete(f.startedAt, name)
	}
	return f.removeErr
}

func testRequest() sandbox.SandboxRequest {
	return sandbox.SandboxRequest{
		Name: "bg-0123456789abcdef", DataDir: "/data", RunID: "run1",
		Image:       "worker@sha256:deadbeef",
		Command:     []string{"/bin/sh", "-c", "script", "--", "python3", "build.py"},
		Environment: []string{"PATH=/first", "FACTORY_MODEL_ID=m", "PATH=/usr/local/bin:/usr/bin:/bin"},
		Memory:      "4g", CPUs: "2",
		Mounts: []sandbox.SandboxMount{
			{Tmpfs: true, Target: "/tmp", SizeBytes: 1 << 30, Mode: 0o1777, Options: []string{"noexec", "nosuid"}},
			{Source: "/host/work", Target: "/workspace"},
			{Source: "/host/guard", Target: "/guard", ReadOnly: true},
		},
		ReadOnlyPaths:  []string{"/guard"},
		ReadWritePaths: []string{"/workspace", "/tmp"},
		Timeout:        time.Hour,
	}
}

func testRoute() *sandbox.RouteAccess {
	return &sandbox.RouteAccess{
		Provider:      "bg-12345678-chatgpt",
		CredentialEnv: []string{"BG_CHATGPT_TOKEN", "BG_CHATGPT_ACCOUNT"},
		Endpoint: sandbox.RouteEndpoint{
			Host: "chatgpt.com", Port: 443, TLS: true, Method: "POST",
			Path: "/backend-api/codex/responses", Binaries: []string{"/usr/local/bin/node"},
		},
	}
}

func TestSandboxSpecWithNoRouteOpensNoNetwork(t *testing.T) {
	spec, err := sandboxSpec(testRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := &v1.SandboxSpec{
		Environment: map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "FACTORY_MODEL_ID": "m"},
		Command:     []string{"/bin/sh", "-c", "script", "--", "python3", "build.py"},
		Template: &v1.SandboxTemplate{
			Image: "worker@sha256:deadbeef",
			DriverConfig: map[string]any{"docker": map[string]any{"mounts": []any{
				map[string]any{"type": "tmpfs", "target": "/tmp", "size_bytes": int64(1 << 30), "mode": int64(0o1777), "options": []any{"noexec", "nosuid"}},
				map[string]any{"type": "bind", "source": "/host/work", "target": "/workspace", "read_only": false},
				map[string]any{"type": "bind", "source": "/host/guard", "target": "/guard", "read_only": true},
			}}},
		},
		Policy: &v1.SandboxPolicy{
			Version:    1,
			Filesystem: &v1.FilesystemPolicy{ReadOnly: []string{"/guard"}, ReadWrite: []string{"/workspace", "/tmp"}},
			Landlock:   &v1.LandlockPolicy{Compatibility: "hard_requirement"},
		},
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec =\n%+v\n%+v\n%+v\nwant\n%+v\n%+v\n%+v", spec, spec.Template, spec.Policy, want, want.Template, want.Policy)
	}
}

func TestSandboxSpecWithARouteAdmitsItsEndpointBehindTheMeter(t *testing.T) {
	req := testRequest()
	req.Route = testRoute()
	req.MeterConfig = map[string]any{"run": "r", "token_ceiling": int64(5)}
	spec, err := sandboxSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bg-12345678-chatgpt"}; !reflect.DeepEqual(spec.Providers, want) {
		t.Errorf("Providers = %v, want %v", spec.Providers, want)
	}
	wantRules := map[string]v1.NetworkPolicyRule{"model": {
		Name: "model",
		Endpoints: []v1.PolicyNetworkEndpoint{{
			Host: "chatgpt.com", Port: 443, Protocol: "rest", Enforcement: v1.NetworkEnforcementModeEnforce,
			Rules:             []v1.L7Rule{{Allow: &v1.L7Allow{Method: "POST", Path: "/backend-api/codex/responses"}}},
			CredentialBinding: &types.NetworkCredentialBinding{Provider: "bg-12345678-chatgpt"},
		}},
		Binaries: []v1.PolicyNetworkBinary{{Path: "/usr/local/bin/node"}},
	}}
	if !reflect.DeepEqual(spec.Policy.NetworkPolicies, wantRules) {
		t.Errorf("NetworkPolicies =\n%+v\nwant\n%+v", spec.Policy.NetworkPolicies, wantRules)
	}
	wantMeter := map[string]types.NetworkMiddlewareConfig{"meter": {
		Name: "meter", Middleware: "factoryd-meter", OnError: "fail_closed", Order: 10,
		Config:    map[string]any{"run": "r", "token_ceiling": int64(5)},
		Endpoints: &types.MiddlewareEndpointSelector{Include: []string{"chatgpt.com"}},
	}}
	if !reflect.DeepEqual(spec.Policy.NetworkMiddlewares, wantMeter) {
		t.Errorf("NetworkMiddlewares =\n%+v\nwant\n%+v", spec.Policy.NetworkMiddlewares, wantMeter)
	}
}

func TestSandboxSpecForAPrefixRouteWithNoCredential(t *testing.T) {
	req := testRequest()
	req.Route = &sandbox.RouteAccess{Endpoint: sandbox.RouteEndpoint{
		Host: "100.64.0.1", Port: 8080, Method: "POST", Path: "/v1/chat/completions", PathIsPrefix: true,
		Binaries: []string{"/usr/local/bin/node"},
	}}
	req.MeterConfig = map[string]any{"run": "r"}
	spec, err := sandboxSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := spec.Policy.NetworkPolicies["model"].Endpoints[0]
	wantRules := []v1.L7Rule{
		{Allow: &v1.L7Allow{Method: "POST", Path: "/v1/chat/completions"}},
		{Allow: &v1.L7Allow{Method: "POST", Path: "/v1/chat/completions/**"}},
	}
	if !reflect.DeepEqual(endpoint.Rules, wantRules) {
		t.Errorf("Rules = %+v, want %+v", endpoint.Rules, wantRules)
	}
	if endpoint.CredentialBinding != nil || spec.Providers != nil {
		t.Errorf("a route with no provider got a credential binding (%v) or providers (%v)", endpoint.CredentialBinding, spec.Providers)
	}
}

func TestSandboxSpecRefuses(t *testing.T) {
	req := testRequest()
	req.Route = testRoute()
	if _, err := sandboxSpec(req); err == nil {
		t.Error("a route without a meter configuration was accepted")
	}
	req = testRequest()
	req.MeterConfig = map[string]any{"run": "r"}
	if _, err := sandboxSpec(req); err == nil {
		t.Error("a meter configuration without a route was accepted")
	}
	req = testRequest()
	req.Environment = []string{"NOVALUE"}
	if _, err := sandboxSpec(req); err == nil {
		t.Error("an environment entry with no = was accepted")
	}
}

func TestCredentialProfileDeclaresOnlyTheNames(t *testing.T) {
	got := credentialProfile([]string{"BG_CHATGPT_TOKEN", "BG_CHATGPT_ACCOUNT"})
	want := v1.ProviderProfile{
		ID:          "buildgate-bg-chatgpt-account-bg-chatgpt-token",
		DisplayName: "Buildgate model route credential",
		Description: "Placeholders for BG_CHATGPT_ACCOUNT, BG_CHATGPT_TOKEN",
		Category:    v1.ProfileCategoryInference,
		Credentials: []v1.ProfileCredential{
			{Name: "BG_CHATGPT_ACCOUNT", EnvVars: []string{"BG_CHATGPT_ACCOUNT"}, Required: true, Secret: true},
			{Name: "BG_CHATGPT_TOKEN", EnvVars: []string{"BG_CHATGPT_TOKEN"}, Required: true, Secret: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profile =\n%+v\nwant\n%+v", got, want)
	}
}

// fakeProfiles is the gateway's profile store, which the SDK's fake client
// does not implement.
type fakeProfiles struct {
	v1.ProfileInterface
	notFound error
	imported map[string]v1.ProviderProfile
}

func (f *fakeProfiles) Get(_ context.Context, _, id string) (*v1.ProviderProfile, error) {
	profile, ok := f.imported[id]
	if !ok {
		return nil, f.notFound
	}
	return &profile, nil
}

func (f *fakeProfiles) Import(_ context.Context, _ string, items []v1.ProfileImportItem) (*v1.ImportResult, error) {
	for _, item := range items {
		f.imported[item.Profile.ID] = item.Profile
	}
	return &v1.ImportResult{Imported: true}, nil
}

type providersWithProfiles struct {
	v1.ProviderInterface
	profiles *fakeProfiles
}

func (p providersWithProfiles) Profiles() v1.ProfileInterface { return p.profiles }

type clientWithProfiles struct {
	*fake.Client
	profiles *fakeProfiles
}

func (c clientWithProfiles) Providers() v1.ProviderInterface {
	return providersWithProfiles{ProviderInterface: c.Client.Providers(), profiles: c.profiles}
}

func newTestRuntime() (*Runtime, clientWithProfiles, *fakeContainers) {
	base := fake.NewClient()
	_, notFound := base.Providers().Get(context.Background(), DefaultWorkspace, "no-such-provider")
	client := clientWithProfiles{Client: base, profiles: &fakeProfiles{notFound: notFound, imported: map[string]v1.ProviderProfile{}}}
	containers := &fakeContainers{startedAt: map[string]string{}}
	rt := &Runtime{Client: client, Containers: containers, PollEvery: time.Millisecond}
	rt.Sleep = func(context.Context, time.Duration) error { return nil }
	return rt, client, containers
}

func TestCreateStoresTheSandboxWithItsRunLabels(t *testing.T) {
	rt, client, _ := newTestRuntime()
	ctx := context.Background()
	ref, err := rt.Create(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	// The SDK's fake assigns no id; the real gateway's is checked live.
	if ref.Name != "bg-0123456789abcdef" {
		t.Fatalf("ref = %+v", ref)
	}
	stored, err := client.Sandboxes().Get(ctx, DefaultWorkspace, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("/data"), cut to the gateway's 63-character label limit.
	if want := map[string]string{"buildgate.run": "run1", "buildgate.data-dir": "bd47413b5c03dfc6e4b5b8143fe205eae10ecaf0c28b426be98d74cff48c745"}; !reflect.DeepEqual(stored.Labels, want) {
		t.Errorf("labels = %v, want %v", stored.Labels, want)
	}
	// The template is removed as soon as the sandbox exists.
	if _, err := client.SandboxTemplates().Get(ctx, DefaultWorkspace, ref.Name); !v1.IsNotFound(err) {
		t.Errorf("Create left the workload template: %v", err)
	}
}

func TestWorkloadTemplateCarriesImageEnvironmentMountsAndLimits(t *testing.T) {
	req := testRequest()
	spec, err := sandboxSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	template, err := workloadTemplate(req, spec)
	if err != nil {
		t.Fatal(err)
	}
	workload := template.Spec.Workload
	if template.Name != req.Name || workload.Image != "worker@sha256:deadbeef" || workload.Resources.CPU != "2" || workload.Resources.Memory != "4294967296" {
		t.Errorf("template %q, workload %+v, resources %+v; want the image with 2 CPUs and 4294967296 bytes", template.Name, workload, workload.Resources)
	}
	if want := map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "FACTORY_MODEL_ID": "m"}; !reflect.DeepEqual(workload.Environment, want) {
		t.Errorf("workload environment = %v, want %v", workload.Environment, want)
	}
	mounts, _ := template.Spec.DriverConfig["docker"].(map[string]any)["mounts"].([]any)
	if len(mounts) != 3 {
		t.Errorf("template mounts = %v, want the request's three", mounts)
	}
	// A sandbox created from a template may set only policy, providers,
	// command and tty.
	if spec.Template != nil || spec.Environment != nil || spec.Policy == nil || len(spec.Command) == 0 {
		t.Errorf("spec after the template took its part = %+v", spec)
	}
}

func TestMemoryQuantity(t *testing.T) {
	for value, want := range map[string]string{"4g": "4294967296", "512m": "536870912", "64K": "65536", "1048576": "1048576", "7b": "7"} {
		if got, err := memoryQuantity(value); err != nil || got != want {
			t.Errorf("memoryQuantity(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"", "0", "4Gi", "1.5g", "-1g", "g"} {
		if got, err := memoryQuantity(value); err == nil {
			t.Errorf("memoryQuantity(%q) = %q, want an error", value, got)
		}
	}
}

func TestStatusReportsTheWorkersStart(t *testing.T) {
	rt, _, containers := newTestRuntime()
	ctx := context.Background()
	if state, err := rt.Status(ctx, "bg-absent"); err != nil || state != (sandbox.SandboxState{}) {
		t.Fatalf("an unknown sandbox: %+v, %v", state, err)
	}
	ref, err := rt.Create(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	containers.startedAt[ref.Name] = "2026-10-04T10:00:00.5Z"
	state, err := rt.Status(ctx, ref.Name)
	if err != nil || state != (sandbox.SandboxState{Present: true, StartedAt: "2026-10-04T10:00:00.5Z"}) {
		t.Fatalf("state = %+v, %v", state, err)
	}
}

func TestStatusSeesAContainerTheGatewayForgot(t *testing.T) {
	rt, _, containers := newTestRuntime()
	containers.startedAt["bg-orphan"] = "2026-10-04T10:00:00Z"
	state, err := rt.Status(context.Background(), "bg-orphan")
	if err != nil || !state.Present {
		t.Fatalf("state = %+v, %v; want present", state, err)
	}
}

func TestDeleteRemovesTheSandboxAndItsContainers(t *testing.T) {
	rt, client, containers := newTestRuntime()
	ctx := context.Background()
	ref, err := rt.Create(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	containers.startedAt[ref.Name] = "t"
	if err := rt.Delete(ctx, ref.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Sandboxes().Get(ctx, DefaultWorkspace, ref.Name); !v1.IsNotFound(err) {
		t.Errorf("the gateway still has the sandbox: %v", err)
	}
	if !reflect.DeepEqual(containers.removed, []string{ref.Name}) {
		t.Errorf("removed %v", containers.removed)
	}
	if err := rt.Delete(ctx, ref.Name); err != nil {
		t.Errorf("deleting a sandbox that is gone: %v", err)
	}
}

func TestDeleteIsUnconfirmedWhileAContainerIsLeft(t *testing.T) {
	rt, _, containers := newTestRuntime()
	containers.startedAt["bg-stuck"] = "t"
	containers.sticky = true
	err := rt.Delete(context.Background(), "bg-stuck")
	if !errors.Is(err, sandbox.ErrCleanupUnconfirmed) {
		t.Fatalf("err = %v, want ErrCleanupUnconfirmed", err)
	}
	containers.sticky, containers.listErr = false, errors.New("docker unreachable")
	if err := rt.Delete(context.Background(), "bg-stuck"); !errors.Is(err, sandbox.ErrCleanupUnconfirmed) {
		t.Fatalf("with Docker unreachable: err = %v, want ErrCleanupUnconfirmed", err)
	}
}

func TestListByRunReturnsRecordedSandboxesThatStillExist(t *testing.T) {
	rt, _, containers := newTestRuntime()
	ctx := context.Background()
	dataDir := t.TempDir()
	req := testRequest()
	req.DataDir = dataDir
	for _, name := range []string{req.Name, "bg-gone", "bg-container-only"} {
		if err := sandbox.RecordSandbox(dataDir, "run1", sandbox.SandboxRecord{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	containers.startedAt["bg-container-only"] = "t"
	containers.startedAt["bg-another-runs"] = "t"
	names, err := rt.ListByRun(ctx, dataDir, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{req.Name, "bg-container-only"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("ListByRun = %v, want %v", names, want)
	}
	containers.listErr = errors.New("docker unreachable")
	if names, err := rt.ListByRun(ctx, dataDir, "run1"); err == nil {
		t.Fatalf("with Docker unreachable: %v, nil; want an error", names)
	}
}

func chatGPTCredential(t *testing.T, token string, expires time.Time) sandbox.RouteCredential {
	t.Helper()
	p := sandbox.RoutePolicy{
		Upstream:          meter.ChatGPTCodexAPIBase,
		AllowedPathPrefix: meter.ChatGPTCodexResponsesPath, AuthMode: meter.CredentialModeChatGPTCodex,
		UsageFormat: meter.UsageFormatOpenAIResponses, WorkerModelID: "m", WorkerModelAPI: meter.RequestFormatOpenAIResponses,
		Route:           "chatgpt",
		MaxRequestBytes: 1 << 20, RequestsPerMinute: 60,
		TokenBudget: 10000, TokenBudgetWindow: time.Minute, CostBudgetMicroUSD: 100000, CostBudgetWindow: time.Minute,
		TokenCeiling: 50000, CostCeilingMicroUSD: 500000,
	}
	access, err := p.RouteAccess(t.TempDir(), []string{"/usr/local/bin/node"})
	if err != nil {
		t.Fatal(err)
	}
	access.Provider = "bg-12345678-chatgpt"
	cred, err := p.RouteCredential(access, sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.NewRouteSecret(token), sandbox.NewRouteSecret("acct"), expires, expires.Add(-10*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestPushCredentialCreatesThenReplacesTheProvidersValues(t *testing.T) {
	rt, client, _ := newTestRuntime()
	ctx := context.Background()
	expires := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if err := rt.PushCredential(ctx, chatGPTCredential(t, "tok-1", expires)); err != nil {
		t.Fatal(err)
	}
	provider, err := client.Providers().Get(ctx, DefaultWorkspace, "bg-12345678-chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Type != "buildgate-bg-chatgpt-account-bg-chatgpt-token" {
		t.Errorf("provider type = %q", provider.Type)
	}
	if _, imported := client.profiles.imported[provider.Type]; !imported || len(client.profiles.imported) != 1 {
		t.Errorf("imported profiles = %v, want the provider's one", client.profiles.imported)
	}
	if want := map[string]string{"BG_CHATGPT_TOKEN": "tok-1", "BG_CHATGPT_ACCOUNT": "acct"}; !reflect.DeepEqual(provider.Spec.Credentials, want) {
		t.Error("the provider holds the wrong credential names or values")
	}
	if got := provider.Spec.CredentialExpiresAt["BG_CHATGPT_TOKEN"]; !got.Equal(expires) {
		t.Errorf("expiry = %v, want %v", got, expires)
	}

	if err := rt.PushCredential(ctx, chatGPTCredential(t, "tok-2", expires.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	provider, err = client.Providers().Get(ctx, DefaultWorkspace, "bg-12345678-chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Spec.Credentials["BG_CHATGPT_TOKEN"] != "tok-2" {
		t.Error("a second push did not replace the token")
	}
	all, err := client.Providers().ListAll(ctx, DefaultWorkspace)
	if err != nil || len(all) != 1 {
		t.Errorf("providers = %d, %v; want one", len(all), err)
	}
}

func TestPushCredentialRefusesAnEmptyCredential(t *testing.T) {
	rt, _, _ := newTestRuntime()
	if err := rt.PushCredential(context.Background(), sandbox.RouteCredential{Provider: "p"}); err == nil {
		t.Error("a credential with no values was accepted")
	}
}

// exitingSandboxes answers Get with a sandbox that is running for the first
// calls and then has the given status.
type exitingSandboxes struct {
	v1.SandboxInterface
	running int
	final   v1.SandboxStatus
	getErr  error
}

func (e *exitingSandboxes) Get(context.Context, string, string) (*v1.Sandbox, error) {
	if e.getErr != nil {
		return nil, e.getErr
	}
	if e.running > 0 {
		e.running--
		return &v1.Sandbox{Status: v1.SandboxStatus{Phase: v1.SandboxReady}}, nil
	}
	return &v1.Sandbox{Status: e.final}, nil
}

type clientWithSandboxes struct {
	v1.ClientInterface
	sandboxes v1.SandboxInterface
}

func (c clientWithSandboxes) Sandboxes() v1.SandboxInterface { return c.sandboxes }

func waitRuntime(s *exitingSandboxes) *Runtime {
	return &Runtime{Client: clientWithSandboxes{sandboxes: s}, PollEvery: time.Millisecond}
}

func TestWaitReturnsTheExitCodeOnceTheCommandEnds(t *testing.T) {
	code := int32(7)
	exit, err := waitRuntime(&exitingSandboxes{running: 3, final: v1.SandboxStatus{Phase: v1.SandboxCompleted, ExitCode: &code}}).Wait(context.Background(), "n")
	if err != nil || exit != (sandbox.SandboxExit{ExitCode: 7, Phase: "Completed"}) {
		t.Fatalf("exit = %+v, %v", exit, err)
	}
}

func TestWaitFails(t *testing.T) {
	if _, err := waitRuntime(&exitingSandboxes{final: v1.SandboxStatus{Phase: v1.SandboxError}}).Wait(context.Background(), "n"); err == nil || !strings.Contains(err.Error(), "no exit code") {
		t.Errorf("a sandbox that ended with no exit code: err = %v", err)
	}
	if _, err := waitRuntime(&exitingSandboxes{getErr: errors.New("gateway unreachable")}).Wait(context.Background(), "n"); err == nil {
		t.Error("an unreachable gateway was not an error")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := waitRuntime(&exitingSandboxes{running: 1 << 30}).Wait(ctx, "n"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a cancelled wait: err = %v, want the context's", err)
	}
}

func TestSandboxSpecAdmitsSidecarsByAddressOutsideTheMeter(t *testing.T) {
	req := testRequest()
	req.Route = testRoute()
	req.MeterConfig = map[string]any{"run": "r"}
	req.Sidecars = []sandbox.SidecarEndpoint{
		{Name: "registry-proxy", IP: "172.28.0.2", Ports: []int{8092}},
		{Name: "db", IP: "172.29.0.5", Ports: []int{5432, 5433}},
	}
	spec, err := sandboxSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	want := v1.NetworkPolicyRule{
		Name:     "sidecars",
		Binaries: []v1.PolicyNetworkBinary{{Path: "/**"}},
		Endpoints: []v1.PolicyNetworkEndpoint{
			{AllowedIPs: []string{"172.28.0.2"}, Ports: []uint32{8092}, TLS: v1.NetworkTLSModeSkip, Enforcement: v1.NetworkEnforcementModeEnforce},
			{AllowedIPs: []string{"172.29.0.5"}, Ports: []uint32{5432, 5433}, TLS: v1.NetworkTLSModeSkip, Enforcement: v1.NetworkEnforcementModeEnforce},
		},
	}
	if got := spec.Policy.NetworkPolicies["sidecars"]; !reflect.DeepEqual(got, want) {
		t.Errorf("sidecars rule =\n%+v\nwant\n%+v", got, want)
	}
	if _, ok := spec.Policy.NetworkPolicies["model"]; !ok || len(spec.Policy.NetworkPolicies) != 2 {
		t.Errorf("rules = %v, want the model rule and the sidecars rule", spec.Policy.NetworkPolicies)
	}
	if include := spec.Policy.NetworkMiddlewares["meter"].Endpoints.Include; !reflect.DeepEqual(include, []string{"chatgpt.com"}) {
		t.Errorf("the meter is in front of %v, want the model host only", include)
	}
}

func TestSandboxSpecAdmitsSidecarsForAStepWithNoRoute(t *testing.T) {
	req := testRequest()
	req.Sidecars = []sandbox.SidecarEndpoint{{Name: "db", IP: "172.29.0.5", Ports: []int{5432}}}
	spec, err := sandboxSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Policy.NetworkPolicies) != 1 || spec.Policy.NetworkMiddlewares != nil {
		t.Errorf("rules = %v, middlewares = %v; want only the sidecars rule", spec.Policy.NetworkPolicies, spec.Policy.NetworkMiddlewares)
	}
}

func TestSandboxSpecRefusesASidecarItCannotAddress(t *testing.T) {
	for name, sidecar := range map[string]sandbox.SidecarEndpoint{
		"no address":   {Name: "db", Ports: []int{5432}},
		"no port":      {Name: "db", IP: "172.29.0.5"},
		"a port of 0":  {Name: "db", IP: "172.29.0.5", Ports: []int{0}},
		"a port 70000": {Name: "db", IP: "172.29.0.5", Ports: []int{70000}},
	} {
		req := testRequest()
		req.Sidecars = []sandbox.SidecarEndpoint{sidecar}
		if _, err := sandboxSpec(req); err == nil {
			t.Errorf("a sidecar with %s was accepted", name)
		}
	}
}

// fakeReadiness answers ProviderInstalled from a script, then with its last
// answer.
type fakeReadiness struct {
	answers []bool
	err     error
	asked   []string
}

func (f *fakeReadiness) ProviderInstalled(_ context.Context, workspace, sandboxName, provider string) (bool, error) {
	f.asked = append(f.asked, workspace+"/"+sandboxName+"/"+provider)
	if f.err != nil {
		return false, f.err
	}
	answer := f.answers[min(len(f.asked), len(f.answers))-1]
	return answer, nil
}

func routedRequest() sandbox.SandboxRequest {
	req := testRequest()
	req.Route = testRoute()
	req.MeterConfig = map[string]any{"route": "chatgpt"}
	return req
}

func TestCreateReturnsARoutedSandboxOnlyOnceItsCredentialIsInstalled(t *testing.T) {
	rt, _, _ := newTestRuntime()
	readiness := &fakeReadiness{answers: []bool{false, false, true}}
	rt.Readiness = readiness
	if _, err := rt.Create(context.Background(), routedRequest()); err != nil {
		t.Fatal(err)
	}
	want := "default/bg-0123456789abcdef/bg-12345678-chatgpt"
	if len(readiness.asked) != 3 || readiness.asked[0] != want {
		t.Errorf("asked = %v, want three times %q", readiness.asked, want)
	}
}

func TestCreateFailsWhenTheRouteCredentialIsNotInstalled(t *testing.T) {
	for name, tc := range map[string]struct {
		readiness RouteReadiness
		want      string
	}{
		"refused":       {&fakeReadiness{err: errors.New("the gateway reports provider p as FAILED")}, "FAILED"},
		"never":         {&fakeReadiness{answers: []bool{false}}, "did not report its model route's credential installed"},
		"not connected": {nil, "no route readiness client"},
	} {
		t.Run(name, func(t *testing.T) {
			rt, _, _ := newTestRuntime()
			rt.Readiness = tc.readiness
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err := rt.Create(ctx, routedRequest())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestCreateAsksNothingForASandboxWithNoRouteCredential(t *testing.T) {
	for name, route := range map[string]*sandbox.RouteAccess{"no route": nil, "no credential": {Endpoint: testRoute().Endpoint}} {
		t.Run(name, func(t *testing.T) {
			rt, _, _ := newTestRuntime()
			req := testRequest()
			if req.Route = route; route != nil {
				req.MeterConfig = map[string]any{"route": "local"}
			}
			if _, err := rt.Create(context.Background(), req); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The supervisor closes every connection through its proxy at its first
// settings poll, so a sandbox that reaches the network is handed back only
// after it: by the provider's readiness when there is one, by time when
// there is none.
func TestCreateHoldsANetworkedSandboxWithNoCredentialUntilTheFirstSettingsPoll(t *testing.T) {
	sidecar := []sandbox.SidecarEndpoint{{Name: "registry-proxy", IP: "172.29.0.5", Ports: []int{8092}}}
	for name, tc := range map[string]struct {
		route    *sandbox.RouteAccess
		sidecars []sandbox.SidecarEndpoint
		want     []time.Duration
	}{
		"no network":                 {},
		"a sidecar":                  {sidecars: sidecar, want: []time.Duration{firstSettingsPollWait}},
		"a route with no credential": {route: &sandbox.RouteAccess{Endpoint: testRoute().Endpoint}, want: []time.Duration{firstSettingsPollWait}},
		"a credentialed route":       {route: testRoute(), sidecars: sidecar},
	} {
		t.Run(name, func(t *testing.T) {
			rt, _, _ := newTestRuntime()
			rt.Readiness = &fakeReadiness{answers: []bool{true}}
			var slept []time.Duration
			rt.Sleep = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}
			req := testRequest()
			req.Sidecars = tc.sidecars
			if req.Route = tc.route; tc.route != nil {
				req.MeterConfig = map[string]any{"route": "r"}
			}
			if _, err := rt.Create(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(slept, tc.want) {
				t.Errorf("slept = %v, want %v", slept, tc.want)
			}
		})
	}
}

func TestCreateFailsWhenTheHoldForTheFirstSettingsPollIsCancelled(t *testing.T) {
	rt, _, _ := newTestRuntime()
	rt.Sleep = nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := testRequest()
	req.Sidecars = []sandbox.SidecarEndpoint{{Name: "registry-proxy", IP: "172.29.0.5", Ports: []int{8092}}}
	_, err := rt.Create(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "first settings poll") {
		t.Fatalf("Create = %v, want an error naming the first settings poll", err)
	}
}

func TestProviderInstalledReadsTheGatewaysState(t *testing.T) {
	for state, want := range map[pb.ProviderReadinessState]struct{ installed, refused bool }{
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_READY:       {installed: true},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PENDING:     {},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_PERSISTED:   {},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_UNSPECIFIED: {},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_FAILED:      {refused: true},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_WITHHELD:    {refused: true},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_REVOKED:     {refused: true},
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_SUPERSEDED:  {refused: true},
	} {
		installed, err := providerInstalled("p", &pb.ProviderReadinessStatus{State: state})
		if installed != want.installed || (err != nil) != want.refused {
			t.Errorf("%s: installed = %v, err = %v", state, installed, err)
		}
	}
}
