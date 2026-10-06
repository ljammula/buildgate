package pgstore

import "testing"

// TestLoadMigrationsAreContiguous guards the embedded migrations/ directory
// itself: a file with a bad prefix, a duplicate or a gap fails here, in
// `go test ./...`, not at a service's startup.
func TestLoadMigrationsAreContiguous(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("expected at least one migration")
	}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Fatalf("migration %d is %s (version %d)", i, m.name, m.version)
		}
		if m.sql == "" {
			t.Fatalf("migration %s is empty", m.name)
		}
	}
}
