package sandbox

import (
	"context"
	"encoding/xml"
	"os"
	"slices"
	"strings"
	"testing"
)

// The Maven and RubyGems routes front their public registries and allow no
// redirect host: neither upstream redirects any request its client makes.
func TestDefaultMavenAndRubyGemsRoutesAllowNoRedirectHost(t *testing.T) {
	want := map[string]string{
		mavenRoutePrefix:    "https://repo.maven.apache.org/maven2",
		rubygemsRoutePrefix: "https://rubygems.org",
	}
	for _, route := range DefaultRegistryProxyRoutes() {
		upstream, ok := want[route.Prefix]
		if !ok {
			continue
		}
		delete(want, route.Prefix)
		if route.Upstream != upstream || len(route.AllowedHosts) != 0 {
			t.Errorf("route %s = %+v, want upstream %s and no redirect host", route.Prefix, route, upstream)
		}
	}
	if len(want) != 0 {
		t.Errorf("DefaultRegistryProxyRoutes lacks %v", want)
	}
}

// startedLifecycle is a registry proxy lifecycle over routes whose proxy has
// started on its own network.
func startedLifecycle(t *testing.T, routes []RegistryProxyRoute) (*RegistryProxyLifecycle, string) {
	t.Helper()
	spec := validRegistryProxySpec()
	spec.Routes = routes
	dataDir := t.TempDir()
	spec.DataDir = dataDir
	handle := &RegistryProxyHandle{
		WorkerBaseURL: "http://registry-proxy:8092",
		AuditFacts:    RegistryProxyLaunchFacts{NetworkName: "factoryd-registryproxy-abc", WorkerBaseURL: "http://registry-proxy:8092"},
	}
	lifecycle, err := BeginRegistryProxyLifecycle(spec, "docker", spec.RunID, dataDir, RegistryProxyHooks{
		StartRegistryProxy:   func(context.Context, string, RegistryProxySpec) (*RegistryProxyHandle, error) { return handle, nil },
		CleanupRegistryProxy: func(context.Context, *RegistryProxyHandle) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.Cleanup() })
	if err := lifecycle.Ensure(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	return lifecycle, dataDir
}

// A worker on a run with the Maven and RubyGems routes gets Maven's global
// settings, its mirror, the wrapper's download URL and Bundler's mirror, all
// on the route; what it downloads goes to the scratch volume; and whatever
// the caller set for those names is replaced.
func TestRegistryProxyWorkerGetsMavenAndRubyGemsEnvironment(t *testing.T) {
	routes, err := NarrowRegistryRoutes(DefaultRegistryProxyRoutes(), []string{"maven", "rubygems"})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, dataDir := startedLifecycle(t, routes)
	stale := []string{"MAVEN_ARGS=-s /workspace/evil.xml", "BUILDGATE_MAVEN_MIRROR=https://evil", "MVNW_REPOURL=https://evil", "BUNDLE_MIRROR__ALL=https://evil", "BUNDLE_PATH=/usr/local/bundle", "GEM_HOME=/usr/local/gems"}
	worker, err := lifecycle.PrepareWorker(LaunchSpec{Name: "factoryd-worker-1", RunID: "run1", DataDir: dataDir, Environment: append(stale, "PATH=/usr/bin")})
	if err != nil {
		t.Fatal(err)
	}
	if !worker.Scratch {
		t.Error("Scratch = false, want the disk-backed scratch volume")
	}
	want := []string{
		"PATH=/usr/bin",
		"BUILDGATE_MAVEN_MIRROR=http://registry-proxy:8092/maven",
		"MAVEN_ARGS=-gs /etc/buildgate/maven/settings.xml -Dmaven.repo.local=/scratch/maven-repository",
		"MVNW_REPOURL=http://registry-proxy:8092/maven",
		"BUNDLE_MIRROR__ALL=http://registry-proxy:8092/rubygems/",
		"BUNDLE_PATH=/scratch/bundle",
		"GEM_HOME=/scratch/gems",
	}
	if !slices.Equal(worker.Environment, want) {
		t.Errorf("Environment = %q\nwant %q", worker.Environment, want)
	}
	for _, value := range stale {
		if slices.Contains(worker.Environment, value) {
			t.Errorf("the caller's %q was kept", value)
		}
	}
}

// Without the two routes nothing of theirs reaches the worker, and the
// image's settings file is never named.
func TestRegistryProxyWorkerWithoutMavenOrRubyGemsRoutesGetsNeither(t *testing.T) {
	routes, err := NarrowRegistryRoutes(DefaultRegistryProxyRoutes(), []string{"npm"})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, dataDir := startedLifecycle(t, routes)
	worker, err := lifecycle.PrepareWorker(LaunchSpec{Name: "factoryd-worker-1", RunID: "run1", DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(worker.Environment, "\n")
	for _, key := range []string{"MAVEN_ARGS", "BUILDGATE_MAVEN_MIRROR", "MVNW_REPOURL", "BUNDLE_MIRROR__ALL", "BUNDLE_PATH", "GEM_HOME"} {
		if strings.Contains(joined, key+"=") {
			t.Errorf("Environment = %q, want no %s", worker.Environment, key)
		}
	}
	if worker.Scratch {
		t.Error("Scratch = true for an npm-only proxy")
	}
}

// mavenSettings is the part of a Maven settings file the worker image's
// global settings may hold.
type mavenSettings struct {
	XMLName xml.Name `xml:"settings"`
	Mirrors []struct {
		ID       string `xml:"id"`
		URL      string `xml:"url"`
		MirrorOf string `xml:"mirrorOf"`
	} `xml:"mirrors>mirror"`
	Servers  []struct{} `xml:"servers>server"`
	Proxies  []struct{} `xml:"proxies>proxy"`
	Profiles []struct{} `xml:"profiles>profile"`
	Offline  string     `xml:"offline"`
}

// The image's global Maven settings mirror every repository through the
// proxy's route and hold nothing else: no credential, no proxy, no profile
// that could add a repository. The worker Dockerfile bakes that file, root
// owned and read-only, at the path MAVEN_ARGS names.
func TestMavenSettingsMirrorEveryRepositoryThroughTheProxy(t *testing.T) {
	data, err := os.ReadFile("maven-settings.xml")
	if err != nil {
		t.Fatal(err)
	}
	var settings mavenSettings
	if err := xml.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse maven-settings.xml: %v", err)
	}
	if len(settings.Mirrors) != 1 || settings.Mirrors[0].MirrorOf != "*" || settings.Mirrors[0].URL != "${env.BUILDGATE_MAVEN_MIRROR}" {
		t.Errorf("mirrors = %+v, want one mirror of * at ${env.BUILDGATE_MAVEN_MIRROR}", settings.Mirrors)
	}
	if len(settings.Servers)+len(settings.Proxies)+len(settings.Profiles) != 0 || settings.Offline != "" {
		t.Errorf("settings = %+v, want nothing but the mirror", settings)
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"COPY internal/sandbox/maven-settings.xml " + mavenSettingsPath,
		"chmod 0444 " + mavenSettingsPath,
		"test -r " + mavenSettingsPath + " && test ! -w " + mavenSettingsPath,
	} {
		if !strings.Contains(string(dockerfile), line) {
			t.Errorf("Dockerfile lacks %q", line)
		}
	}
}
