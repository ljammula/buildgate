package sessionconfig

import (
	"strings"
	"testing"
)

// TestApplySettingsDockerContext: docker_context reaches Settings, an absent
// or empty key leaves it empty, and a name the docker CLI would not have is
// refused with the key named.
func TestApplySettingsDockerContext(t *testing.T) {
	cases := []struct {
		name, yaml, want, refused string
	}{
		{"unset", "sandbox_docker: docker\n", "", ""},
		{"empty", "docker_context: \"\"\n", "", ""},
		{"colima profile", "docker_context: colima-buildgate\n", "colima-buildgate", ""},
		{"dots and plus", "docker_context: ci.host+1_x\n", "ci.host+1_x", ""},
		{"leading dash", "docker_context: -x\n", "", "docker_context"},
		{"slash", "docker_context: a/b\n", "", "docker_context"},
		{"space", "docker_context: \"a b\"\n", "", "docker_context"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, c.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			settings, err := cfg.ApplySettings(DefaultSettings())
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("ApplySettings = %v, want a refusal naming %s", err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplySettings: %v", err)
			}
			if settings.DockerContext != c.want {
				t.Errorf("DockerContext = %q, want %q", settings.DockerContext, c.want)
			}
		})
	}
}
