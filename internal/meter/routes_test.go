package meter

import (
	"strings"
	"testing"
)

// TestValidateGitHubCopilotRoute is the single source-of-truth
// accepted/refused table for ValidateGitHubCopilotRoute -- every caller
// (RoutePolicy.Validate in internal/sandbox, the session config's route
// validation and factoryd doctor's doctorValidateCopilotUpstream) goes
// through this one function, so this one table is what proves the pin
// itself is correct.
func TestValidateGitHubCopilotRoute(t *testing.T) {
	cases := []struct {
		name        string
		upstream    string
		wantErrLike string // substring expected in the error; "" means accept (err must be nil)
	}{
		{"individual plan host", "https://api.individual.githubcopilot.com", ""},
		{"business plan host", "https://api.business.githubcopilot.com", ""},
		{"enterprise plan host", "https://api.enterprise.githubcopilot.com", ""},
		{"bare apex domain", "https://githubcopilot.com", ""},
		{"mixed case host", "https://API.INDIVIDUAL.GITHUBCOPILOT.COM", ""},
		{"trailing DNS-root dot normalized away", "https://api.individual.githubcopilot.com.", ""},
		{"plaintext scheme", "http://api.individual.githubcopilot.com", "must be an https URL"},
		{"lookalike prefix domain", "https://evilgithubcopilot.com", "must be, or end with"},
		{"lookalike suffix domain", "https://githubcopilot.com.evil.com", "must be, or end with"},
		{"unrelated host", "https://api.anthropic.com", "must be, or end with"},
		{"userinfo masking a lookalike host", "https://api.individual.githubcopilot.com@evil.com", "must not embed userinfo"},
		{"userinfo over an otherwise-genuine host", "https://x@api.individual.githubcopilot.com", "must not embed userinfo"},
		{"GHE.com data-residency host refused (token exchange is github.com-only)", "https://copilot-api.acme-corp.ghe.com", "GHE.com Copilot data-residency tenant, which is not supported yet"},
		{"GHE.com host with two tenant labels (not GitHub's documented shape, refused as an ordinary lookalike)", "https://copilot-api.a.b.ghe.com", "must be, or end with"},
		{"empty upstream", "", "must be an https URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGitHubCopilotRoute(tc.upstream)
			if tc.wantErrLike == "" {
				if err != nil {
					t.Errorf("ValidateGitHubCopilotRoute(%q) = %v, want nil", tc.upstream, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrLike) {
				t.Errorf("ValidateGitHubCopilotRoute(%q) = %v, want an error containing %q", tc.upstream, err, tc.wantErrLike)
			}
		})
	}
}

// TestValidateChatGPTCodexRoute is TestValidateGitHubCopilotRoute's own
// counterpart for ValidateChatGPTCodexRoute: the single source-of-truth
// table for the exact-URL-plus-path pin, shared by the same three
// callers.
func TestValidateChatGPTCodexRoute(t *testing.T) {
	cases := []struct {
		name                              string
		upstream, allowedPrefix, basePath string
		wantErrLike                       string
	}{
		{"pinned upstream and path", ChatGPTCodexAPIBase, ChatGPTCodexResponsesPath, "", ""},
		{"pinned upstream with trailing slash", ChatGPTCodexAPIBase + "/", ChatGPTCodexResponsesPath, "", ""},
		{"non-Codex upstream", "http://model-host:8080", ChatGPTCodexResponsesPath, "", "the ChatGPT OAuth token is never sent to any other host"},
		{"root path prefix", ChatGPTCodexAPIBase, "/", "", ChatGPTCodexResponsesPath},
		{"v1 path prefix", ChatGPTCodexAPIBase, "/v1", "", ChatGPTCodexResponsesPath},
		{"non-empty worker base path", ChatGPTCodexAPIBase, ChatGPTCodexResponsesPath, "/v1", ChatGPTCodexResponsesPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateChatGPTCodexRoute(tc.upstream, tc.allowedPrefix, tc.basePath)
			if tc.wantErrLike == "" {
				if err != nil {
					t.Errorf("ValidateChatGPTCodexRoute(%q, %q, %q) = %v, want nil", tc.upstream, tc.allowedPrefix, tc.basePath, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrLike) {
				t.Errorf("ValidateChatGPTCodexRoute(%q, %q, %q) = %v, want an error containing %q", tc.upstream, tc.allowedPrefix, tc.basePath, err, tc.wantErrLike)
			}
		})
	}
}

// TestHasPathPrefixResponsesTrailingSlashAndRoot is the regression test
// for a round-2 review: sandbox.ValidateGitHubCopilotWorkerAPI reuses
// this exact function (rather than an exact-string compare) so an
// operator-set relay_allowed_path_prefix of "/responses/" (trailing
// slash) or "/" (root, allowing every path) must both be accepted as
// covering CopilotResponsesPath under relay_worker_api:
// openai-responses.
func TestHasPathPrefixResponsesTrailingSlashAndRoot(t *testing.T) {
	for _, tc := range []struct {
		requestPath, allowedPrefix string
		want                       bool
	}{
		{"/responses", "/responses/", true},
		{"/responses", "/", true},
		{"/responses", "", true},
		{"/responses", "/chat/completions", false},
		{"/chat/completions", "/chat/completions/", true},
	} {
		if got := HasPathPrefix(tc.requestPath, tc.allowedPrefix); got != tc.want {
			t.Errorf("HasPathPrefix(%q, %q) = %v, want %v", tc.requestPath, tc.allowedPrefix, got, tc.want)
		}
	}
}
