package request

import (
	"testing"

	"buildgate/internal/oraclecanary"
)

// The generated multi-file Go command is directory-scoped: it names no mounted
// file, so every ticket subset passes the scope check, and it satisfies the
// static floor a hand-written command must meet.
func TestGeneratedMultiFileGoCommandFitsEveryTicketSubset(t *testing.T) {
	cmd, err := oraclecanary.GoMultiCommand("svc", []oraclecanary.GoOracleFile{
		{Name: "a_oracle_test.go", PkgDir: "p"},
		{Name: "b_oracle_test.go", PkgDir: "."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateOracleRunCommand(cmd); err != nil {
		t.Errorf("ValidateOracleRunCommand: %v", err)
	}
	for _, subset := range [][]string{{"a_oracle_test.go"}, {"b_oracle_test.go"}, {"a_oracle_test.go", "b_oracle_test.go"}} {
		if err := checkRunCommandScope(cmd, subset); err != nil {
			t.Errorf("subset %v: %v", subset, err)
		}
	}
}
