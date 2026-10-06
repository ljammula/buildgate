package composeservices

import (
	"reflect"
	"strings"
	"testing"
)

var allowDockerHub = Options{AllowedImageRegistries: []string{"docker.io/library/"}}

func TestParseFileAllowsAWellFormedService(t *testing.T) {
	yaml := `
services:
  postgres:
    image: docker.io/library/postgres:16.4
    environment:
      POSTGRES_USER: todo
      POSTGRES_PASSWORD: todo
    ports:
      - "5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U todo"]
      interval: 2s
      timeout: 2s
      retries: 15
    volumes:
      - pgdata:/var/lib/postgresql/data
`
	services, rejected, skipped, err := ParseFile([]byte(yaml), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %v", skipped)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	s := services[0]
	if s.Name != "postgres" || s.Image != "docker.io/library/postgres:16.4" {
		t.Fatalf("unexpected service: %+v", s)
	}
	if s.Environment["POSTGRES_USER"] != "todo" {
		t.Fatalf("unexpected environment: %+v", s.Environment)
	}
	if s.Healthcheck == nil || s.Healthcheck.Retries != 15 {
		t.Fatalf("unexpected healthcheck: %+v", s.Healthcheck)
	}
}

func TestParseFileAllowsSupportedPlatformInertContainerNameAndRelativeBind(t *testing.T) {
	doc := `
services:
  database:
    image: docker.io/library/postgres:16
    platform: linux/amd64
    container_name: target-database
    volumes:
      - ./pgdata:/var/lib/postgresql/data
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 || len(services) != 1 {
		t.Fatalf("expected one accepted service, got services=%v rejected=%v", services, rejected)
	}
	if services[0].Platform != "linux/amd64" {
		t.Fatalf("Platform = %q, want linux/amd64", services[0].Platform)
	}
	if len(services[0].NamedVolumes) != 1 {
		t.Fatalf("NamedVolumes = %#v, want one bind mount", services[0].NamedVolumes)
	}
	volume := services[0].NamedVolumes[0]
	if volume.Type != "bind" || volume.Source != "pgdata" || volume.Target != "/var/lib/postgresql/data" {
		t.Fatalf("bind volume = %#v, want normalized pgdata bind", volume)
	}
}

func TestParseFileRejectsUnsupportedPlatform(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    platform: windows/amd64
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "not allow-listed") {
		t.Fatalf("expected unsupported-platform rejection, got %v", rejected)
	}
}

func TestParseFileRejectsDangerousFields(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		expect string
	}{
		{"privileged", "privileged: true", "privileged"},
		{"cap_add", "cap_add: [\"SYS_ADMIN\"]", "capabilities"},
		{"network_mode", "network_mode: host", "internal network"},
		{"pid", "pid: host", "PID namespace"},
		{"devices", "devices: [\"/dev/kvm\"]", "devices"},
		{"security_opt", "security_opt: [\"seccomp=unconfined\"]", "security options"},
		{"volumes_from", "volumes_from: [\"other\"]", "another container's volumes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "services:\n  svc:\n    image: docker.io/library/redis:7\n    " + tc.field + "\n"
			_, rejected, skipped, err := ParseFile([]byte(doc), allowDockerHub)
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			if len(skipped) != 0 {
				t.Fatalf("unexpected skips: %v", skipped)
			}
			if len(rejected) != 1 {
				t.Fatalf("expected exactly 1 rejection, got %v", rejected)
			}
			if !strings.Contains(rejected[0].Reason, tc.expect) {
				t.Fatalf("reason %q does not mention %q", rejected[0].Reason, tc.expect)
			}
		})
	}
}

// TestParseFileSkipsBuildServices covers a deliberate design choice:
// unlike every other rejection, a service naming `build:` is
// skipped (and the file's other services still validate normally), not
// rejected outright -- see ParseFile's own doc comment.
func TestParseFileSkipsBuildServices(t *testing.T) {
	doc := `
services:
  postgres:
    image: docker.io/library/postgres:16.4
  fromsource:
    build: .
`
	services, rejected, skipped, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("expected no rejections, got %v", rejected)
	}
	if len(services) != 1 || services[0].Name != "postgres" {
		t.Fatalf("expected only postgres to survive, got %v", services)
	}
	if len(skipped) != 1 || skipped[0].Service != "fromsource" {
		t.Fatalf("expected fromsource to be skipped, got %v", skipped)
	}
}

// TestParseFileRejectsUnreviewedField covers a real, schema-valid compose
// field this package hasn't reviewed onto the allow-list -- the new
// equivalent of the old hand-walker's "some_future_compose_field" case,
// which itself no longer applies: a genuinely made-up key is now rejected
// by compose-go's own schema validation before this package's allow-list
// is even consulted (see TestParseFileRejectsAGenuinelyUnknownTopLevelKey
// for that whole-file failure mode).
func TestParseFileRejectsUnreviewedField(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    pull_policy: always
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "unreviewed") {
		t.Fatalf("expected an unreviewed-field rejection, got %v", rejected)
	}
}

// TestParseFileRejectsAGenuinelyUnknownTopLevelKey exercises the one new
// whole-file failure mode this package's compose-go-backed loader
// introduces: a key that isn't part of the real compose spec at all fails
// compose-go's own schema validation for the whole document, so nothing
// in the file can be recovered -- ParseFile reports this the same way it
// reports every other whole-file problem (Options.MaxServices exceeded,
// "include:" present): one Rejection keyed to the file itself, no
// services or skips.
func TestParseFileRejectsAGenuinelyUnknownTopLevelKey(t *testing.T) {
	doc := `
services:
  postgres:
    image: docker.io/library/postgres:16.4
  svc:
    image: docker.io/library/redis:7
    some_future_compose_field: whatever
`
	services, rejected, skipped, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 0 || len(skipped) != 0 {
		t.Fatalf("expected nothing to survive a whole-file schema failure, got services=%v skipped=%v", services, skipped)
	}
	if len(rejected) != 1 || rejected[0].Service != wholeFileRejectionService {
		t.Fatalf("expected a single whole-file rejection, got %v", rejected)
	}
}

func TestParseFileRejectsHostBindMount(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    volumes:
      - /etc/passwd:/etc/passwd
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "outside the compose project directory") {
		t.Fatalf("expected an outside-project bind rejection, got %v", rejected)
	}
}

func TestParseFileStripsHostSideOfPortMapping(t *testing.T) {
	// A provisioner never publishes a sidecar to the host regardless of
	// what the compose file requests -- so a "host:container" mapping is
	// accepted, with only the container side kept, not rejected outright.
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    ports:
      - "6380:6379"
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("expected no rejections, got %v", rejected)
	}
	if len(services) != 1 || len(services[0].Ports) != 1 || services[0].Ports[0] != "6379" {
		t.Fatalf("expected only the container-side port 6379 to survive, got %+v", services)
	}
}

func TestParseFileRejectsUnlistedRegistry(t *testing.T) {
	doc := `
services:
  svc:
    image: attacker.example.com/malicious:latest
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "not under an allow-listed registry") {
		t.Fatalf("expected a registry rejection, got %v", rejected)
	}
}

func TestParseFileFailsClosedWithNoAllowedRegistries(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
`
	_, rejected, _, err := ParseFile([]byte(doc), Options{})
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 {
		t.Fatalf("expected the service to be rejected when no registry is allow-listed, got %v", rejected)
	}
}

func TestParseFileAllowsHostname(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    hostname: my-redis
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 || services[0].Hostname != "my-redis" {
		t.Fatalf("expected hostname %q to be kept, got %+v", "my-redis", services)
	}
}

func TestParseFileMixedAllowAndReject(t *testing.T) {
	doc := `
services:
  postgres:
    image: docker.io/library/postgres:16.4
  evil:
    image: docker.io/library/redis:7
    privileged: true
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 1 || services[0].Name != "postgres" {
		t.Fatalf("expected only postgres to survive, got %v", services)
	}
	if len(rejected) != 1 || rejected[0].Service != "evil" {
		t.Fatalf("expected evil to be rejected, got %v", rejected)
	}
}

// TestParseFileRejectsExtends covers the extends/include-survives-Skip
// behavior documented above: even with the loader's SkipExtends option
// set, "extends" survives on the parsed service (confirmed empirically,
// see loadIsolated's own comment) and must still be rejected explicitly.
func TestParseFileRejectsExtends(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    extends:
      service: other
      file: /etc/passwd
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "extends") {
		t.Fatalf("expected an extends rejection, got %v", rejected)
	}
}

// TestParseFileRejectsInclude covers the other half of the extends/
// include-survives-Skip behavior: "include" is a top-level directive,
// not a per-service field, so it produces a whole-file rejection rather
// than one scoped to a single service.
func TestParseFileRejectsInclude(t *testing.T) {
	doc := `
include:
  - other-compose.yml
services:
  svc:
    image: docker.io/library/redis:7
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("expected no services to survive an \"include\" file, got %v", services)
	}
	if len(rejected) != 1 || rejected[0].Service != wholeFileRejectionService || !strings.Contains(rejected[0].Reason, "include") {
		t.Fatalf("expected a whole-file include rejection, got %v", rejected)
	}
}

// allowDockerHubWithRegistryProxyReserved mirrors what
// internal/sandbox.ComposeServicesLifecycle actually threads into
// Options.ReservedAliases in production (its own
// ReservedWorkerAliases) -- this package can't import internal/sandbox's
// real constant directly without cycling back (see Options.ReservedAliases'
// own doc comment), so these two tests supply the literal name a caller
// would, the same way that lifecycle does.
var allowDockerHubWithRegistryProxyReserved = Options{AllowedImageRegistries: []string{"docker.io/library/"}, ReservedAliases: []string{"registry-proxy"}}

// TestParseFileRejectsReservedAliasServiceName covers the reserved-alias
// rejection: a service named after one of the sandbox's own
// internal-network aliases is rejected outright, regardless of anything
// else about the service.
func TestParseFileRejectsReservedAliasServiceName(t *testing.T) {
	doc := `
services:
  registry-proxy:
    image: docker.io/library/redis:7
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHubWithRegistryProxyReserved)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "reserved") {
		t.Fatalf("expected a reserved-alias rejection, got %v", rejected)
	}
}

// TestParseFileRejectsReservedNetworkAlias extends the reserved-alias
// rejection to a service that isn't itself named after a reserved
// alias, but tries to claim one via a network alias -- the same
// collision/impersonation risk the reserved-name check exists for.
func TestParseFileRejectsReservedNetworkAlias(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    networks:
      default:
        aliases:
          - registry-proxy
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHubWithRegistryProxyReserved)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "reserved") {
		t.Fatalf("expected a reserved network-alias rejection, got %v", rejected)
	}
}

// TestParseFileEnforcesMaxServices covers the MaxServices option: a
// compose file declaring more services than the configured maximum is
// rejected wholesale, without parsing any individual service.
func TestParseFileEnforcesMaxServices(t *testing.T) {
	doc := `
services:
  a:
    image: docker.io/library/redis:7
  b:
    image: docker.io/library/redis:7
  c:
    image: docker.io/library/redis:7
`
	opts := Options{AllowedImageRegistries: []string{"docker.io/library/"}, MaxServices: 2}
	services, rejected, _, err := ParseFile([]byte(doc), opts)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("expected no services when MaxServices is exceeded, got %v", services)
	}
	if len(rejected) != 1 || rejected[0].Service != wholeFileRejectionService {
		t.Fatalf("expected a single whole-file rejection, got %v", rejected)
	}
}

// TestParseFileKeepsEntrypointSeparateFromCommand covers a finding from
// Codex review of PR #131: entrypoint and command must never be merged
// into one field -- entrypoint overrides the image's own ENTRYPOINT,
// command only ever supplies arguments to whatever ENTRYPOINT is in
// effect.
func TestParseFileKeepsEntrypointSeparateFromCommand(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    entrypoint: ["/custom-entrypoint"]
    command: ["--flag"]
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	s := services[0]
	if len(s.Entrypoint) != 1 || s.Entrypoint[0] != "/custom-entrypoint" {
		t.Fatalf("Entrypoint = %v, want [/custom-entrypoint]", s.Entrypoint)
	}
	if len(s.Command) != 1 || s.Command[0] != "--flag" {
		t.Fatalf("Command = %v, want [--flag] -- entrypoint must not be merged into it", s.Command)
	}
}

// TestParseFilePreservesExplicitEmptyCommandAndEntrypoint covers the
// other half of the same PR #131 finding: compose-go's ShellCommand
// distinguishes "not set" (nil) from "explicitly cleared" (non-nil,
// empty) -- confirmed empirically
// via DecodeMapstructure's own []interface{}{} branch -- and ParseFile must
// preserve that distinction rather than collapsing an explicit `command:
// []`/`entrypoint: []` down to the same nil as an absent field entirely.
func TestParseFilePreservesExplicitEmptyCommandAndEntrypoint(t *testing.T) {
	doc := `
services:
  cleared:
    image: docker.io/library/redis:7
    command: []
    entrypoint: []
  unset:
    image: docker.io/library/redis:7
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	byName := map[string]ServiceSpec{}
	for _, s := range services {
		byName[s.Name] = s
	}
	cleared := byName["cleared"]
	if cleared.Command == nil || len(cleared.Command) != 0 {
		t.Fatalf("cleared.Command = %#v, want a non-nil empty slice (explicit override)", cleared.Command)
	}
	if cleared.Entrypoint == nil || len(cleared.Entrypoint) != 0 {
		t.Fatalf("cleared.Entrypoint = %#v, want a non-nil empty slice (explicit override)", cleared.Entrypoint)
	}
	unset := byName["unset"]
	if unset.Command != nil {
		t.Fatalf("unset.Command = %#v, want nil (field never set)", unset.Command)
	}
	if unset.Entrypoint != nil {
		t.Fatalf("unset.Entrypoint = %#v, want nil (field never set)", unset.Entrypoint)
	}
}

// TestParseFileHonorsUserWorkingDirReadOnlyTmpfsShmSizeInit covers a
// finding from Codex review of PR #131: these six fields are
// classified as allowed (see allowedFields), so ParseFile must actually
// represent each of them on ServiceSpec -- being allow-listed only
// means "not rejected outright"; a
// service using one of them must not have it silently dropped between
// validation and what actually gets synthesized (see synthesize_test.go
// for the matching Synthesize-side coverage).
func TestParseFileHonorsUserWorkingDirReadOnlyTmpfsShmSizeInit(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    user: "1000:1000"
    working_dir: /srv/app
    read_only: true
    tmpfs:
      - /tmp
      - /run
    shm_size: 134217728
    init: true
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	s := services[0]
	if s.User != "1000:1000" {
		t.Errorf("User = %q, want %q", s.User, "1000:1000")
	}
	if s.WorkingDir != "/srv/app" {
		t.Errorf("WorkingDir = %q, want %q", s.WorkingDir, "/srv/app")
	}
	if !s.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
	if len(s.Tmpfs) != 2 || s.Tmpfs[0] != "/tmp" || s.Tmpfs[1] != "/run" {
		t.Errorf("Tmpfs = %v, want [/tmp /run]", s.Tmpfs)
	}
	if s.ShmSize != 134217728 {
		t.Errorf("ShmSize = %d, want %d", s.ShmSize, 134217728)
	}
	if !s.Init {
		t.Error("Init = false, want true")
	}
}

// TestParseFilePreservesNamedVolumeSourceAndTmpfsType covers a finding
// from Codex review of PR #131: a named volume's own source name and a
// `volumes:`-declared tmpfs mount's own type must survive ParseFile, not
// just its target path, so Synthesize can keep two services' shared named
// volume actually shared and a tmpfs mount actually tmpfs -- see
// VolumeMount's own doc comment.
func TestParseFilePreservesNamedVolumeSourceAndTmpfsType(t *testing.T) {
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    volumes:
      - pgdata:/var/lib/postgresql/data
      - type: tmpfs
        target: /tmp/scratch
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 || len(services[0].NamedVolumes) != 2 {
		t.Fatalf("expected 1 service with 2 volume mounts, got %v", services)
	}
	byTarget := map[string]VolumeMount{}
	for _, v := range services[0].NamedVolumes {
		byTarget[v.Target] = v
	}
	vol := byTarget["/var/lib/postgresql/data"]
	if vol.Type != "volume" || vol.Source != "pgdata" {
		t.Errorf("named volume = %+v, want Type=volume Source=pgdata", vol)
	}
	tmpfs := byTarget["/tmp/scratch"]
	if tmpfs.Type != "tmpfs" {
		t.Errorf("tmpfs mount = %+v, want Type=tmpfs", tmpfs)
	}
}

// TestParseFileUsesOnlyTheSuppliedEnvFile covers a design rule:
// interpolation must be answered only from Options.EnvFileContent,
// never from the process's own environment.
func TestParseFileUsesOnlyTheSuppliedEnvFile(t *testing.T) {
	t.Setenv("SHOULD_NOT_LEAK", "leaked-from-os-environ")
	doc := `
services:
  svc:
    image: docker.io/library/redis:7
    environment:
      FROM_ENV_FILE: ${FROM_ENV_FILE}
      FROM_OS_ENVIRON: ${SHOULD_NOT_LEAK}
`
	opts := Options{
		AllowedImageRegistries: []string{"docker.io/library/"},
		EnvFileContent:         []byte("FROM_ENV_FILE=from-the-env-file\n"),
	}
	services, rejected, _, err := ParseFile([]byte(doc), opts)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	env := services[0].Environment
	if env["FROM_ENV_FILE"] != "from-the-env-file" {
		t.Errorf("FROM_ENV_FILE = %q, want %q", env["FROM_ENV_FILE"], "from-the-env-file")
	}
	if env["FROM_OS_ENVIRON"] != "" {
		t.Errorf("FROM_OS_ENVIRON = %q, want empty -- os.Environ() must never be consulted", env["FROM_OS_ENVIRON"])
	}
}

// TestParseFilePreservesExplicitDependsOnCondition covers a finding from
// Codex review of PR #131, round 2: a long-form depends_on entry's own
// explicit condition (here, service_completed_successfully, the shape a
// migration job needs) must survive ParseFile unchanged, not be discarded
// down to just the dependency's name.
func TestParseFilePreservesExplicitDependsOnCondition(t *testing.T) {
	doc := `
services:
  migrate:
    image: docker.io/library/myapp-migrate:1
  app:
    image: docker.io/library/myapp:1
    depends_on:
      migrate:
        condition: service_completed_successfully
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	var app ServiceSpec
	for _, s := range services {
		if s.Name == "app" {
			app = s
		}
	}
	if len(app.DependsOn) != 1 || app.DependsOn[0].Name != "migrate" || app.DependsOn[0].Condition != "service_completed_successfully" {
		t.Fatalf("app.DependsOn = %+v, want [{migrate service_completed_successfully}]", app.DependsOn)
	}
}

// TestParseFileShortFormDependsOnDefaultsToServiceStarted covers the
// other half of the same PR #131 round-2 finding: a short-form
// `depends_on: [name]` entry has no
// condition of its own in the source file, but compose-go's own loader
// already defaults it to service_started before ParseFile ever sees it
// (see ServiceSpec.DependsOn's own doc comment) -- so ParseFile must keep
// that default, not (as before this fix) discard it and have Synthesize
// re-derive one from healthcheck presence later.
func TestParseFileShortFormDependsOnDefaultsToServiceStarted(t *testing.T) {
	doc := `
services:
  db:
    image: docker.io/library/postgres:16
    healthcheck:
      test: ["CMD-SHELL", "pg_isready"]
  app:
    image: docker.io/library/myapp:1
    depends_on:
      - db
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	var app ServiceSpec
	for _, s := range services {
		if s.Name == "app" {
			app = s
		}
	}
	if len(app.DependsOn) != 1 || app.DependsOn[0].Name != "db" || app.DependsOn[0].Condition != "service_started" {
		t.Fatalf("app.DependsOn = %+v, want [{db service_started}]", app.DependsOn)
	}
}

// TestParseFileRejectsUnsupportedDependsOnCondition covers ParseFile's own
// fail-closed handling of a condition value outside the three the compose
// spec documents: compose-go's own loader already refuses this at load
// time (confirmed here as a whole-file rejection), and parseOneService's
// own allowedDependsOnConditions check backs that up defensively should a
// future compose-go version ever loosen its own validation.
func TestParseFileRejectsUnsupportedDependsOnCondition(t *testing.T) {
	doc := `
services:
  db:
    image: docker.io/library/postgres:16
  app:
    image: docker.io/library/myapp:1
    depends_on:
      db:
        condition: service_made_up
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("expected no services returned once the unsupported condition is rejected, got %v", services)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "condition") {
		t.Fatalf("rejected = %v, want a rejection naming the unsupported condition", rejected)
	}
}

// TestParseFilePreservesDisabledHealthcheck covers a finding from Codex
// review of PR #131, round 2: `healthcheck: {disable: true}` must survive
// ParseFile as an explicitly-disabled Healthcheck, not a non-nil
// Healthcheck with no test (which Synthesize would otherwise turn into a
// `test` field the source file never declared).
func TestParseFilePreservesDisabledHealthcheck(t *testing.T) {
	doc := `
services:
  web:
    image: docker.io/library/myapp:1
    healthcheck:
      disable: true
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 || services[0].Healthcheck == nil || !services[0].Healthcheck.Disabled {
		t.Fatalf("expected service with a disabled healthcheck, got %+v", services)
	}
}

// TestParseFileAllowsAliasPortExtension covers the one narrow "x-*"
// extension key this package reads: "x-bg-service-port" overrides
// ServiceSpec.AliasPort, used when a service's own `ports:`-derived
// container port isn't the one a container-network client should connect
// to (e.g. Kafka's internal-only listener).
func TestParseFileAllowsAliasPortExtension(t *testing.T) {
	doc := `
services:
  kafka:
    image: docker.io/library/kafka:3
    ports:
      - "9092"
    x-bg-service-port: 19092
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 || services[0].AliasPort != 19092 {
		t.Fatalf("expected AliasPort 19092, got %+v", services)
	}
}

// TestParseFileRejectsMalformedAliasPortExtension covers both a
// non-numeric value and an out-of-range port: neither is silently
// ignored, each rejects the whole service with a clear reason.
func TestParseFileRejectsMalformedAliasPortExtension(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"not a number", `"not-a-number"`},
		{"out of range", "99999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `
services:
  kafka:
    image: docker.io/library/kafka:3
    x-bg-service-port: ` + tc.value + `
`
			_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "x-bg-service-port") {
				t.Fatalf("expected a rejection naming x-bg-service-port, got %v", rejected)
			}
		})
	}
}

// TestParseFileRejectsOtherExtensionKeys proves this change is a narrow,
// specific exception, not "allow all x- extensions": every extension key
// other than x-bg-service-port must still be rejected exactly as before.
func TestParseFileRejectsOtherExtensionKeys(t *testing.T) {
	doc := `
services:
  web:
    image: docker.io/library/myapp:1
    x-some-other-extension: true
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "custom \"x-\" extension fields are unreviewed") {
		t.Fatalf("expected the standard extensions rejection, got %v", rejected)
	}
}

// TestParseFileRequireDigestRejectsTaggedImage proves the opt-in
// RequireDigest option (sessionconfig's compose_services_require_digest)
// fails closed on an otherwise-allowed image that names only a tag,
// naming the image and the fix without needing any secret-shaped value in
// the reason string.
func TestParseFileRequireDigestRejectsTaggedImage(t *testing.T) {
	doc := `
services:
  postgres:
    image: docker.io/library/postgres:16.4
`
	opts := Options{AllowedImageRegistries: []string{"docker.io/library/"}, RequireDigest: true}
	services, rejected, _, err := ParseFile([]byte(doc), opts)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("expected the service to be rejected, got %+v", services)
	}
	if len(rejected) != 1 {
		t.Fatalf("expected exactly 1 rejection, got %v", rejected)
	}
	if !strings.Contains(rejected[0].Reason, "docker.io/library/postgres:16.4") || !strings.Contains(rejected[0].Reason, "not pinned by digest") {
		t.Fatalf("unexpected rejection reason: %q", rejected[0].Reason)
	}
}

// TestParseFileRequireDigestAllowsDigestPinnedImage proves the same option
// accepts an image already pinned by digest, tag alongside the digest
// included (Docker itself resolves that form by digest, ignoring the tag).
func TestParseFileRequireDigestAllowsDigestPinnedImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	doc := `
services:
  postgres:
    image: docker.io/library/postgres:16.4@` + digest + `
`
	opts := Options{AllowedImageRegistries: []string{"docker.io/library/"}, RequireDigest: true}
	services, rejected, _, err := ParseFile([]byte(doc), opts)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 || services[0].Image != "docker.io/library/postgres:16.4@"+digest {
		t.Fatalf("unexpected services: %+v", services)
	}
}

// TestParseFileRequireDigestOffByDefault proves the zero-value Options
// (RequireDigest unset) keeps today's default behavior -- a tagged image
// is still allowed -- so this option is genuinely opt-in.
func TestParseFileRequireDigestOffByDefault(t *testing.T) {
	doc := `
services:
  postgres:
    image: docker.io/library/postgres:16.4
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("unexpected rejections: %v", rejected)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %+v", services)
	}
}

// TestParseFileAllowsAliasPortExtensionAlongsideRejectedOtherKey proves the
// alias-port key doesn't widen the allow-list for a service that also sets
// some other, still-unreviewed x-* key -- the whole service is rejected.
func TestParseFileAllowsAliasPortExtensionAlongsideRejectedOtherKey(t *testing.T) {
	doc := `
services:
  kafka:
    image: docker.io/library/kafka:3
    x-bg-service-port: 19092
    x-some-other-extension: true
`
	_, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "custom \"x-\" extension fields are unreviewed") {
		t.Fatalf("expected the standard extensions rejection, got %v", rejected)
	}
}

// A service's own mem_limit may only narrow the operator's
// compose_services_memory (Options.MemoryCeiling), never widen it.
func TestParseFileMemLimitOnlyNarrows(t *testing.T) {
	parse := func(memLimit, ceiling string) ([]ServiceSpec, []Rejection) {
		t.Helper()
		doc := "services:\n  kafka:\n    image: redis:7\n    mem_limit: " + memLimit + "\n"
		opts := allowDockerHub
		opts.MemoryCeiling = ceiling
		services, rejected, _, err := ParseFile([]byte(doc), opts)
		if err != nil {
			t.Fatalf("ParseFile: %v", err)
		}
		return services, rejected
	}

	for _, memLimit := range []string{"256m", "512m"} {
		services, rejected := parse(memLimit, "512m")
		want, _ := MemoryBytes(memLimit)
		if len(rejected) != 0 || len(services) != 1 || services[0].MemLimit != want {
			t.Errorf("mem_limit %s under a 512m ceiling: services = %+v, rejected = %v; want accepted with MemLimit %d", memLimit, services, rejected, want)
		}
	}

	_, rejected := parse("1g", "512m")
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "above the operator's compose_services_memory 512m") {
		t.Errorf("mem_limit 1g under a 512m ceiling: rejected = %v, want a rejection naming the ceiling", rejected)
	}

	_, rejected = parse("256m", "")
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "not a usable ceiling") {
		t.Errorf("mem_limit with no ceiling configured: rejected = %v, want fail-closed rejection", rejected)
	}
}

// TestParseFileKeepsPublishedTCPPorts: the host side of a fixed TCP mapping
// is kept for the worker's loopback forwards (ranges arrive expanded);
// a random host port or a UDP mapping has nothing to forward.
func TestParseFileKeepsPublishedTCPPorts(t *testing.T) {
	doc := `
services:
  web:
    image: docker.io/library/nginx:1
    ports:
      - "8000-8001:9000-9001"
      - "127.0.0.1:5433:5432"
      - "6379"
      - "53:53/udp"
`
	services, rejected, _, err := ParseFile([]byte(doc), allowDockerHub)
	if err != nil || len(rejected) != 0 || len(services) != 1 {
		t.Fatalf("ParseFile = %v, rejected %v, err %v", services, rejected, err)
	}
	want := []PublishedPort{{Host: 8000, Target: 9000}, {Host: 8001, Target: 9001}, {Host: 5433, Target: 5432}}
	if !reflect.DeepEqual(services[0].PublishedPorts, want) {
		t.Fatalf("PublishedPorts = %+v, want %+v", services[0].PublishedPorts, want)
	}
}

// TestRejectionNamesTheRegistryThatWouldAdmitTheImage: only a service
// rejected for its image carries AllowRegistry, and it is the narrow
// namespace, never the whole registry of a namespaced image.
func TestRejectionNamesTheRegistryThatWouldAdmitTheImage(t *testing.T) {
	compose := "services:\n  kafka:\n    image: apache/kafka:3.8.0\n  search:\n    image: ghcr.io/acme/search:1\n  db:\n    image: postgres:16\n    privileged: true\n"
	_, rejected, _, err := ParseFile([]byte(compose), Options{AllowedImageRegistries: []string{"docker.io/library/"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rejected {
		got[r.Service] = r.AllowRegistry
	}
	want := map[string]string{"kafka": "docker.io/apache/", "search": "ghcr.io/acme/", "db": ""}
	for service, registry := range want {
		if got[service] != registry {
			t.Errorf("AllowRegistry for %s = %q, want %q (all: %v)", service, got[service], registry, got)
		}
	}
}
