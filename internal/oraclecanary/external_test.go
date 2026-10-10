package oraclecanary_test

import (
	"strings"
	"testing"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/request"
)

// internal/request imports oraclecanary (approval refuses an oracle directory no
// canary can be built for), so these request-dependent checks live in the
// external test package.
func TestCommandsPassValidateOracleRunCommand(t *testing.T) {
	goCmd, err := oraclecanary.GoCommand("svc", "internal/mood", "mood_oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}
	cmds := map[string]string{
		"go": goCmd, "python": "pytest --rootdir . --import-mode=importlib .oracle", "vitest": "vitest run .oracle",
		"jest": "jest --rootDir . --roots .oracle", "dart": "flutter test .oracle/",
		"python-stdlib": oraclecanary.PythonStdlibCommand(),
		// The vacuous command passes the static check: that is why the runtime
		// canary exists.
		"vacuous": "echo .oracle && cd svc && go test ./...",
	}
	for name, c := range cmds {
		if err := request.ValidateOracleRunCommand(c); err != nil {
			t.Errorf("%s command %q refused: %v", name, c, err)
		}
	}
}

func TestMountPathMatchesRequestPackage(t *testing.T) {
	cmd := oraclecanary.PythonStdlibCommand()
	if want := `glob.glob("` + request.TicketOracleMountPath + `/test_oracle_*.py")`; !strings.Contains(cmd, want) {
		t.Errorf("PythonStdlibCommand = %q, want it to hold %q: the duplicated mount path drifted from request.TicketOracleMountPath", cmd, want)
	}
}
