package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexTokenExpiryMargin is how far ahead of the ChatGPT access token's own
// JWT "exp" claim resolveChatGPTCodexCredential starts refusing it: a
// relay must not launch carrying a credential that could expire mid-run.
// The direct and in-process Temporal paths resolve it once per run, and a
// default run can take longer than 2h (-max-rounds 3 x -timeout-minutes 45
// of build alone, plus verify and review; found in review 2026-09-23), so
// 6h. Access tokens live ~10 days. The host codex CLI does not refresh one
// inside this margin (seen 2026-10-07, codex-cli 0.157.1: a `codex exec`
// with 1.3h left kept the token), so for those hours the route needs a
// `codex login`.
const codexTokenExpiryMargin = 6 * time.Hour

// chatGPTCodexDefaultModelID and chatGPTCodexDefaultContextWindow are
// `quickstart -route chatgpt-codex`'s own defaults when the operator gives
// no -model-id/-context-window: codex exec's own relay route sends only
// POST <base>/responses (see meter.ChatGPTCodexResponsesPath's own doc
// comment) -- there is no GET /models listing to discover a model id or
// context window from the way -route openai/copilot can (see
// quickstartFetchModelIDs / meter.ListGitHubCopilotModels) -- so this
// falls back to the one model this route has been live-validated against:
// USAGE.md's own route table records gpt-5.6-luna with a 272000-token
// context window as the working configuration.
const (
	chatGPTCodexDefaultModelID       = "gpt-5.6-luna"
	chatGPTCodexDefaultContextWindow = 272000
)

// codexAuthFileCredential is the shape of ~/.codex/auth.json's relevant
// fields for a ChatGPT OAuth login (codex-cli 0.154.0): only auth_mode and
// the two token fields this package needs are modeled; every other field
// (last_refresh, ...) is ignored.
type codexAuthFileCredential struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// resolveChatGPTCodexCredential reads path (or, when empty, $CODEX_HOME/
// auth.json, falling back to ~/.codex/auth.json) FRESH on every call -- no
// caching -- and returns the ChatGPT access token and account id this
// relay forces onto every forwarded request, or a fail-closed, actionable
// error: the file is missing or unparseable, auth_mode is not "chatgpt",
// either token field is empty, or the access token's own JWT "exp" claim is
// less than codexTokenExpiryMargin away.
//
// Deliberately never refreshes or writes auth.json (see
// meter.CredentialModeChatGPTCodex's own doc comment for why: the
// operator's ChatGPT refresh token rotates on use, so a background refresh
// here would log the host `codex` CLI out from underneath the operator).
// Never logs or includes the token itself in an error message.
func resolveChatGPTCodexCredential(path string) (accessToken, accountID string, err error) {
	resolved, err := resolveCodexAuthFilePath(path)
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", "", fmt.Errorf("read codex auth file %q: %w; authenticate with `codex` on the host, or set the route's codex_auth_file", resolved, err)
	}
	var auth codexAuthFileCredential
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", "", fmt.Errorf("parse codex auth file %q: %w", resolved, err)
	}
	if auth.AuthMode != "chatgpt" {
		return "", "", fmt.Errorf("codex auth file %q has auth_mode %q, want \"chatgpt\": credential_mode: chatgpt-codex requires a ChatGPT OAuth login (run `codex login` on the host)", resolved, auth.AuthMode)
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.AccountID == "" {
		return "", "", fmt.Errorf("codex auth file %q is missing tokens.access_token or tokens.account_id; run `codex login` on the host", resolved)
	}
	expiresAt, err := jwtExpiry(auth.Tokens.AccessToken)
	if err != nil {
		return "", "", fmt.Errorf("codex auth file %q: parse access token expiry: %w", resolved, err)
	}
	if time.Until(expiresAt) < codexTokenExpiryMargin {
		return "", "", fmt.Errorf("codex access token in %q expires at %s, within the required %s margin; run `codex login` on the host for a new token (other codex commands leave a token this close to expiry as it is)", resolved, expiresAt.UTC().Format(time.RFC3339), codexTokenExpiryMargin)
	}
	return auth.Tokens.AccessToken, auth.Tokens.AccountID, nil
}

// resolveCodexAuthFilePath applies -relay-codex-auth-file's own three-tier
// fallback: the explicit path, else $CODEX_HOME/auth.json, else
// ~/.codex/auth.json -- codex-cli's own two auth.json locations.
func resolveCodexAuthFilePath(path string) (string, error) {
	if path != "" {
		return expandAuthPath(path)
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for codex auth discovery: %w", err)
	}
	return filepath.Join(homeDir, ".codex", "auth.json"), nil
}

// jwtExpiry decodes -- without verifying a signature, since this process
// trusts the token's own issuer (ChatGPT's login flow), not this claim --
// the "exp" claim of a compact JWT's second (payload) segment.
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("access token is not a JWT (want 3 dot-separated segments, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse JWT payload: %w", err)
	}
	if claims.Exp <= 0 {
		return time.Time{}, fmt.Errorf("JWT payload has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}
