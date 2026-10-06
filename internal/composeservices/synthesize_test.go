package composeservices

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var testOpts = SynthesizeOptions{MemoryLimit: "2g", CPUs: "1", PIDsLimit: 256}

func TestSynthesizeNeverEmitsPorts(t *testing.T) {
	services := []ServiceSpec{{Name: "web", Image: "nginx:1", Ports: []string{"8080"}}}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if strings.Contains(string(out), "ports") {
		t.Fatalf("synthesized project must never emit \"ports\":\n%s", out)
	}
}

func TestSynthesizeEmitsPlatformAndReadOnlyMaterializedBind(t *testing.T) {
	services := []ServiceSpec{{
		Name:     "database",
		Image:    "postgres:16",
		Platform: "linux/amd64",
		NamedVolumes: []VolumeMount{{
			Type:   "bind",
			Source: "pgdata",
			Target: "/var/lib/postgresql/data",
		}},
	}}
	bindRoot := filepath.Join(t.TempDir(), "bind-inputs")
	out, err := Synthesize(services, "bg-compose-x", SynthesizeOptions{
		MemoryLimit:   "2g",
		CPUs:          "1",
		PIDsLimit:     256,
		BindSourceDir: bindRoot,
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	database := doc["services"].(map[string]any)["database"].(map[string]any)
	if database["platform"] != "linux/amd64" {
		t.Fatalf("platform = %#v, want linux/amd64", database["platform"])
	}
	volumes, ok := database["volumes"].([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("volumes = %#v, want one bind mount", database["volumes"])
	}
	want := filepath.ToSlash(bindRoot) + "/pgdata:/var/lib/postgresql/data:ro"
	if volumes[0] != want {
		t.Fatalf("bind volume = %#v, want %q", volumes[0], want)
	}
	if strings.Contains(string(out), "container_name") {
		t.Fatalf("synthesized project must never emit container_name:\n%s", out)
	}
}

func TestSynthesizePinsToExternalNetwork(t *testing.T) {
	services := []ServiceSpec{{Name: "web", Image: "nginx:1"}}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	networks, ok := doc["networks"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level \"networks\", got %#v", doc["networks"])
	}
	def, ok := networks["default"].(map[string]any)
	if !ok {
		t.Fatalf("expected \"networks.default\", got %#v", networks["default"])
	}
	if def["external"] != true || def["name"] != "bg-compose-x" {
		t.Fatalf("expected external network pinned to bg-compose-x, got %#v", def)
	}
}

// TestSynthesizePreservesDependsOnCondition covers a finding from Codex
// review of PR #131, round 2: Synthesize must emit exactly the condition
// ParseFile already validated onto ServiceSpec.DependsOn, never re-derive
// one from the target service's own healthcheck presence -- db's explicit
// service_healthy and cache's service_started (compose's own default for
// an unqualified dependency) must both survive unchanged even though db is
// the one with a healthcheck.
func TestSynthesizePreservesDependsOnCondition(t *testing.T) {
	services := []ServiceSpec{
		{Name: "db", Image: "postgres:16", Healthcheck: &Healthcheck{Test: []string{"CMD", "pg_isready"}}},
		{Name: "cache", Image: "redis:7"},
		{Name: "app", Image: "myapp:1", DependsOn: []ServiceDependency{
			{Name: "db", Condition: "service_healthy"},
			{Name: "cache", Condition: "service_started"},
		}},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	app := doc["services"].(map[string]any)["app"].(map[string]any)
	dependsOn := app["depends_on"].(map[string]any)
	if dependsOn["db"].(map[string]any)["condition"] != "service_healthy" {
		t.Fatalf("expected db dependency to require service_healthy, got %#v", dependsOn["db"])
	}
	if dependsOn["cache"].(map[string]any)["condition"] != "service_started" {
		t.Fatalf("expected cache dependency to require service_started, got %#v", dependsOn["cache"])
	}
}

// TestSynthesizePreservesCompletedSuccessfullyCondition is the regression
// for that same PR #131 finding's own headline case: a migration-style
// dependency must keep waiting for completion, not get silently
// downgraded to service_started/service_healthy by healthcheck-based
// inference.
func TestSynthesizePreservesCompletedSuccessfullyCondition(t *testing.T) {
	services := []ServiceSpec{
		{Name: "migrate", Image: "myapp/migrate:1"},
		{Name: "app", Image: "myapp:1", DependsOn: []ServiceDependency{
			{Name: "migrate", Condition: "service_completed_successfully"},
		}},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	app := doc["services"].(map[string]any)["app"].(map[string]any)
	dependsOn := app["depends_on"].(map[string]any)
	if dependsOn["migrate"].(map[string]any)["condition"] != "service_completed_successfully" {
		t.Fatalf("expected migrate dependency to require service_completed_successfully, got %#v", dependsOn["migrate"])
	}
}

// TestSynthesizeDisabledHealthcheckEmitsDisableTrue covers a finding from
// Codex review of PR #131, round 2: a service that explicitly disabled
// its healthcheck must synthesize `healthcheck: {disable: true}`, never a
// `test` field, so a real `docker compose up --wait` never waits on (or
// inherits from the image) a healthcheck the source file turned off.
func TestSynthesizeDisabledHealthcheckEmitsDisableTrue(t *testing.T) {
	services := []ServiceSpec{{Name: "web", Image: "myimage:1", Healthcheck: &Healthcheck{Disabled: true}}}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	hc, ok := web["healthcheck"].(map[string]any)
	if !ok || hc["disable"] != true {
		t.Fatalf("expected healthcheck: {disable: true}, got %#v", web["healthcheck"])
	}
	if _, ok := hc["test"]; ok {
		t.Fatalf("expected no \"test\" field alongside disable:true, got %#v", hc)
	}
}

func TestSynthesizeEscapesDollarInEnvironment(t *testing.T) {
	services := []ServiceSpec{{Name: "app", Image: "myapp:1", Environment: map[string]string{"SECRET": "a$b"}}}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if !strings.Contains(string(out), "a$$b") {
		t.Fatalf("expected literal \"$\" re-escaped as \"$$\" in synthesized yaml:\n%s", out)
	}
}

// TestSynthesizeEmitsEntrypointAndCommandSeparately covers a finding
// from Codex review of PR #131: entrypoint and command must be emitted as
// independent fields, matching what ParseFile actually decoded, and an
// explicit empty override (a non-nil, empty slice, see ServiceSpec.
// Entrypoint's own doc comment) must still be emitted as `[]` rather than
// omitted like an unset field.
func TestSynthesizeEmitsEntrypointAndCommandSeparately(t *testing.T) {
	services := []ServiceSpec{{
		Name:       "web",
		Image:      "myimage:1",
		Entrypoint: []string{"/custom-entrypoint"},
		Command:    []string{"--flag"},
	}}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	entrypoint, _ := web["entrypoint"].([]any)
	if len(entrypoint) != 1 || entrypoint[0] != "/custom-entrypoint" {
		t.Fatalf("entrypoint = %#v, want [/custom-entrypoint]", web["entrypoint"])
	}
	command, _ := web["command"].([]any)
	if len(command) != 1 || command[0] != "--flag" {
		t.Fatalf("command = %#v, want [--flag]", web["command"])
	}
}

func TestSynthesizeEmitsExplicitEmptyEntrypointButOmitsUnset(t *testing.T) {
	services := []ServiceSpec{
		{Name: "cleared", Image: "myimage:1", Entrypoint: []string{}},
		{Name: "unset", Image: "myimage:1"},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	svcs := doc["services"].(map[string]any)
	cleared := svcs["cleared"].(map[string]any)
	if _, ok := cleared["entrypoint"]; !ok {
		t.Fatal("expected an explicit, empty \"entrypoint\" key for the cleared service")
	}
	unset := svcs["unset"].(map[string]any)
	if _, ok := unset["entrypoint"]; ok {
		t.Fatalf("expected no \"entrypoint\" key for the unset service, got %#v", unset["entrypoint"])
	}
}

// TestSynthesizeHonorsUserWorkingDirReadOnlyTmpfsShmSizeInit covers the
// same finding from Codex review of PR #131: each of these accepted
// fields must actually be emitted, not silently dropped between
// ParseFile and the container that launches.
func TestSynthesizeHonorsUserWorkingDirReadOnlyTmpfsShmSizeInit(t *testing.T) {
	services := []ServiceSpec{{
		Name:       "web",
		Image:      "myimage:1",
		User:       "1000:1000",
		WorkingDir: "/srv/app",
		ReadOnly:   true,
		Tmpfs:      []string{"/tmp", "/run"},
		ShmSize:    134217728,
		Init:       true,
	}}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	if web["user"] != "1000:1000" {
		t.Errorf("user = %#v, want 1000:1000", web["user"])
	}
	if web["working_dir"] != "/srv/app" {
		t.Errorf("working_dir = %#v, want /srv/app", web["working_dir"])
	}
	if web["read_only"] != true {
		t.Errorf("read_only = %#v, want true", web["read_only"])
	}
	if web["init"] != true {
		t.Errorf("init = %#v, want true", web["init"])
	}
	if web["shm_size"] != 134217728 {
		t.Errorf("shm_size = %#v, want 134217728", web["shm_size"])
	}
	tmpfs, _ := web["tmpfs"].([]any)
	if len(tmpfs) != 2 || tmpfs[0] != "/tmp" || tmpfs[1] != "/run" {
		t.Errorf("tmpfs = %#v, want [/tmp /run]", web["tmpfs"])
	}
}

// TestSynthesizeSharesNamedVolumeAcrossServices covers a finding from
// Codex review of PR #131: two services referencing the same named
// volume by its own source name must share one top-level volume in the
// synthesized file, not each get an unshared volume of their own.
func TestSynthesizeSharesNamedVolumeAcrossServices(t *testing.T) {
	services := []ServiceSpec{
		{Name: "writer", Image: "myimage:1", NamedVolumes: []VolumeMount{{Type: "volume", Source: "shared-data", Target: "/data"}}},
		{Name: "reader", Image: "myimage:1", NamedVolumes: []VolumeMount{{Type: "volume", Source: "shared-data", Target: "/data-ro"}}},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	volumes, ok := doc["volumes"].(map[string]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("expected exactly 1 top-level volume (shared), got %#v", doc["volumes"])
	}
	if _, ok := volumes["shared-data"]; !ok {
		t.Fatalf("expected the top-level volume to keep the source name \"shared-data\", got %#v", volumes)
	}
	svcs := doc["services"].(map[string]any)
	writerVolumes, _ := svcs["writer"].(map[string]any)["volumes"].([]any)
	if len(writerVolumes) != 1 || writerVolumes[0] != "shared-data:/data" {
		t.Errorf("writer volumes = %#v, want [shared-data:/data]", writerVolumes)
	}
	readerVolumes, _ := svcs["reader"].(map[string]any)["volumes"].([]any)
	if len(readerVolumes) != 1 || readerVolumes[0] != "shared-data:/data-ro" {
		t.Errorf("reader volumes = %#v, want [shared-data:/data-ro]", readerVolumes)
	}
}

// TestSynthesizeKeepsAnonymousVolumesUnshared covers the fallback half
// of that same PR #131 finding: a mount with no source name in the
// original file (an anonymous volume) must keep deriving a per-service
// name exactly as before, since two services never intentionally share
// an anonymous volume's contents.
func TestSynthesizeKeepsAnonymousVolumesUnshared(t *testing.T) {
	services := []ServiceSpec{
		{Name: "a", Image: "myimage:1", NamedVolumes: []VolumeMount{{Type: "volume", Target: "/data"}}},
		{Name: "b", Image: "myimage:1", NamedVolumes: []VolumeMount{{Type: "volume", Target: "/data"}}},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	volumes, ok := doc["volumes"].(map[string]any)
	if !ok || len(volumes) != 2 {
		t.Fatalf("expected 2 unshared top-level volumes, got %#v", doc["volumes"])
	}
}

// TestSynthesizeEmitsRealTmpfsMount covers the other half of that same
// PR #131 finding: a `volumes:`-declared tmpfs mount must become a real
// `tmpfs:` entry, never a disk-backed named volume.
func TestSynthesizeEmitsRealTmpfsMount(t *testing.T) {
	services := []ServiceSpec{
		{Name: "web", Image: "myimage:1", NamedVolumes: []VolumeMount{{Type: "tmpfs", Target: "/tmp/scratch"}}},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	if _, ok := doc["volumes"]; ok {
		t.Fatalf("expected no top-level named volume for a tmpfs mount, got %#v", doc["volumes"])
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	tmpfs, _ := web["tmpfs"].([]any)
	if len(tmpfs) != 1 || tmpfs[0] != "/tmp/scratch" {
		t.Fatalf("tmpfs = %#v, want [/tmp/scratch]", web["tmpfs"])
	}
}

// TestSynthesizeRoundTripsThroughRealComposeCLI is the concrete detector for
// CLI/compose-go version skew and for accidental re-interpolation bugs (see
// Synthesize's own doc comment on why the "$$" re-escape is load-bearing):
// it feeds a synthesized project through the actual `docker compose` CLI --
// the same binary a real run would exec -- and checks the resolved config
// back out, field for field, rather than only asserting properties of the
// YAML this package itself produced.
func TestSynthesizeRoundTripsThroughRealComposeCLI(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available on this host")
	}
	scrubbedEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	probe := exec.Command("docker", "compose", "version")
	probe.Env = scrubbedEnv
	if err := probe.Run(); err != nil {
		t.Skipf("docker compose plugin not available on this host: %v", err)
	}

	const networkName = "bg-compose-roundtrip-test"
	services := []ServiceSpec{
		{
			Name:         "db",
			Image:        "postgres:16",
			Hostname:     "db-host",
			Environment:  map[string]string{"POSTGRES_PASSWORD": "p$ssword"},
			Healthcheck:  &Healthcheck{Test: []string{"CMD", "pg_isready"}, Interval: "5s", Timeout: "3s", Retries: 5},
			NamedVolumes: []VolumeMount{{Type: "volume", Target: "/var/lib/postgresql/data"}},
		},
		{
			Name:      "app",
			Image:     "myapp:latest",
			Command:   []string{"serve"},
			DependsOn: []ServiceDependency{{Name: "db", Condition: "service_healthy"}},
		},
		{
			Name:        "quiet",
			Image:       "alpine:3.20",
			Healthcheck: &Healthcheck{Disabled: true},
		},
	}
	out, err := Synthesize(services, networkName, testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	tmpDir := t.TempDir()
	composePath := filepath.Join(tmpDir, "synthesized.yml")
	if err := os.WriteFile(composePath, out, 0o644); err != nil {
		t.Fatalf("write synthesized compose file: %v", err)
	}

	create := exec.Command("docker", "network", "create", "--internal", networkName)
	create.Env = scrubbedEnv
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create test network: %v: %s", err, out)
	}
	defer func() {
		rm := exec.Command("docker", "network", "rm", networkName)
		rm.Env = scrubbedEnv
		rm.Run()
	}()

	cmd := exec.Command("docker", "compose", "-f", composePath, "--project-directory", tmpDir, "config", "--format", "json")
	// A scrubbed environment, not the test process' own inherited one
	// (which may itself set POSTGRES_PASSWORD or similar): the resolved
	// config below must reflect only what Synthesize wrote, never
	// anything ambient re-interpolated in by the real CLI.
	cmd.Env = scrubbedEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose config: %v: %s", err, stderr.String())
	}

	var resolved struct {
		Networks map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"networks"`
		Services map[string]struct {
			Image       string            `json:"image"`
			Hostname    string            `json:"hostname"`
			Environment map[string]string `json:"environment"`
			Command     []string          `json:"command"`
			DependsOn   map[string]struct {
				Condition string `json:"condition"`
			} `json:"depends_on"`
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
				Target string `json:"target"`
			} `json:"volumes"`
			Ports       json.RawMessage `json:"ports"`
			Healthcheck *struct {
				Disable bool `json:"disable"`
			} `json:"healthcheck"`
		} `json:"services"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resolved); err != nil {
		t.Fatalf("unmarshal resolved config: %v\n%s", err, stdout.String())
	}

	net, ok := resolved.Networks["default"]
	if !ok || !net.External || net.Name != networkName {
		t.Fatalf("expected default network pinned externally to %q, got %#v", networkName, resolved.Networks)
	}

	db, ok := resolved.Services["db"]
	if !ok {
		t.Fatalf("resolved config missing service \"db\": %s", stdout.String())
	}
	if db.Image != "postgres:16" || db.Hostname != "db-host" {
		t.Fatalf("db service resolved unexpectedly: %#v", db)
	}
	if len(db.Ports) != 0 && string(db.Ports) != "null" && string(db.Ports) != "[]" {
		t.Fatalf("synthesized project must never resolve any ports, got %s", db.Ports)
	}
	// `docker compose config` re-escapes "$" back to "$$" in its own
	// resolved-config output (confirmed empirically) -- undoing that once
	// more here is what actually proves the round trip: the value
	// Synthesize was handed ("p$ssword") must be exactly what a real
	// `docker compose up` would resolve, not something re-interpolated by
	// the CLI's own load a second time.
	if got := strings.ReplaceAll(db.Environment["POSTGRES_PASSWORD"], "$$", "$"); got != "p$ssword" {
		t.Fatalf("expected POSTGRES_PASSWORD to resolve to \"p$ssword\", got %q (raw %q)", got, db.Environment["POSTGRES_PASSWORD"])
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Type != "volume" || db.Volumes[0].Target != "/var/lib/postgresql/data" {
		t.Fatalf("expected exactly one named-volume (never bind) mount on db, got %#v", db.Volumes)
	}

	app, ok := resolved.Services["app"]
	if !ok {
		t.Fatalf("resolved config missing service \"app\": %s", stdout.String())
	}
	if app.Image != "myapp:latest" || len(app.Command) != 1 || app.Command[0] != "serve" {
		t.Fatalf("app service resolved unexpectedly: %#v", app)
	}
	if cond, ok := app.DependsOn["db"]; !ok || cond.Condition != "service_healthy" {
		t.Fatalf("expected app to depend on db with service_healthy, got %#v", app.DependsOn)
	}

	quiet, ok := resolved.Services["quiet"]
	if !ok {
		t.Fatalf("resolved config missing service \"quiet\": %s", stdout.String())
	}
	if quiet.Healthcheck == nil || !quiet.Healthcheck.Disable {
		t.Fatalf("expected quiet's healthcheck to resolve as disabled, got %#v", quiet.Healthcheck)
	}
}

func TestSynthesizeAppliesTheNarrowerMemLimit(t *testing.T) {
	services := []ServiceSpec{
		{Name: "kafka", Image: "apache/kafka:3.8.0", MemLimit: 512 << 20},
		{Name: "redis", Image: "redis:7"},
		// Wider than the operator's limit: ParseFile never returns this,
		// and Synthesize must still apply the operator's.
		{Name: "wide", Image: "postgres:16", MemLimit: 4 << 30},
	}
	out, err := Synthesize(services, "bg-compose-x", testOpts)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal synthesized yaml: %v", err)
	}
	for name, want := range map[string]any{"kafka": 512 << 20, "redis": "2g", "wide": 2 << 30} {
		svc := doc.Services[name]
		if svc["mem_limit"] != want || svc["memswap_limit"] != want {
			t.Errorf("%s: mem_limit = %#v, memswap_limit = %#v; want %#v", name, svc["mem_limit"], svc["memswap_limit"], want)
		}
	}
}
