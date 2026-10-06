package sandbox

import "testing"

func TestIsForbiddenCredentialEnvKey(t *testing.T) {
	forbidden := []string{
		"FACTORYD_API_START_TOKEN",
		"FACTORYD_API_OVERRIDE_TOKEN",
		"DOCKER_HOST",
		"GIT_CONFIG_COUNT",
		"CLOUDSDK_CORE_ACCOUNT",
		"SSH_AUTH_SOCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"OPENAI_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"AZURE_CLIENT_SECRET",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GITLAB_TOKEN",
		"NPM_TOKEN",
		"HF_TOKEN",
		"ANTHROPIC_AUTH_TOKEN",
		"KUBECONFIG",
	}
	for _, key := range forbidden {
		if !IsForbiddenCredentialEnvKey(key) {
			t.Errorf("IsForbiddenCredentialEnvKey(%q) = false, want true", key)
		}
	}

	allowed := []string{"PATH", "HOME", "LANG", "TZ", "SOME_OTHER_VAR", "ANTHROPIC_API_KEY"}
	for _, key := range allowed {
		if IsForbiddenCredentialEnvKey(key) {
			t.Errorf("IsForbiddenCredentialEnvKey(%q) = true, want false", key)
		}
	}
}
