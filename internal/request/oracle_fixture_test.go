package request

import (
	"os"
	"path/filepath"
	"testing"
)

// writeValidOracleTest adds an oracle test file the runtime canary can be built
// for (approval refuses a directory it cannot).
func writeValidOracleTest(t *testing.T, oracleDir string) {
	t.Helper()
	body := "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(oracleDir, "x_oracle_test.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
