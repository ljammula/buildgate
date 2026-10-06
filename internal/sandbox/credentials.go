package sandbox

import "strings"

// AnthropicAPIKeyPlaceholder is the only ANTHROPIC_API_KEY value a worker may
// be given (LaunchSpec.Validate): it satisfies a client that requires a
// non-empty API key and is not a credential.
const AnthropicAPIKeyPlaceholder = "factoryd-relay-placeholder"

// forbiddenCredentialPrefixes and forbiddenCredentialNames enumerate
// environment variable names that must never reach an untrusted worker.
// Every build_app.py/canonical-verification/full-suite invocation is
// sandboxed inside Docker unconditionally (LaunchSpec.Validate checks
// this set there; there is no host-execution opt-out for that path).
// internal/runner's fixed hostWorkerEnvBaseline allowlist, used only by
// the request driver's own unsandboxed spec-drafting/plan-drafting jobs,
// double-checks against this same set. Kept as one shared list, not one
// per call site, so the two enforcement points cannot silently drift
// apart the way they did before this file existed: a review found the
// sandboxed path forbade exactly this set while the host-execution path
// (since removed; see internal/runner's hostWorkerEnvBaseline doc
// comment) handed over every name in it.
var (
	forbiddenCredentialPrefixes = []string{
		"FACTORYD_API_",
		"DOCKER_",
		// GIT_CONFIG_KEY_/GIT_CONFIG_VALUE_ (plus the exact-name
		// GIT_CONFIG_COUNT below) are git's own env-based arbitrary-config
		// injection mechanism: GIT_CONFIG_COUNT=1
		// GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0='!...'
		// lets a caller define any git config key -- including a
		// credential helper or a hook-running alias -- with no file on
		// disk at all. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM are
		// deliberately NOT covered by this prefix (a review of
		// internal/runner's host-worker allowlist inversion found they only
		// redirect *which file* git reads as its global/system config,
		// carry no credential value of their own, and this repo's own
		// integration-test suite relies on setting both to /dev/null for
		// test isolation (see cmd/factoryd/integration_test.go). A prefix
		// broad enough to also catch those two would make that legitimate,
		// non-credential test-isolation pattern impossible to allowlist
		// without weakening this hard floor instead.
		"GIT_CONFIG_KEY_",
		"GIT_CONFIG_VALUE_",
		"CLOUDSDK_",
	}
	forbiddenCredentialNames = []string{
		"GIT_CONFIG_COUNT",
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
)

// IsForbiddenCredentialEnvKey reports whether key names a host credential
// that must never reach an untrusted worker process. ANTHROPIC_API_KEY is
// deliberately not included here: a sandboxed worker is allowed to see it
// only as AnthropicAPIKeyPlaceholder (checked separately by callers that
// also validate the value, e.g. LaunchSpec.Validate), which is not "always
// forbidden by name" the way every key here is.
func IsForbiddenCredentialEnvKey(key string) bool {
	for _, prefix := range forbiddenCredentialPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, name := range forbiddenCredentialNames {
		if key == name {
			return true
		}
	}
	return false
}
