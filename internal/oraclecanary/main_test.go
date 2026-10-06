package oraclecanary

import (
	"os"
	"testing"
	"time"
)

// TestMain raises typeCheckTimeout for this package's tests: they assert what
// the type check reports, not how fast it runs, and a full-suite run can load
// the machine enough for the production 60s bound to skip them.
func TestMain(m *testing.M) {
	typeCheckTimeout = 10 * time.Minute
	os.Exit(m.Run())
}
