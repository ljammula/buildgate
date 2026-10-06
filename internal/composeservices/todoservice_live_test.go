package composeservices

import (
	"os"
	"strings"
	"testing"
)

// todoServiceComposeFile is the real docker-compose.yml of the
// todo-kafka-service fixture (a small Go service with genuine Postgres,
// Kafka and Redis dependencies) that make live-compose builds against.
const todoServiceComposeFile = "../../testdata/fixtures/todo-kafka-service/docker-compose.yml"

// TestParseFileAgainstTodoServiceLive is not a unit test in the usual
// sense -- it runs ParseFile against that fixture's compose file, to
// validate this package's design against something real rather than only
// synthetic fixtures.
func TestParseFileAgainstTodoServiceLive(t *testing.T) {
	data, err := os.ReadFile(todoServiceComposeFile)
	if err != nil {
		t.Fatal(err)
	}

	// docker.io/library/ covers postgres and redis (official images);
	// docker.io/ (the bare Docker Hub namespace) additionally covers
	// kafka's own apache/kafka, a non-"library" Docker Hub image -- see
	// that repo's docker-compose.yml: postgres, a Kafka broker, and redis
	// (the API's cache and idempotency store).
	services, rejected, skipped, err := ParseFile(data, Options{AllowedImageRegistries: []string{"docker.io/"}})
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("expected todo-service's clean compose file to have no rejections, got: %v", rejected)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected todo-service's clean compose file to have no skips, got: %v", skipped)
	}
	if len(services) != 3 {
		t.Fatalf("expected exactly 3 allowed services, got %d: %+v", len(services), services)
	}

	byName := make(map[string]ServiceSpec, len(services))
	for _, s := range services {
		byName[s.Name] = s
	}

	postgres, ok := byName["postgres"]
	if !ok {
		t.Fatalf("expected a postgres service, got %+v", services)
	}
	if postgres.Image != "postgres:16.4" {
		t.Errorf("postgres Image = %q, want %q", postgres.Image, "postgres:16.4")
	}
	if postgres.Environment["POSTGRES_USER"] != "todo" || postgres.Environment["POSTGRES_DB"] != "todo" {
		t.Errorf("postgres Environment = %+v, missing expected POSTGRES_USER/POSTGRES_DB", postgres.Environment)
	}
	if len(postgres.Ports) != 1 || postgres.Ports[0] != "5432" {
		// The compose file's own "5433:5432" is a host port mapping for
		// local dev convenience -- the provisioner never publishes to the
		// host regardless, so only the container-side "5432" survives.
		t.Errorf("postgres Ports = %v, want [5432] (host side of a mapping is stripped, not kept)", postgres.Ports)
	}
	if postgres.Healthcheck == nil || postgres.Healthcheck.Retries != 15 {
		t.Errorf("postgres Healthcheck = %+v, want Retries=15", postgres.Healthcheck)
	}
	if len(postgres.NamedVolumes) != 1 || postgres.NamedVolumes[0].Target != "/var/lib/postgresql/data" {
		t.Errorf("postgres NamedVolumes = %v, want target /var/lib/postgresql/data", postgres.NamedVolumes)
	}

	kafka, ok := byName["kafka"]
	if !ok {
		t.Fatalf("expected a kafka service, got %+v", services)
	}
	if kafka.Image != "apache/kafka:3.8.0" {
		t.Errorf("kafka Image = %q, want %q", kafka.Image, "apache/kafka:3.8.0")
	}
	if kafka.Hostname != "kafka" {
		t.Errorf("kafka Hostname = %q, want %q", kafka.Hostname, "kafka")
	}
	if kafka.Healthcheck == nil {
		t.Error("kafka Healthcheck = nil, want a healthcheck")
	}

	redis, ok := byName["redis"]
	if !ok {
		t.Fatalf("expected a redis service, got %+v", services)
	}
	if redis.Image != "redis:7.4" {
		t.Errorf("redis Image = %q, want %q", redis.Image, "redis:7.4")
	}
	if len(redis.Ports) != 1 || redis.Ports[0] != "6379" {
		t.Errorf("redis Ports = %v, want [6379] (host side of a mapping is stripped, not kept)", redis.Ports)
	}
	if redis.Healthcheck == nil {
		t.Error("redis Healthcheck = nil, want a healthcheck")
	}

	t.Logf("allowed: postgres image=%s ports=%v; kafka image=%s hostname=%s; redis image=%s ports=%v", postgres.Image, postgres.Ports, kafka.Image, kafka.Hostname, redis.Image, redis.Ports)
}

// TestParseFileRejectsAHostileAdditionToTodoService takes the real
// todo-service compose file and appends a service an attacker (or a
// compromised worker editing its own repo -- see this package's own doc
// comment on provenance) would want: an unlisted-registry image demanding
// privileged mode. Demonstrates the reject path against real surrounding
// content, not just an isolated synthetic fixture.
func TestParseFileRejectsAHostileAdditionToTodoService(t *testing.T) {
	data, err := os.ReadFile(todoServiceComposeFile)
	if err != nil {
		t.Fatal(err)
	}

	// Insert under the real "services:" block, before the file's own
	// trailing top-level "volumes:" section -- appending at the very end
	// would land the new block under "volumes:" instead, since YAML
	// structure is positional. This mirrors how a worker actually editing
	// docker-compose.yml would add a sibling service.
	const marker = "\nvolumes:\n"
	idx := strings.Index(string(data), marker)
	if idx == -1 {
		t.Fatalf("fixture %s does not contain expected %q marker", todoServiceComposeFile, marker)
	}
	hostile := string(data[:idx]) + "  attacker:\n    image: evil.example.com/payload:latest\n    privileged: true\n" + string(data[idx:])

	services, rejected, _, err := ParseFile([]byte(hostile), Options{AllowedImageRegistries: []string{"docker.io/"}})
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(services) != 3 {
		t.Fatalf("expected postgres, kafka and redis to survive, got %+v", services)
	}
	for _, s := range services {
		if s.Name == "attacker" {
			t.Fatalf("attacker service should have been rejected, not allowed: %+v", s)
		}
	}
	if len(rejected) != 1 || rejected[0].Service != "attacker" {
		t.Fatalf("expected only the attacker service to be rejected, got %v", rejected)
	}
	t.Logf("rejected: %s", rejected[0])
}
