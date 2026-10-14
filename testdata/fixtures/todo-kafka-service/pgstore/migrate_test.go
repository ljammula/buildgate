package pgstore

import (
	"sort"
	"strconv"
	"strings"
	"testing"
)

// migrationVersions validates the embedded golang-migrate file pairs and
// returns their ordered versions.
func migrationVersions(t *testing.T) []int {
	t.Helper()
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	pairs := map[int]map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		kind := ""
		if strings.HasSuffix(name, ".up.sql") {
			kind = "up"
		} else if strings.HasSuffix(name, ".down.sql") {
			kind = "down"
		} else {
			t.Fatalf("migration %q must end in .up.sql or .down.sql", name)
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".up.sql"), ".down.sql")
		prefix, description, ok := strings.Cut(base, "_")
		version, parseErr := strconv.Atoi(prefix)
		if !ok || parseErr != nil || len(prefix) != 6 || version <= 0 || description == "" {
			t.Fatalf("migration %q must be NNNNNN_description.%s.sql", name, kind)
		}
		if pairs[version] == nil {
			pairs[version] = map[string]bool{}
		}
		if pairs[version][kind] {
			t.Fatalf("duplicate %s migration for version %d", kind, version)
		}
		pairs[version][kind] = true
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil || len(strings.TrimSpace(string(body))) == 0 {
			t.Fatalf("migration %q is empty or unreadable: %v", name, err)
		}
	}
	if len(pairs) == 0 {
		t.Fatal("expected at least one migration")
	}
	versions := make([]int, 0, len(pairs))
	for version, pair := range pairs {
		if !pair["up"] || !pair["down"] {
			t.Fatalf("migration %d must have both up and down files", version)
		}
		versions = append(versions, version)
	}
	for version := 1; version <= len(versions); version++ {
		if pairs[version] == nil {
			t.Fatalf("missing migration version %d", version)
		}
	}
	sort.Ints(versions)
	return versions
}

func TestMigrationFilesAreContiguousUpDownPairs(t *testing.T) {
	versions := migrationVersions(t)
	if versions[len(versions)-1] != len(versions) {
		t.Fatalf("migration versions %v are not contiguous from 1", versions)
	}
}

func TestMigrateForceRejectsInvalidVersions(t *testing.T) {
	for _, version := range []int{-2, 0} {
		if err := MigrateForce(nil, version); err == nil {
			t.Errorf("MigrateForce should reject version %d", version)
		}
	}
}
