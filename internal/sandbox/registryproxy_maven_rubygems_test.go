package sandbox

import (
	"context"
	"encoding/xml"
	"regexp"
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

// A worker on a run with the Maven and RubyGems routes gets Maven's mirror,
// the wrapper's download URL and Bundler's mirror, all on the route, and no
// settings file named by path (an image may lack it); what it downloads and
// Java's temporary files go to the scratch volume; and whatever the caller
// set for those names is replaced.
func TestRegistryProxyWorkerGetsMavenAndRubyGemsEnvironment(t *testing.T) {
	routes, err := NarrowRegistryRoutes(DefaultRegistryProxyRoutes(), []string{"maven", "rubygems"})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, dataDir := startedLifecycle(t, routes)
	stale := []string{"MAVEN_ARGS=-s /workspace/evil.xml", "BUILDGATE_MAVEN_MIRROR=https://evil", "MVNW_REPOURL=https://evil", "JAVA_TOOL_OPTIONS=-Djava.io.tmpdir=/tmp", "BUNDLE_MIRROR__ALL=https://evil", "BUNDLE_PATH=/usr/local/bundle", "GEM_HOME=/usr/local/gems"}
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
		"MAVEN_ARGS=-Dmaven.repo.local=/scratch/maven-repository",
		"MVNW_REPOURL=http://registry-proxy:8092/maven",
		"JAVA_TOOL_OPTIONS=-Djava.io.tmpdir=/scratch/java-tmp",
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

// Without the two routes nothing of theirs reaches the worker.
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
	for _, key := range []string{"MAVEN_ARGS", "BUILDGATE_MAVEN_MIRROR", "MVNW_REPOURL", "JAVA_TOOL_OPTIONS", "BUNDLE_MIRROR__ALL", "BUNDLE_PATH", "GEM_HOME"} {
		if strings.Contains(joined, key+"=") {
			t.Errorf("Environment = %q, want no %s", worker.Environment, key)
		}
	}
	if worker.Scratch {
		t.Error("Scratch = true for an npm-only proxy")
	}
}

// mavenSettings is the part of a Maven settings file the derived Maven's
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

// derivedMavenSettingsPath is where the Java stage of Dockerfile.project
// writes Maven's global settings: Maven's own, read with no option.
const derivedMavenSettingsPath = "/usr/share/maven/conf/settings.xml"

// derivedMavenSettings is the settings file Dockerfile.project's Java stage
// writes: the single-quoted lines of its `printf '%s\n'` into
// derivedMavenSettingsPath.
func derivedMavenSettings(t *testing.T) string {
	t.Helper()
	_, rest, found := strings.Cut(string(ProjectDockerfile), "FROM python-${PYTHON_MODE} AS java-replace")
	if !found {
		t.Fatal("Dockerfile.project has no java-replace stage")
	}
	stage, _, _ := strings.Cut(rest, "\nFROM ")
	_, body, found := strings.Cut(stage, "printf '%s\\n'")
	if !found {
		t.Fatal("the java-replace stage writes no settings file")
	}
	body, _, found = strings.Cut(body, "> "+derivedMavenSettingsPath+";")
	if !found {
		t.Fatalf("the java-replace stage's printf does not write %s", derivedMavenSettingsPath)
	}
	var lines []string
	for _, quoted := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(body, -1) {
		lines = append(lines, quoted[1])
	}
	return strings.Join(lines, "\n")
}

// The derived Maven's global settings mirror every repository through the
// proxy's route and hold nothing else: no credential, no proxy, no profile
// that could add a repository. The Java stage writes them root-owned and
// read-only, and its image's own mirror value is Maven Central.
func TestMavenSettingsMirrorEveryRepositoryThroughTheProxy(t *testing.T) {
	data := derivedMavenSettings(t)
	var settings mavenSettings
	if err := xml.Unmarshal([]byte(data), &settings); err != nil {
		t.Fatalf("parse the derived Maven settings %q: %v", data, err)
	}
	if len(settings.Mirrors) != 1 || settings.Mirrors[0].MirrorOf != "*" || settings.Mirrors[0].URL != "${env.BUILDGATE_MAVEN_MIRROR}" {
		t.Errorf("mirrors = %+v, want one mirror of * at ${env.BUILDGATE_MAVEN_MIRROR}", settings.Mirrors)
	}
	if len(settings.Servers)+len(settings.Proxies)+len(settings.Profiles) != 0 || settings.Offline != "" {
		t.Errorf("settings = %+v, want nothing but the mirror", settings)
	}
	for _, line := range []string{
		"chown root:root " + derivedMavenSettingsPath,
		"chmod 0444 " + derivedMavenSettingsPath,
		"ENV BUILDGATE_MAVEN_MIRROR=https://repo.maven.apache.org/maven2",
	} {
		if !strings.Contains(string(ProjectDockerfile), line) {
			t.Errorf("Dockerfile.project lacks %q", line)
		}
	}
}
