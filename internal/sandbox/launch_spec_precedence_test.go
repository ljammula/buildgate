package sandbox

import (
	"fmt"
	"testing"
)

// launchSpecViolations is one edit per rule of LaunchSpec.Validate, in the
// order Validate reports them. field names what the edit overwrites: two
// edits of one field cannot be combined.
var launchSpecViolations = []struct {
	name  string
	field string
	edit  func(*LaunchSpec)
}{
	{"missing image", "image", func(s *LaunchSpec) { s.Image = "" }},
	{"unpinned image", "image", func(s *LaunchSpec) { s.Image = "factory-worker:test" }},
	{"missing command", "command", func(s *LaunchSpec) { s.Command = nil }},
	{"missing work dir", "workdir", func(s *LaunchSpec) { s.WorkDir = "" }},
	{"relative work dir", "workdir", func(s *LaunchSpec) { s.WorkDir = "workspace" }},
	{"relative git common dir", "git", func(s *LaunchSpec) { s.gitCommonDir, s.gitCommonDirTarget = "git", "/git" }},
	{"relative git common dir target", "git", func(s *LaunchSpec) { s.gitCommonDir, s.gitCommonDirTarget = "/git", "git" }},
	{"git common dir without target", "git", func(s *LaunchSpec) { s.gitCommonDir, s.gitCommonDirTarget = "/git", "" }},
	{"relative input dir", "inputdir", func(s *LaunchSpec) { s.InputDir = "inputs" }},
	{"relative oracle dir", "oracle", func(s *LaunchSpec) { s.ReferenceOracleDir, s.ReferenceOracleMountPath = "oracle", "oracle" }},
	{"oracle dir without mount path", "oracle", func(s *LaunchSpec) { s.ReferenceOracleDir, s.ReferenceOracleMountPath = "/tmp/oracle", "" }},
	{"absolute oracle mount path", "oracle", func(s *LaunchSpec) { s.ReferenceOracleDir, s.ReferenceOracleMountPath = "/tmp/oracle", "/oracle" }},
	{"oracle dir inside workspace", "oracle", func(s *LaunchSpec) {
		s.ReferenceOracleDir, s.ReferenceOracleMountPath = "/tmp/workspace/oracle", "oracle"
	}},
	{"relative scratch dir", "scratch", func(s *LaunchSpec) { s.ScratchDir = "scratch" }},
	{"scratch dir inside workspace", "scratch", func(s *LaunchSpec) { s.ScratchDir = "/tmp/workspace/scratch" }},
	{"invalid input mount", "inputs", func(s *LaunchSpec) { s.Inputs = []InputMount{{Source: "/tmp/in", Target: "../in"}} }},
	{"missing log path", "logpath", func(s *LaunchSpec) { s.LogPath = "" }},
	{"missing name", "name", func(s *LaunchSpec) { s.Name = "" }},
	{"run id with tab", "runid", func(s *LaunchSpec) { s.RunID = "run\t1" }},
	{"relative data dir", "datadir", func(s *LaunchSpec) { s.DataDir = "data" }},
	{"root user", "user", func(s *LaunchSpec) { s.User = "0" }},
	{"bad umask", "umask", func(s *LaunchSpec) { s.WorkerUmask = "9" }},
	{"unsafe name", "name", func(s *LaunchSpec) { s.Name = "a/b" }},
	{"zero timeout", "timeout", func(s *LaunchSpec) { s.Timeout = 0 }},
	{"missing memory limit", "memory", func(s *LaunchSpec) { s.Memory = "" }},
	{"missing network", "network", func(s *LaunchSpec) { s.Network = "" }},
	{"host network", "network", func(s *LaunchSpec) { s.Network = "host" }},
	{"relay egress network", "network", func(s *LaunchSpec) { s.Network = relayEgressNetworkName }},
	{"registry proxy egress network", "network", func(s *LaunchSpec) { s.Network = registryProxyEgressNetworkName }},
	{"unlisted network", "network", func(s *LaunchSpec) { s.Network = "bridge" }},
	{"unlisted compose network", "compose", func(s *LaunchSpec) { s.ComposeNetwork = "other" }},
	{"unrecorded entry without value", "", func(s *LaunchSpec) {
		s.UnrecordedEnvironment = append(s.UnrecordedEnvironment, "NOVALUE")
	}},
	{"unrecorded reserved key", "", func(s *LaunchSpec) {
		s.UnrecordedEnvironment = append(s.UnrecordedEnvironment, "PATH=/bin")
	}},
	{"unrecorded key the factory sets", "", func(s *LaunchSpec) {
		s.Environment = append(s.Environment, "SHARED=1")
		s.UnrecordedEnvironment = append(s.UnrecordedEnvironment, "SHARED=2")
	}},
	{"environment entry without value", "", func(s *LaunchSpec) { s.Environment = append(s.Environment, "NOVALUE") }},
	{"environment entry without key", "", func(s *LaunchSpec) { s.Environment = append(s.Environment, "=value") }},
	{"environment git config redirect", "", func(s *LaunchSpec) {
		s.Environment = append(s.Environment, "GIT_CONFIG_GLOBAL=/tmp/gitconfig")
	}},
	{"environment real API key", "", func(s *LaunchSpec) {
		s.Environment = append(s.Environment, "ANTHROPIC_API_KEY=real")
	}},
}

// TestLaunchSpecValidateReportsTheFirstViolatedRule pins the precedence of
// LaunchSpec.Validate: a spec that breaks two rules is refused with the
// error of the rule listed first in launchSpecViolations, the same error
// that rule gives alone.
func TestLaunchSpecValidateReportsTheFirstViolatedRule(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
	alone := make([]string, len(launchSpecViolations))
	seen := map[string]string{}
	for i, v := range launchSpecViolations {
		spec := validSpec()
		v.edit(&spec)
		err := spec.Validate()
		if err == nil {
			t.Fatalf("%s: accepted", v.name)
		}
		alone[i] = err.Error()
		if other, ok := seen[alone[i]]; ok {
			t.Fatalf("%s and %s give the same error %q, so the pairs below could not tell them apart", v.name, other, alone[i])
		}
		seen[alone[i]] = v.name
	}
	for i, first := range launchSpecViolations {
		for j := i + 1; j < len(launchSpecViolations); j++ {
			second := launchSpecViolations[j]
			if first.field != "" && first.field == second.field {
				continue
			}
			t.Run(fmt.Sprintf("%s+%s", first.name, second.name), func(t *testing.T) {
				spec := validSpec()
				first.edit(&spec)
				second.edit(&spec)
				err := spec.Validate()
				if err == nil || err.Error() != alone[i] {
					t.Fatalf("got %v, want the first rule's error %q", err, alone[i])
				}
			})
		}
	}
}
