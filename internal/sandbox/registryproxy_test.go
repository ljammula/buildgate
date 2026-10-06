package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

func validRegistryProxyPolicy() RegistryProxyPolicy {
	return RegistryProxyPolicy{
		Image:                 "registry.example/registry-proxy@sha256:" + strings.Repeat("b", 64),
		Routes:                DefaultRegistryProxyRoutes(),
		CacheBytes:            1 << 30,
		MaxObjectBytes:        256 << 20,
		MaxConcurrentUpstream: 16,
		UpstreamTimeout:       60 * time.Second,
	}
}

func validRegistryProxySpec() RegistryProxySpec {
	return validRegistryProxyPolicy().Spec("run1", "/tmp/data")
}

func TestRegistryProxySpecRejectsUnsafeConfiguration(t *testing.T) {
	checks := []struct {
		name string
		edit func(*RegistryProxySpec)
	}{
		{"missing image", func(s *RegistryProxySpec) { s.Image = "" }},
		{"mutable image", func(s *RegistryProxySpec) { s.Image = "registry-proxy:latest" }},
		{"short digest", func(s *RegistryProxySpec) { s.Image = "registry-proxy@sha256:deadbeef" }},
		{"no routes", func(s *RegistryProxySpec) { s.Routes = nil }},
		{"bad prefix missing leading slash", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "npm/", Upstream: "https://registry.npmjs.org"}}
		}},
		{"bad prefix missing trailing slash", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "/npm", Upstream: "https://registry.npmjs.org"}}
		}},
		{"duplicate prefix", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{
				{Prefix: "/npm/", Upstream: "https://registry.npmjs.org"},
				{Prefix: "/npm/", Upstream: "https://mirror.example"},
			}
		}},
		{"upstream with credentials", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "/npm/", Upstream: "https://user:pass@registry.npmjs.org"}}
		}},
		{"upstream with query", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "/npm/", Upstream: "https://registry.npmjs.org?x=1"}}
		}},
		{"non-http upstream scheme", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "/npm/", Upstream: "ftp://registry.npmjs.org"}}
		}},
		{"relative upstream", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "/npm/", Upstream: "/registry.npmjs.org"}}
		}},
		{"unsafe character in allowed host", func(s *RegistryProxySpec) {
			s.Routes = []RegistryProxyRoute{{Prefix: "/pypi/", Upstream: "https://pypi.org/simple", AllowedHosts: []string{"evil\n.example"}}}
		}},
		{"zero cache bytes", func(s *RegistryProxySpec) { s.CacheBytes = 0 }},
		{"object bytes exceed cache bytes", func(s *RegistryProxySpec) { s.MaxObjectBytes = s.CacheBytes + 1 }},
		{"zero max object bytes", func(s *RegistryProxySpec) { s.MaxObjectBytes = 0 }},
		{"zero max concurrent upstream", func(s *RegistryProxySpec) { s.MaxConcurrentUpstream = 0 }},
		{"zero upstream timeout", func(s *RegistryProxySpec) { s.UpstreamTimeout = 0 }},
		{"run id newline", func(s *RegistryProxySpec) { s.RunID = "run\n123" }},
		{"missing run id", func(s *RegistryProxySpec) { s.RunID = "" }},
		{"missing data dir", func(s *RegistryProxySpec) { s.DataDir = "" }},
		{"relative data dir", func(s *RegistryProxySpec) { s.DataDir = "relative/data" }},
		{"egress CA bundle does not exist", func(s *RegistryProxySpec) { s.CABundlePath = "/nonexistent/corp-ca.pem" }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			s := validRegistryProxySpec()
			tc.edit(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}

func TestRegistryProxySpecAcceptsDefaultConfiguration(t *testing.T) {
	if err := validRegistryProxySpec().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// fakeRegistryProxyDocker mirrors fakeRelayDocker exactly (see its own doc
// comment) with the registry proxy's own operations: no credential env var
// to capture, and a different readiness log line.
func fakeRegistryProxyDocker(t *testing.T, logPath string, leaveContainer bool) string {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker-fake")
	leave := ""
	if leaveContainer {
		leave = "printf 'container-still-present\\n'"
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "network create") printf 'network-id-123\n' ;;
  "run --detach") printf 'container-id-123\n' ;;
  "inspect --type") printf 'true\n' ;;
  "logs "*) printf 'serving registry proxy on :8092\n' ;;
  "ps -a") %s ;;
  "network ls") ;;
esac
`, logPath, leave)
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker
}

func TestLaunchRegistryProxyAndCleanupUseExactRestrictedLifecycle(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker-calls")
	docker := fakeRegistryProxyDocker(t, logPath, false)

	spec := validRegistryProxySpec()
	handle, err := LaunchRegistryProxy(context.Background(), docker, spec)
	if err != nil {
		t.Fatalf("LaunchRegistryProxy: %v", err)
	}
	if got, want := handle.WorkerBaseURL, "http://registry-proxy:8092"; got != want {
		t.Fatalf("WorkerBaseURL = %q, want %q", got, want)
	}
	facts := handle.Facts()
	if facts.ImageDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("ImageDigest = %q, want digest", facts.ImageDigest)
	}
	if !strings.HasPrefix(facts.NetworkName, "factoryd-registryproxy-") {
		t.Errorf("NetworkName = %q, want factoryd-registryproxy- prefix", facts.NetworkName)
	}
	if facts.NetworkID != "network-id-123" || facts.ContainerID != "container-id-123" {
		t.Errorf("facts IDs = network %q/container %q, want fake IDs", facts.NetworkID, facts.ContainerID)
	}
	wantHosts := map[string]bool{"registry.npmjs.org": true, "pypi.org": true, "files.pythonhosted.org": true, "sum.golang.org": true, "proxy.golang.org": true, "storage.googleapis.com": true}
	for _, h := range facts.Hosts {
		if !wantHosts[h] {
			t.Errorf("unexpected audited host %q", h)
		}
	}

	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	calls := readRelayCalls(t, logPath)
	wantOps := []string{
		"network ls",     // ensureRegistryProxyEgressNetwork: is it already there?
		"network create", // ensureRegistryProxyEgressNetwork: no, so create it
		"network create", // the per-run --internal network
		"run",
		"network connect",
		"inspect",
		"logs", // waitRegistryProxyListening
		"rm",
		"ps",
		"network rm",
		"network ls",
	}
	if len(calls) != len(wantOps) {
		t.Fatalf("Docker operation count = %d, calls = %v", len(calls), calls)
	}
	for i, want := range wantOps {
		if got := relayCallOperation(calls[i]); got != want {
			t.Errorf("Docker call %d operation = %q, want %q (full call %v)", i, got, want, calls[i])
		}
	}
	if !strings.Contains(strings.Join(calls[1], " "), registryProxyEgressNetworkName) {
		t.Errorf("egress network create missing its name: %v", calls[1])
	}
	if !strings.Contains(strings.Join(calls[2], " "), "--internal") {
		t.Errorf("network create missing --internal: %v", calls[2])
	}
	if !strings.Contains(strings.Join(calls[3], " "), "--network "+registryProxyEgressNetworkName) {
		t.Errorf("registry proxy start missing egress network, not the ordinary bridge: %v", calls[3])
	}
	if !strings.Contains(strings.Join(calls[4], " "), "--alias registry-proxy") {
		t.Errorf("network connect missing stable alias: %v", calls[4])
	}
	if !strings.Contains(strings.Join(calls[3], " "), "--user 65532:65532") {
		t.Errorf("registry proxy start missing explicit non-root --user: %v", calls[3])
	}
	if !strings.Contains(strings.Join(calls[3], " "), "--read-only") || !strings.Contains(strings.Join(calls[3], " "), "--cap-drop=ALL") {
		t.Errorf("registry proxy start missing read-only/cap-drop hardening: %v", calls[3])
	}
	if !strings.Contains(strings.Join(calls[3], " "), "-route /npm/=https://registry.npmjs.org") {
		t.Errorf("registry proxy start missing npm route: %v", calls[3])
	}
	if !strings.Contains(strings.Join(calls[3], " "), "-route /pypi/=https://pypi.org/simple,files.pythonhosted.org->/pypi-files/") {
		t.Errorf("registry proxy start missing pypi route with allowed redirect + rewrite host: %v", calls[3])
	}
	if !strings.Contains(strings.Join(calls[3], " "), "-route /pypi-files/=https://files.pythonhosted.org") {
		t.Errorf("registry proxy start missing pypi-files route: %v", calls[3])
	}
}

// TestLaunchRegistryProxyMountsAndTrustsEgressCABundleWhenSet: the mount/env pair
// must reach the registry-proxy container's own `docker run` argv when
// spec.CABundlePath is set, the mounted source must be a staged 0644 copy
// rather than the operator's own path, and Cleanup must remove it.
func TestLaunchRegistryProxyMountsAndTrustsEgressCABundleWhenSet(t *testing.T) {
	bundle := generateTestCABundle(t)
	if err := os.Chmod(bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "docker-calls")
	docker := fakeRegistryProxyDocker(t, logPath, false)

	spec := validRegistryProxySpec()
	spec.CABundlePath = bundle
	handle, err := LaunchRegistryProxy(context.Background(), docker, spec)
	if err != nil {
		t.Fatalf("LaunchRegistryProxy: %v", err)
	}

	calls := readRelayCalls(t, logPath)
	var runCall []string
	for _, call := range calls {
		if relayCallOperation(call) == "run" {
			runCall = call
		}
	}
	joined := strings.Join(runCall, " ")
	if strings.Contains(joined, "--volume "+bundle+":") {
		t.Errorf("registry proxy run mounted the operator's own path %q directly, want a staged copy", bundle)
	}
	if !strings.Contains(joined, "--env FACTORYD_EGRESS_CA_BUNDLE="+EgressCABundleContainerPath) {
		t.Errorf("registry proxy run missing FACTORYD_EGRESS_CA_BUNDLE env: %v", runCall)
	}
	var stagedPath string
	suffix := ":" + EgressCABundleContainerPath + ":ro"
	for i, a := range runCall {
		if a == "--volume" && i+1 < len(runCall) && strings.HasSuffix(runCall[i+1], suffix) {
			stagedPath = strings.TrimSuffix(runCall[i+1], suffix)
		}
	}
	if stagedPath == "" {
		t.Fatalf("could not find the egress CA bundle --volume argument: %v", runCall)
	}
	info, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("stat staged bundle copy: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("staged bundle copy mode = %v, want 0644", info.Mode().Perm())
	}

	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Errorf("staged bundle copy still present after Cleanup (err=%v), want it removed", err)
	}
}

// TestLaunchRegistryProxyOmitsEgressCABundleWhenUnset guards the default
// (no -egress-ca-bundle configured) case.
func TestLaunchRegistryProxyOmitsEgressCABundleWhenUnset(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker-calls")
	docker := fakeRegistryProxyDocker(t, logPath, false)

	handle, err := LaunchRegistryProxy(context.Background(), docker, validRegistryProxySpec())
	if err != nil {
		t.Fatalf("LaunchRegistryProxy: %v", err)
	}
	defer handle.Cleanup(context.Background())

	calls := readRelayCalls(t, logPath)
	for _, call := range calls {
		if relayCallOperation(call) != "run" {
			continue
		}
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "FACTORYD_EGRESS_CA_BUNDLE") || strings.Contains(joined, EgressCABundleContainerPath) {
			t.Errorf("registry proxy run unexpectedly mounted/trusted an egress CA bundle with none configured: %v", call)
		}
	}
}

func TestLaunchRegistryProxyRejectsBeforeDocker(t *testing.T) {
	invalid := validRegistryProxySpec()
	invalid.Routes = nil
	if _, err := LaunchRegistryProxy(context.Background(), "docker-should-never-run", invalid); err == nil {
		t.Fatal("LaunchRegistryProxy unexpectedly succeeded with an invalid spec")
	}
}

func TestLaunchRegistryProxyFailsClosedWhenContainerRemains(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "docker-calls")
	docker := fakeRegistryProxyDocker(t, logPath, true) // "rm" reports the container still present

	handle, err := LaunchRegistryProxy(context.Background(), docker, validRegistryProxySpec())
	if err != nil {
		t.Fatalf("LaunchRegistryProxy: %v", err)
	}
	if err := handle.Cleanup(context.Background()); err == nil {
		t.Fatal("Cleanup unexpectedly succeeded with the container still present")
	}
}

// TestReconcileRelayOrphansCoversRegistryProxy is the registry-proxy
// counterpart of TestReconcileRelayOrphansRemovesTerminalRunsAndOrphanedNetworks,
// proving the SAME reconciliation pass (not a second one) reaps a leaked
// registry-proxy container/network exactly like a relay's.
func TestReconcileRelayOrphansCoversRegistryProxy(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-proxy-terminal", State: run.StateHalted, HaltConfirmed: true}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.registryproxy=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-registryproxy-container-terminal\trun-proxy-terminal\n'
    ;;
  *"ps -a --filter name=^/"*)
    ;;
  *"network ls --filter label=buildgate.registryproxy=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-registryproxy-terminal\trun-proxy-terminal\n'
    ;;
  *"network ls --filter name=^"*)
    ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	want := map[string]bool{
		"factoryd-registryproxy-container-terminal": true,
		"factoryd-registryproxy-terminal":           true,
	}
	if len(removed) != len(want) {
		t.Fatalf("removed = %v, want exactly %v", removed, want)
	}
	for _, name := range removed {
		if !want[name] {
			t.Errorf("unexpectedly removed %q", name)
		}
	}
}

func TestRegistryProxyLifecyclePrepareWorkerSetsNetworkAndEnv(t *testing.T) {
	spec := validRegistryProxySpec()
	fakeHandle := &RegistryProxyHandle{
		WorkerBaseURL: "http://registry-proxy:8092",
		AuditFacts: RegistryProxyLaunchFacts{
			NetworkName:   "factoryd-registryproxy-abc",
			WorkerBaseURL: "http://registry-proxy:8092",
		},
	}
	hooks := RegistryProxyHooks{
		StartRegistryProxy: func(ctx context.Context, docker string, s RegistryProxySpec) (*RegistryProxyHandle, error) {
			return fakeHandle, nil
		},
		CleanupRegistryProxy: func(ctx context.Context, h *RegistryProxyHandle) error { return nil },
	}
	dataDir := t.TempDir()
	spec.DataDir = dataDir
	lifecycle, err := BeginRegistryProxyLifecycle(spec, "docker", spec.RunID, dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginRegistryProxyLifecycle: %v", err)
	}
	if err := lifecycle.Ensure(context.Background(), ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	worker := LaunchSpec{Name: "factoryd-worker-1", Environment: []string{"npm_config_registry=http://evil", "GOMODCACHE=/usr/local/gomodcache", "GOFLAGS=-tags=enterprise", "PATH=/usr/bin"}, DataDir: dataDir, RunID: spec.RunID, User: fmt.Sprintf("65532:%d", os.Getgid())}
	worker, err = lifecycle.PrepareWorker(worker)
	if err != nil {
		t.Fatalf("PrepareWorker: %v", err)
	}
	// The Go route only works with a writable module cache: the image's
	// baked GOMODCACHE is read-only, so it must be replaced, not kept,
	// and backed by this container's own scratch directory (found live
	// 2026-09-10; per launch since the 2026-09-29 demo, see RunScratchDir).
	if want := filepath.Join(RunScratchDir(dataDir, spec.RunID), "factoryd-worker-1"); worker.ScratchDir != want {
		t.Errorf("ScratchDir = %q, want %q", worker.ScratchDir, want)
	}
	if info, statErr := os.Stat(worker.ScratchDir); statErr != nil {
		t.Errorf("scratch dir not created: %v", statErr)
	} else if info.Mode().Perm()&0o070 != 0o070 {
		t.Errorf("scratch dir mode = %v, want group rwx for the worker's host gid", info.Mode().Perm())
	}
	for _, value := range worker.Environment {
		if value == "GOMODCACHE=/usr/local/gomodcache" {
			t.Errorf("Environment still carries the image's read-only GOMODCACHE: %v", worker.Environment)
		}
	}
	joined := strings.Join(worker.Environment, "\n")
	for _, want := range []string{"GOMODCACHE=" + WorkerScratchMount + "/gomodcache", "GOCACHE=" + WorkerScratchMount + "/go-build", "GOFLAGS=-tags=enterprise -modcacherw"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Environment = %v, want %s", worker.Environment, want)
		}
	}
	if worker.Network != "factoryd-registryproxy-abc" {
		t.Errorf("Network = %q, want the registry proxy's own network", worker.Network)
	}
	joined = strings.Join(worker.Environment, "\n")
	if !strings.Contains(joined, "npm_config_registry=http://registry-proxy:8092/npm/") {
		t.Errorf("environment missing npm_config_registry, got %v", worker.Environment)
	}
	if strings.Contains(joined, "http://evil") {
		t.Errorf("stale npm_config_registry not replaced, got %v", worker.Environment)
	}
	if !strings.Contains(joined, "PIP_INDEX_URL=http://registry-proxy:8092/pypi/") {
		t.Errorf("environment missing PIP_INDEX_URL, got %v", worker.Environment)
	}
	if !strings.Contains(joined, "GOPROXY=http://registry-proxy:8092/gomodproxy") {
		t.Errorf("environment missing GOPROXY, got %v", worker.Environment)
	}
	if !strings.Contains(joined, "GOSUMDB=sum.golang.org") {
		t.Errorf("environment missing GOSUMDB=sum.golang.org, got %v", worker.Environment)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("unrelated environment entry dropped, got %v", worker.Environment)
	}
	if err := lifecycle.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

func TestRegistryProxyLifecyclePrepareWorkerRejectsUnsafeNetwork(t *testing.T) {
	spec := validRegistryProxySpec()
	dataDir := t.TempDir()
	spec.DataDir = dataDir
	hooks := RegistryProxyHooks{
		StartRegistryProxy: func(ctx context.Context, docker string, s RegistryProxySpec) (*RegistryProxyHandle, error) {
			return &RegistryProxyHandle{
				WorkerBaseURL: "http://registry-proxy:8092",
				AuditFacts: RegistryProxyLaunchFacts{
					NetworkName:   registryProxyEgressNetworkName, // the real internet-routable egress network
					WorkerBaseURL: "http://registry-proxy:8092",
				},
			}, nil
		},
	}
	lifecycle, err := BeginRegistryProxyLifecycle(spec, "docker", spec.RunID, dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginRegistryProxyLifecycle: %v", err)
	}
	if err := lifecycle.Ensure(context.Background(), ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := lifecycle.PrepareWorker(LaunchSpec{}); err == nil {
		t.Fatal("PrepareWorker unexpectedly accepted the egress network as the worker's own network")
	}
}

// TestDefaultGoProxyRouteAllowsStorageRedirect pins the redirect host a
// real proxy.golang.org large-zip download lands on (found live
// 2026-09-10): without it the proxy refuses the redirect and the worker
// sees a 502 for exactly the modules big enough to be served that way.
func TestDefaultGoProxyRouteAllowsStorageRedirect(t *testing.T) {
	for _, route := range DefaultRegistryProxyRoutes() {
		if route.Prefix != goProxyRoutePrefix {
			continue
		}
		for _, host := range route.AllowedHosts {
			if host == "storage.googleapis.com" {
				return
			}
		}
		t.Fatalf("go proxy route AllowedHosts = %v, want storage.googleapis.com", route.AllowedHosts)
	}
	t.Fatal("no go proxy route in DefaultRegistryProxyRoutes")
}

// TestRemoveScratchDirDeletesReadOnlyModuleCache: the go command extracts
// modules as 0444 files inside 0555 directories, which a plain RemoveAll
// cannot delete (found live 2026-09-10). The cleanup must succeed anyway.
func TestRemoveScratchDirDeletesReadOnlyModuleCache(t *testing.T) {
	dataDir := t.TempDir()
	dir, err := EnsureScratchDir(dataDir, "run-1", "worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "gomodcache", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "a.go"), []byte("package m\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mod, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err == nil {
		t.Skip("this platform lets RemoveAll delete a read-only directory tree; nothing to prove here")
	}
	if err := RemoveScratchDir(dataDir, "run-1"); err != nil {
		t.Fatalf("RemoveScratchDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir still exists after RemoveScratchDir: %v", err)
	}
	if err := RemoveScratchDir(dataDir, "run-1"); err != nil {
		t.Fatalf("second RemoveScratchDir on a missing dir: %v", err)
	}
}

// TestRegistryProxyRoutesWithUpstreamsKeepsDefaultsAndDropsForMirrors pins
// the one route table: default upstreams reproduce DefaultRegistryProxyRoutes
// exactly (redirect allowlists included -- cmd/factoryd's own hand-written
// copy used to drop them), and a mirror keeps only the PyPI rewrite, whose
// host follows the pypi-files upstream.
func TestRegistryProxyRoutesWithUpstreamsKeepsDefaultsAndDropsForMirrors(t *testing.T) {
	got, err := RegistryProxyRoutesWithUpstreams("https://registry.npmjs.org", "https://pypi.org/simple", "https://files.pythonhosted.org", "https://proxy.golang.org", "https://sum.golang.org")
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultRegistryProxyRoutes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default upstreams:\n got %+v\nwant %+v", got, want)
	}

	mirror, err := RegistryProxyRoutesWithUpstreams("https://npm.corp", "https://pypi.corp/simple", "https://files.corp", "https://gomod.corp", "https://sumdb.corp")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range mirror {
		switch r.Prefix {
		case pypiRoutePrefix:
			if !reflect.DeepEqual(r.AllowedHosts, []string{"files.corp->" + pypiFilesRoutePrefix}) {
				t.Errorf("pypi AllowedHosts = %v, want the mirror's files host rewrite", r.AllowedHosts)
			}
			if r.Upstream != "https://pypi.corp/simple" {
				t.Errorf("pypi Upstream = %q", r.Upstream)
			}
		case goProxyRoutePrefix:
			if r.Upstream != "https://gomod.corp" || r.AllowedHosts != nil {
				t.Errorf("go route = %+v, want the mirror upstream and no inherited public redirect host", r)
			}
		}
	}
}

// TestEnsureScratchDirOwnsForWorkerGroup: with a numeric worker identity
// the directory belongs to that gid (the process's own here, the only one
// a non-root test can chown to) and is group-writable; the image-default
// (empty) identity and a named user are left to the process's own.
func TestEnsureScratchDirOwnsForWorkerGroup(t *testing.T) {
	dataDir := t.TempDir()
	dir, err := EnsureScratchDir(dataDir, "run-g", "worker-1", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o070 != 0o070 {
		t.Errorf("mode = %v, want group rwx", info.Mode().Perm())
	}
	if _, err := EnsureScratchDir(dataDir, "run-named", "worker-1", "worker"); err != nil {
		t.Errorf("named user: %v", err)
	}
	if _, _, ok := parseWorkerIdentity("worker"); ok {
		t.Error("parseWorkerIdentity(named) = ok, want not ok")
	}
	if uid, gid, ok := parseWorkerIdentity("65532:20"); !ok || uid != 65532 || gid != 20 {
		t.Errorf("parseWorkerIdentity(65532:20) = %d,%d,%v", uid, gid, ok)
	}
	if uid, gid, ok := parseWorkerIdentity("65532"); !ok || uid != 65532 || gid != -1 {
		t.Errorf("parseWorkerIdentity(65532) = %d,%d,%v", uid, gid, ok)
	}
}

func TestWithModCacheRWPreservesExistingFlags(t *testing.T) {
	for env, want := range map[string]string{"": "-modcacherw", "GOFLAGS=-tags=x -mod=vendor": "-tags=x -mod=vendor -modcacherw", "GOFLAGS=-modcacherw": "-modcacherw"} {
		var environment []string
		if env != "" {
			environment = []string{"PATH=/x", env}
		}
		if got := withModCacheRW(environment); got != want {
			t.Errorf("withModCacheRW(%q) = %q, want %q", env, got, want)
		}
	}
}

// TestReconcileScratchDirsRemovesOnlyRunsNotInFlight: the crash path. A
// scratch directory whose run is terminal or has no record is removed;
// one whose run is still in flight is kept.
func TestReconcileScratchDirsRemovesOnlyRunsNotInFlight(t *testing.T) {
	dataDir := t.TempDir()
	for _, id := range []string{"done", "gone", "live"} {
		if _, err := EnsureScratchDir(dataDir, id, "worker-1", ""); err != nil {
			t.Fatal(err)
		}
	}
	removed := ReconcileScratchDirs(dataDir, func(runID string) bool { return runID == "live" })
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want done and gone", removed)
	}
	if _, err := os.Stat(RunScratchDir(dataDir, "live")); err != nil {
		t.Errorf("in-flight run's scratch dir was removed: %v", err)
	}
	if ReconcileScratchDirs(t.TempDir(), func(string) bool { return false }) != nil {
		t.Error("no scratch directory at all should reconcile to nothing")
	}
}

// TestEnsureScratchDirIsPerLaunch: each container gets its own scratch
// directory under the run's, so canonical verify never compiles or replays
// a cache the build phase (or an earlier attempt) wrote.
func TestEnsureScratchDirIsPerLaunch(t *testing.T) {
	dataDir := t.TempDir()
	build, err := EnsureScratchDir(dataDir, "run-1", "factoryd-worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	verify, err := EnsureScratchDir(dataDir, "run-1", "factoryd-worker-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if build == verify || filepath.Dir(build) != RunScratchDir(dataDir, "run-1") || filepath.Dir(verify) != RunScratchDir(dataDir, "run-1") {
		t.Fatalf("build = %q, verify = %q; want two distinct directories under %q", build, verify, RunScratchDir(dataDir, "run-1"))
	}
	for _, launch := range []string{"", ".", "..", "a/b"} {
		if _, err := EnsureScratchDir(dataDir, "run-1", launch, ""); err == nil {
			t.Errorf("EnsureScratchDir(launch %q) = nil error, want a rejection", launch)
		}
	}
}
