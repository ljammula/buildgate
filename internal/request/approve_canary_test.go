package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Approval refuses, before the build, an oracle directory no canary can be
// built for -- it would otherwise only fail at the reference_oracle gate after a
// full build. The error names the offending file and what the canary supports;
// the request stays in plan_review.
func TestApproveRefusesOracleDirTheCanaryCannotBeBuiltFor(t *testing.T) {
	cases := map[string]struct {
		extra    map[string]string
		mention  string
		hasValid bool
	}{
		"fixture file":                   {map[string]string{"fixture.json": "{}"}, "fixture.json", true},
		"helper test without TestOracle": {map[string]string{"helper_test.go": "package x\n\nimport \"testing\"\n\nfunc TestHelper(t *testing.T) {}\n"}, "helper_test.go", true},
		"mixed ecosystems":               {map[string]string{"test_oracle_a.py": "def test_a():\n    pass\n"}, "mixes ecosystems", true},
		"no test files":                  {nil, "no test files", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			newApprovableRequest(t, dataDir, "req-1", StatePlanReview, true)
			oracleDir := filepath.Join(Dir(dataDir, "req-1"), "tickets", "001.oracle")
			if err := os.MkdirAll(oracleDir, 0o750); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{TicketOracleRunCommandFilename: "go test ./.oracle/...\n"}
			if c.hasValid {
				files["x_oracle_test.go"] = "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"
			}
			for n, body := range c.extra {
				files[n] = body
			}
			for n, body := range files {
				if err := os.WriteFile(filepath.Join(oracleDir, n), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
			if err == nil {
				t.Fatal("approval accepted an oracle directory no canary can be built for")
			}
			if !strings.Contains(err.Error(), c.mention) || !strings.Contains(err.Error(), "supported:") {
				t.Errorf("error = %v, want it to name %q and say what the canary supports", err, c.mention)
			}
			loaded, _ := Load(dataDir, "req-1")
			if loaded.State != StatePlanReview {
				t.Errorf("state = %q, want unchanged plan_review", loaded.State)
			}
		})
	}
}
