package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sessionconfig"
)

// fakeCodexJWT builds a compact JWT (header.payload.signature) whose payload
// carries only "exp" -- resolveChatGPTCodexCredential never verifies a
// signature, so the third segment can be any non-empty placeholder.
func fakeCodexJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]int64{"exp": exp.Unix()})
	if err != nil {
		t.Fatalf("marshal JWT payload: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeCodexAuthFile(t *testing.T, dir string, contents map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	data, err := json.Marshal(contents)
	if err != nil {
		t.Fatalf("marshal codex auth file: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write codex auth file: %v", err)
	}
	return path
}

func TestResolveChatGPTCodexCredentialReadsFreshValidCredential(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuthFile(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
			"account_id":   "acct-123",
		},
	})

	token, accountID, err := resolveChatGPTCodexCredential(path)
	if err != nil {
		t.Fatalf("resolveChatGPTCodexCredential: %v", err)
	}
	if accountID != "acct-123" {
		t.Fatalf("accountID = %q, want acct-123", accountID)
	}
	if token == "" {
		t.Fatal("token is empty")
	}

	// Changing the file on disk and re-resolving must observe the new
	// value -- proving this reads fresh every call rather than caching.
	newToken := fakeCodexJWT(t, time.Now().Add(48*time.Hour))
	writeCodexAuthFile(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": newToken,
			"account_id":   "acct-456",
		},
	})
	token2, accountID2, err := resolveChatGPTCodexCredential(path)
	if err != nil {
		t.Fatalf("resolveChatGPTCodexCredential (second read): %v", err)
	}
	if token2 != newToken {
		t.Fatalf("second read token = %q, want the freshly written %q (freshness not honored)", token2, newToken)
	}
	if accountID2 != "acct-456" {
		t.Fatalf("second read accountID = %q, want acct-456", accountID2)
	}
}

func TestResolveChatGPTCodexCredentialFailsClosed(t *testing.T) {
	farFuture := time.Now().Add(24 * time.Hour)
	soon := time.Now().Add(time.Hour) // inside codexTokenExpiryMargin (6h)

	tests := []struct {
		name     string
		contents map[string]any
		wantErr  string
	}{
		{
			name: "wrong auth mode",
			contents: map[string]any{
				"auth_mode": "apikey",
				"tokens": map[string]any{
					"access_token": fakeCodexJWTLiteral(farFuture),
					"account_id":   "acct-123",
				},
			},
			wantErr: "auth_mode",
		},
		{
			name: "missing access token",
			contents: map[string]any{
				"auth_mode": "chatgpt",
				"tokens": map[string]any{
					"account_id": "acct-123",
				},
			},
			wantErr: "missing",
		},
		{
			name: "missing account id",
			contents: map[string]any{
				"auth_mode": "chatgpt",
				"tokens": map[string]any{
					"access_token": fakeCodexJWTLiteral(farFuture),
				},
			},
			wantErr: "missing",
		},
		{
			name: "expiring within margin",
			contents: map[string]any{
				"auth_mode": "chatgpt",
				"tokens": map[string]any{
					"access_token": fakeCodexJWTLiteral(soon),
					"account_id":   "acct-123",
				},
			},
			wantErr: "expires",
		},
		{
			name: "access token not a JWT",
			contents: map[string]any{
				"auth_mode": "chatgpt",
				"tokens": map[string]any{
					"access_token": "not-a-jwt",
					"account_id":   "acct-123",
				},
			},
			wantErr: "JWT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeCodexAuthFile(t, dir, tc.contents)
			_, _, err := resolveChatGPTCodexCredential(path)
			if err == nil {
				t.Fatalf("resolveChatGPTCodexCredential: got nil error, want a %q failure", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.wantErr)
			}
			// The raw token must never appear in the error message.
			if token, ok := tc.contents["tokens"].(map[string]any)["access_token"].(string); ok && token != "" && strings.Contains(err.Error(), token) {
				t.Fatalf("error message leaked the access token: %s", err.Error())
			}
		})
	}
}

func TestResolveChatGPTCodexCredentialMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, _, err := resolveChatGPTCodexCredential(filepath.Join(dir, "does-not-exist.json"))
	if err == nil {
		t.Fatal("resolveChatGPTCodexCredential: got nil error for a missing file")
	}
}

func TestResolveCodexAuthFilePathFallsBackToCodexHomeThenDefault(t *testing.T) {
	explicit, err := resolveCodexAuthFilePath("/explicit/path/auth.json")
	if err != nil {
		t.Fatalf("resolveCodexAuthFilePath (explicit): %v", err)
	}
	if explicit != "/explicit/path/auth.json" {
		t.Fatalf("resolveCodexAuthFilePath (explicit) = %q, want the path unchanged", explicit)
	}

	t.Setenv("CODEX_HOME", "/custom/codex/home")
	fromEnv, err := resolveCodexAuthFilePath("")
	if err != nil {
		t.Fatalf("resolveCodexAuthFilePath (CODEX_HOME): %v", err)
	}
	if fromEnv != filepath.Join("/custom/codex/home", "auth.json") {
		t.Fatalf("resolveCodexAuthFilePath (CODEX_HOME) = %q, want $CODEX_HOME/auth.json", fromEnv)
	}

	t.Setenv("CODEX_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	fromHome, err := resolveCodexAuthFilePath("")
	if err != nil {
		t.Fatalf("resolveCodexAuthFilePath (default): %v", err)
	}
	if fromHome != filepath.Join(home, ".codex", "auth.json") {
		t.Fatalf("resolveCodexAuthFilePath (default) = %q, want ~/.codex/auth.json", fromHome)
	}
}

// fakeCodexJWTLiteral is fakeCodexJWT without the *testing.T dependency, for
// use inside table-driven map literals above.
func fakeCodexJWTLiteral(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
func TestResolveRequestJobRelaySpecCarriesResponsesWorkerAPI(t *testing.T) {
	authFile := writeCodexAuthFile(t, t.TempDir(), map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(48*time.Hour)),
			"account_id":   "acct-123",
		},
	})
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"codex": {CredentialMode: meter.CredentialModeChatGPTCodex, CodexAuthFile: authFile},
	}
	settings.Models = map[string]sessionconfig.Model{
		"gpt-5.6-luna": {ID: "gpt-5.6-luna", Routes: []string{"codex"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "gpt-5.6-luna"}}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	spec, err := resolveRequestJobRelaySpec(cfg, "spec drafting", "run-1", t.TempDir(), requestJobRoleOverride{Thinking: sel.Thinking, RouteSelection: &sel})
	if err != nil {
		t.Fatalf("resolveRequestJobRelaySpec = %v, want a valid chatgpt-codex drafting relay", err)
	}
	if spec.WorkerModelAPI != meter.RequestFormatOpenAIResponses {
		t.Fatalf("drafting worker model API = %q, want %q", spec.WorkerModelAPI, meter.RequestFormatOpenAIResponses)
	}
}

// TestDoctorRouteCredentialFixNamesWhyNoRouteIsUsable: a role whose only
// route's credential does not resolve used to fail doctor with modelrole's
// fixed "credential did not resolve" and nothing to act on (from-nothing
// install walk, 2026-10-07: a Codex token inside the expiry margin).
func TestDoctorRouteCredentialFixNamesWhyNoRouteIsUsable(t *testing.T) {
	authFile := writeCodexAuthFile(t, t.TempDir(), map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(time.Hour)),
			"account_id":   "acct-123",
		},
	})
	settings := sessionconfig.Settings{Routes: map[string]sessionconfig.Route{
		"codex": {CredentialMode: "chatgpt-codex", CodexAuthFile: authFile},
	}}
	fix := doctorRouteCredentialFix(settings, fmt.Errorf("roles.execution: %w", modelrole.ErrNoRouteAvailable))
	for _, want := range []string{"route codex:", "expires at", "codex login"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix = %q, want it to contain %q", fix, want)
		}
	}
	if got := doctorRouteCredentialFix(settings, errors.New("another failure")); got != "" {
		t.Errorf("fix for an unrelated error = %q, want none", got)
	}
}
