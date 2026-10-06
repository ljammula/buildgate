package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/sandbox"
)

// listGitHubCopilotModelsFn is meter.ListGitHubCopilotModels, indirected
// through a package var (the same pattern forge.userLogin uses)
// so doctorCheckCopilotModelListed, doctorListModels's own github-copilot
// branch, and quickstartBuildConfig's own copilot model picker are all
// testable against a stub instead of a real network call to
// api.individual.githubcopilot.com/api.github.com.
var listGitHubCopilotModelsFn = meter.ListGitHubCopilotModels

// doctorRegisterListModelsFlag mirrors doctorRegisterYesFlag/
// doctorRegisterNotifyTestFlag's own pattern: registered directly on
// doctorMain's FlagSet rather than threaded through newDoctorFlags' own
// already-long return tuple.
func doctorRegisterListModelsFlag(flags *flag.FlagSet) *bool {
	return flags.Bool("list-models", false, "list the models roles.execution's resolved route actually serves (host-side, reusing the same credential/token-exchange this check suite already performs), then exit without running the rest of doctor's checks")
}

// anthropicDefaultUpstream is the Anthropic route's own hardcoded upstream
// (defaultedRoute's own static-mode default, internal/sessionconfig/routing.go);
// an empty in.relayUpstream (a route resolved with no Upstream configured
// at all) also counts as "the anthropic route" for doctorListModels' own
// purposes below -- an operator who never set an upstream is exactly the
// common anthropic-route case, not a signal to build a bogus "/models"
// lookup.
const anthropicDefaultUpstream = "https://api.anthropic.com"

// doctorUpstreamLooksAnthropic reports whether upstream is (or, empty,
// defaults to) the Anthropic Messages API -- which has no OpenAI-shaped
// GET /v1/models listing this command's default branch can use.
func doctorUpstreamLooksAnthropic(upstream string) bool {
	return upstream == "" || strings.TrimRight(upstream, "/") == anthropicDefaultUpstream
}

// redactUserinfo strips a URL's embedded userinfo (https://user:pass@host/..)
// before it is ever printed -- a round-2 review: a printed listing
// URL must never echo a credential an operator embedded directly in
// a route's upstream. Only what doctorListModels prints goes through this;
// the real request always uses the unmodified URL.
func redactUserinfo(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User == nil {
		return rawURL
	}
	parsed.User = nil
	return parsed.String()
}

// doctorFetchModelIDs is fetchOpenAIModelIDs' own request/parse shape
// (parseOpenAIModelIDs, quickstart.go), but through the CA-aware
// transport a real relay gets (meter.OutboundTransport) and, when
// headerName is non-empty, the same credential header a real forwarded
// request would carry -- a round-2 review found doctor's own listing
// must reach the SAME route with the SAME auth a real run does, not an
// unauthenticated bare probe that can see a different (or no) catalog,
// and must never trust an unbounded/uncertified response the way a bare
// client.Get would. CheckRedirect: meter.NoRedirectCheckRedirect -- a
// round-3 review found Go's default client forwards a same-scheme
// redirect's request headers -- a credential header included -- to
// whatever host the redirect names; this call must never do that.
func doctorFetchModelIDs(rawURL, caBundlePath, headerName, headerValue string) ([]string, error) {
	transport, err := meter.OutboundTransport(caBundlePath)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: meter.NoRedirectCheckRedirect}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if headerName != "" {
		req.Header.Set(headerName, headerValue)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read model list: %w", err)
	}
	return parseOpenAIModelIDs(body)
}

// doctorListModels implements `factoryd doctor -list-models`: writes the
// models in.relayCredentialMode's route actually serves to w, reusing
// exactly the credential/token-exchange code the corresponding doctor
// check (or, for chatgpt-codex, the relay-launch path) already uses --
// never a separate, duplicated exchange. Host-side only, same as every
// other credential-touching doctor check. w, not a direct os.Stdout
// write, so tests can assert on the printed output without capturing the
// real process stdout.
func doctorListModels(ctx context.Context, w io.Writer, in doctorInputs) error {
	// Resolve the same selection a real run's own roles.execution would
	// make (modelrole.SelectRoute, the same probe every caller uses) and
	// list against THAT route.
	sel, err := modelrole.SelectRoute(in.settings, modelrole.RoleExecution, "", "", "", doctorRouteCredentialProbe)
	if err != nil {
		return fmt.Errorf("roles.execution: %w", err)
	}
	in.relayCredentialMode = sel.Policy.AuthMode
	in.relayUpstream = sel.Policy.Upstream
	in.relayWorkerAPI = sel.Policy.WorkerModelAPI
	in.relayWorkerModelID = sel.Policy.WorkerModelID
	in.relayWorkerBasePath = sel.Policy.WorkerBasePath
	in.relayCredentialHeader = sel.Policy.UpstreamAuthHeader
	in.relayAllowPlaintextUpstream = sel.Policy.AllowPlaintextUpstream
	in.relayAllowNoCredential = sel.Policy.AllowUnauthenticatedUpstream
	in.relayGitHubTokenFile = sel.Route.GitHubTokenFile
	in.relayGitHubTokenKey = sel.Route.GitHubTokenKey
	in.relayCodexAuthFile = sel.Route.CodexAuthFile
	switch in.relayCredentialMode {
	case meter.CredentialModeGitHubCopilot:
		// A round-3 review found the same guard doctorCheckCopilotModelListed
		// applies, before the token exchange -- a stale relay_upstream must
		// never receive the exchanged Copilot API token.
		if err := doctorValidateCopilotUpstream(effectiveCopilotUpstream(in.relayUpstream), in.relayAllowPlaintextUpstream); err != nil {
			return err
		}
		token, err := resolveGitHubCopilotToken(in.relayGitHubTokenFile, in.relayGitHubTokenKey)
		if err != nil {
			return err
		}
		models, err := listGitHubCopilotModelsFn(ctx, in.egressCABundle, token, in.relayUpstream)
		if err != nil {
			return fmt.Errorf("list copilot models: %w", err)
		}
		if len(models) == 0 {
			fmt.Fprintln(w, "No Copilot models listed for this account.")
			return nil
		}
		fmt.Fprintf(w, "%d entitled Copilot model(s):\n", len(models))
		for _, m := range models {
			var tags []string
			if m.ModelPickerEnabled {
				tags = append(tags, "picker default")
			}
			if m.Preview {
				tags = append(tags, "preview")
			}
			if m.IsPremium {
				tags = append(tags, fmt.Sprintf("premium x%g", m.Multiplier))
			}
			if ok, reason, fix := meter.CopilotModelUsable(m, in.relayWorkerAPI); !ok {
				// This line already prints m's own endpoints as their own
				// "endpoints: ..." tag just below when there are any, so
				// prefer fix (CopilotModelUsable's own actionable
				// suggestion, e.g. "set models.<name>.api: ...") over
				// reason (which repeats the endpoint list) whenever one
				// exists; a policy refusal has no fix and falls back to
				// its own (endpoint-free) reason.
				msg := fix
				if msg == "" {
					msg = reason
				}
				tags = append(tags, "not usable: "+msg)
			}
			line := "  " + m.ID
			if len(m.SupportedEndpoints) > 0 {
				line += fmt.Sprintf(" [endpoints: %s]", strings.Join(m.SupportedEndpoints, ", "))
			}
			if m.ContextWindow > 0 {
				line += fmt.Sprintf(" (context window %d)", m.ContextWindow)
			}
			if len(tags) > 0 {
				line += " [" + strings.Join(tags, ", ") + "]"
			}
			fmt.Fprintln(w, line)
			if evidence, weak := quickstartWeakModelAdvisories[m.ID]; weak {
				fmt.Fprintf(w, "    warning: observed too weak for the build loop -- %s\n", evidence)
			}
		}
		return nil

	case meter.CredentialModeChatGPTCodex:
		// codex exec's own relay route speaks only POST <base>/responses --
		// there is no listing endpoint (see meter.ChatGPTCodexResponsesPath's
		// own doc comment and chatGPTCodexDefaultModelID's own doc comment).
		fmt.Fprintln(w, "chatgpt-codex has no model-listing endpoint: codex exec speaks only POST /responses.")
		configured := in.relayWorkerModelID
		if configured == "" {
			configured = "(none configured; the default is " + chatGPTCodexDefaultModelID + ")"
		}
		fmt.Fprintf(w, "Configured model: %s\n", configured)
		return nil

	default:
		// Static credential mode covers both the Anthropic route and an
		// arbitrary openai-compatible endpoint -- there is no distinct
		// credential mode for "Anthropic" to switch on, so this treats an
		// empty or api.anthropic.com upstream as the Anthropic route (item
		// 5, round-2 review): its Messages API has no OpenAI-shaped
		// GET /v1/models listing, so this says so plainly instead of
		// building a bogus lookup URL and reporting a confusing failure.
		if doctorUpstreamLooksAnthropic(in.relayUpstream) {
			fmt.Fprintln(w, "listing not supported for this route (the Anthropic Messages API has no GET /v1/models-shaped listing).")
			return nil
		}
		basePath := in.relayWorkerBasePath
		if basePath == "" {
			basePath = "/v1"
		}
		listURL := strings.TrimRight(in.relayUpstream, "/") + "/" + strings.Trim(basePath, "/") + "/models"
		printURL := redactUserinfo(listURL)

		// Sent only when this route's own credential is actually configured
		// (resolveRouteCredentials(sel.Route) -- the same resolver a real
		// launch uses, reading routes.<name>.credential_env, not a
		// hard-coded ANTHROPIC_API_KEY: a route naming a different env var
		// must never have this listing call fall back to Anthropic's,
		// found via review) AND the selected route's own
		// allow_no_credential is not set: that setting means a real run
		// would send NO credential at all (quickstart's own -route openai
		// "no credential" path even scrubs ANTHROPIC_API_KEY from the
		// daemon's environment), so this listing call must not send one
		// either just because the operator's shell happens to still have
		// it set.
		var headerName, headerValue string
		credentialPresent := false
		if creds, credErr := resolveRouteCredentials(sel.Route); credErr == nil && creds.apiKey.Configured() && !in.relayAllowNoCredential {
			credentialPresent = true
			headerName = in.relayCredentialHeader
			if headerName == "" {
				headerName = meter.CredentialHeaderXAPIKey
			}
			// sel.Route is Routing()'s defaulted copy, so CredentialEnv is
			// already filled in for a static route (defaultedRoute).
			headerValue = meter.CredentialHeaderValue(headerName, os.Getenv(sel.Route.CredentialEnv))
		}

		// A round-3 review found this must apply exactly the scheme/plaintext-vs-
		// credential rules a real relay launch would (RouteSpec.Validate,
		// via its own exported ValidateUpstreamScheme/CredentialSafeForUpstream
		// halves) -- never send a request, credentialed or not, that a
		// real run would have refused to even start over. Nothing is sent
		// (not even an unauthenticated probe) when this fails: an operator
		// relying on -relay-allow-plaintext-upstream for a genuinely safe
		// local endpoint sees exactly why nothing happened, not a silent
		// skip.
		policy := sandbox.RoutePolicy{Upstream: in.relayUpstream, AllowPlaintextUpstream: in.relayAllowPlaintextUpstream}
		if err := policy.ValidateUpstreamScheme(); err != nil {
			fmt.Fprintf(w, "Not listing models: %v\n", err)
			return nil
		}
		if err := sandbox.CredentialSafeForUpstream(in.relayUpstream, in.relayAllowPlaintextUpstream, credentialPresent); err != nil {
			fmt.Fprintf(w, "Not listing models: %v\n", err)
			return nil
		}

		ids, err := doctorFetchModelIDs(listURL, in.egressCABundle, headerName, headerValue)
		if err != nil {
			return fmt.Errorf("list models from %s: %w", printURL, err)
		}
		if len(ids) == 0 {
			fmt.Fprintf(w, "%s listed no models.\n", printURL)
			return nil
		}
		fmt.Fprintf(w, "%d model(s) from %s:\n", len(ids), printURL)
		for _, id := range ids {
			line := "  " + id
			if evidence, weak := quickstartWeakModelAdvisories[id]; weak {
				line += "\n    warning: observed too weak for the build loop -- " + evidence
			}
			fmt.Fprintln(w, line)
		}
		return nil
	}
}

// doctorValidateCopilotUpstream applies the same scheme-safety validation
// a real relay launch would (sandbox.RoutePolicy.ValidateUpstreamScheme)
// to the effective Copilot models-listing upstream -- a round-3 review
// found a stale relay_upstream (left over, say, after switching from a
// local-model route to github-copilot) must never receive the exchanged
// Copilot API token in cleartext. Shared by doctorCheckCopilotModelListed
// and doctorListModels' own github-copilot branch so the same rule
// applies to both, called BEFORE either one resolves a token or exchanges
// it -- an invalid upstream means no GitHub API call happens at all, not
// just that its result goes unused.
func doctorValidateCopilotUpstream(effectiveUpstream string, allowPlaintextUpstream bool) error {
	policy := sandbox.RoutePolicy{Upstream: effectiveUpstream, AllowPlaintextUpstream: allowPlaintextUpstream}
	if err := policy.ValidateUpstreamScheme(); err != nil {
		return fmt.Errorf("the route's upstream %s is not valid for github-copilot: %w", redactUserinfo(effectiveUpstream), err)
	}
	// The same pinned-upstream check RoutePolicy.Validate applies to a real
	// relay launch (meter.ValidateGitHubCopilotRoute, Phase 2 PR B) --
	// found via review round 1: without this, a stale relay_upstream left
	// over from switching off a local-model route (e.g.
	// https://openrouter.ai/api, a genuine https host that sails right
	// past ValidateUpstreamScheme above) would still get the exchanged
	// Copilot bearer token, both from `doctor` and from `doctor
	// -list-models`, before either one ever reaches token exchange.
	if err := meter.ValidateGitHubCopilotRoute(effectiveUpstream); err != nil {
		return fmt.Errorf("the route's upstream %s is not valid for github-copilot: %w", redactUserinfo(effectiveUpstream), err)
	}
	// The Copilot token is always a credential, so RouteSpec.Validate's
	// credential-over-plaintext rule applies too. Found live in the
	// 2026-09-25 closing walk: ValidateUpstreamScheme alone accepted a
	// private http:// relay_upstream under relay_allow_plaintext_upstream
	// (a local model host left in the default config), so doctor
	// -list-models sent the exchanged Copilot token to it in plaintext,
	// where a real run would have refused to start.
	if err := sandbox.CredentialSafeForUpstream(effectiveUpstream, allowPlaintextUpstream, true); err != nil {
		return fmt.Errorf("the route's upstream %s is not valid for github-copilot: %w", redactUserinfo(effectiveUpstream), err)
	}
	return nil
}
