package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/workspace"
)

// WithWorkspaces configures the workspace allowlist POST /requests and
// GET /workspaces consult: the session config's own `workspaces:`
// key (sessionconfig.Settings.Workspaces). Without this option the
// allowlist is empty, so POST /requests still accepts a workspace that
// already matches an existing internal/request.Request's own Workspace
// (see workspaceAllowed) but nothing else -- fail-closed, not fail-open,
// for a Server never told about this key.
func WithWorkspaces(paths []string) Option {
	return func(s *Server) {
		s.allowedWorkspaces = paths
	}
}

// createRequestBody is POST /requests' JSON body. Field names mirror
// `factoryd submit`'s own flags one-for-one (cmd/factoryd/submit.go's
// newSubmitFlags) so an operator moving between the CLI and the console
// "New request" form recognizes the same vocabulary. Workspace/Text are
// the only required fields; every other field left unset behaves exactly
// as the equivalent submit flag being left unset does (resolved from the
// workspace's own .factory.yml where one exists).
type createRequestBody struct {
	Workspace        string `json:"workspace"`
	Text             string `json:"text"`
	VerifyCommand    string `json:"verify_command,omitempty"`
	FullSuiteCommand string `json:"full_suite_command,omitempty"`
	PreflightProfile string `json:"preflight_profile,omitempty"`
	DraftOracles     bool   `json:"draft_oracles,omitempty"`
	By               string `json:"by,omitempty"`
	// Models is `factoryd submit -model role=model`'s HTTP equivalent: a
	// per-request model pick for "planning" and/or "execution", checked
	// against s.settings' own roles.<role>.allowed by
	// requestsubmit.Submit (sessionconfig.ValidateRequestModels) before
	// the request is created. Review is never requester-selectable.
	Models map[string]string `json:"models,omitempty"`
	// Harnesses is `factoryd submit -harness role=name`'s HTTP equivalent: a
	// per-request coding-agent CLI pick for "planning" and/or "execution",
	// checked against roles.<role>.allowed_harnesses by requestsubmit.Submit
	// (sessionconfig.ValidateRequestHarnesses). Review is never
	// requester-selectable.
	Harnesses map[string]string `json:"harnesses,omitempty"`
}

// createRequest serves POST /requests: starts a request from the
// console instead of requiring `factoryd submit` at a terminal. Gated by
// authorizeRequestWrite -- the same loopback-same-origin-JSON-or-
// override-token rule approve/reject/retry/cancel already use (see that
// function's own doc comment and safety-contract.md's "Console loopback
// writes" trust-boundary row) -- since starting a request is the same
// class of operator write POST /requests/{id}/approve already is, not a
// stronger one: unlike POST /runs/{id}/override, it can only ever create
// a new request in StateSubmitted, never move an existing run past a
// safety gate. POST /mcp's submit_request tool (mcpCaller) is the one other
// way in, behind the MCP token; the workspace allowlist below applies to it
// unchanged.
//
// Every field of internal/requestsubmit.Params this handler doesn't
// expose (Model, ConfigPath) is left at its zero value, exactly
// as an operator who never passes the corresponding `factoryd submit`
// flag gets today -- there is no console UI for either yet.
func (s *Server) createRequest(w http.ResponseWriter, r *http.Request) {
	if !mcpCaller(r) && !s.authorizeRequestWrite(r) {
		writeError(w, http.StatusForbidden, "requests endpoint is not authorized")
		return
	}

	var body createRequestBody
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode request body")
		return
	}
	// Reject a second JSON value, matching startRunWithID's own
	// treatment of trailing bytes after a valid JSON object.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return
	}
	if strings.TrimSpace(body.Workspace) == "" || strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "workspace and text are required")
		return
	}

	canonicalWorkspace, allowed, err := s.workspaceAllowed(body.Workspace)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("workspace: %s", err))
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "workspace is not allowlisted: add it to the session config's workspaces list, or submit against a workspace an existing request already uses")
		return
	}

	by := body.By
	if by == "" {
		by = requestAPIPrincipal
	}

	result, err := requestsubmit.Submit(requestsubmit.Params{
		WorkspaceArg:               canonicalWorkspace,
		DataDir:                    s.dataDir,
		RequestText:                body.Text,
		Source:                     request.Source{Kind: request.SourceText},
		VerifyCommand:              body.VerifyCommand,
		VerifyCommandExplicit:      body.VerifyCommand != "",
		FullSuiteCommand:           body.FullSuiteCommand,
		DraftOracles:               body.DraftOracles,
		PreflightProfile:           body.PreflightProfile,
		PreflightProfileExplicit:   body.PreflightProfile != "",
		SessionTokenCeiling:        s.sessionTokenCeiling,
		SessionCostCeilingMicroUSD: s.sessionCostCeilingMicroUSD,
		Models:                     body.Models,
		Harnesses:                  body.Harnesses,
		Settings:                   s.settings,
	})
	if err != nil {
		// Every requestsubmit.Submit error is a caller-input/precondition
		// problem (an unresolvable verify command, a colliding project,
		// -data-dir-inside-workspace, ...), never an internal failure --
		// 422, mirroring the ticket's own "resolution error text" wording
		// requirement, not 500.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// by is not yet a durable field on internal/request.Request (no
	// `factoryd submit -by` exists either -- see createRequestBody's own
	// doc comment on why this handler stops at accepting it), so this
	// log line is its only record today: enough for an operator to trace
	// who submitted a request from the console's own server log, the
	// same "operator must be able to follow what the factory did"
	// standard the rest of this file's console-loopback-write routes
	// already hold themselves to.
	log.Printf("POST /requests: created %s by %q workspace=%q", result.ID, by, canonicalWorkspace)

	loaded, err := request.Load(s.dataDir, result.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load created request")
		return
	}
	s.wakeRequest(r.Context(), result.ID)
	writeJSON(w, http.StatusCreated, s.buildRequestDetailView(s.dataDir, result.ID, loaded))
}

// workspaceAllowed resolves workspaceArg to its canonical absolute path,
// requires it to be a git repository root (workspace.CanonicalPath +
// requestsubmit.GitToplevel -- see that function's own doc comment for
// why release.RepositoryRoot can't serve this check), and reports
// whether it is allowed for an HTTP-originated submission: either listed
// in s.allowedWorkspaces (the session config's `workspaces:` key, see
// WithWorkspaces), or already the Workspace of some existing
// internal/request.Request under s.dataDir. This is deliberately a
// narrower rule than `factoryd submit`'s own -- an operator at a
// terminal can already point -workspace at any local path, so
// restricting that would add friction with no safety benefit; a browser
// caller reaching this over HTTP (even loopback-only, per
// authorizeRequestWrite) gets an explicit allowlist instead, the same
// posture -api-allowed-sandbox-images already gives POST /runs'
// sandbox_image (safety-contract.md's Control-plane API trust-boundary
// row).
func (s *Server) workspaceAllowed(workspaceArg string) (canonical string, allowed bool, err error) {
	canonical, err = workspace.CanonicalPath(workspaceArg)
	if err != nil {
		return "", false, fmt.Errorf("resolve path: %w", err)
	}
	// Allowlist first, git second: `git rev-parse` reads the directory's
	// own .git/config (core.fsmonitor and similar can run commands), so an
	// HTTP caller's path must never reach git until it is a workspace the
	// operator listed or already submitted from (found in review,
	// 2026-09-24).
	if !s.workspaceListed(canonical) {
		listed, listErr := s.workspaceOfExistingRequest(canonical)
		if listErr != nil {
			return "", false, listErr
		}
		if !listed {
			return canonical, false, nil
		}
	}
	top, err := requestsubmit.GitToplevel(canonical)
	if err != nil {
		return "", false, fmt.Errorf("must be a git repository root: %w", err)
	}
	canonicalTop, err := workspace.CanonicalPath(top)
	if err != nil {
		return "", false, fmt.Errorf("resolve path: %w", err)
	}
	if canonicalTop != canonical {
		return "", false, fmt.Errorf("%q is not the git repository root (root is %q)", workspaceArg, canonicalTop)
	}
	return canonical, true, nil
}

// workspaceListed reports whether canonical is one of the session config's
// `workspaces:` entries.
func (s *Server) workspaceListed(canonical string) bool {
	for _, allowedPath := range s.allowedWorkspaces {
		if canonicalAllowed, err := workspace.CanonicalPath(allowedPath); err == nil && canonicalAllowed == canonical {
			return true
		}
	}
	return false
}

// workspaceOfExistingRequest reports whether canonical is already the
// workspace of a request in this data dir.
func (s *Server) workspaceOfExistingRequest(canonical string) (bool, error) {
	existing, err := request.List(s.dataDir)
	if err != nil {
		return false, fmt.Errorf("list existing requests: %w", err)
	}
	for _, req := range existing {
		if canonicalExisting, err := workspace.CanonicalPath(req.Workspace); err == nil && canonicalExisting == canonical {
			return true, nil
		}
	}
	return false, nil
}

// workspaceHintView is one GET /workspaces row: a workspace this Server
// will accept from POST /requests, plus the console "New request" form's
// own hints -- the verify command it would resolve to (and where that
// came from) and whether a .factory.yml exists at all, so an operator
// never has to open a terminal just to answer "what would this
// workspace's verify command default to."
type workspaceHintView struct {
	Workspace         string `json:"workspace"`
	HasFactoryYML     bool   `json:"has_factory_yml"`
	ResolvedVerifyCmd string `json:"resolved_verify_command,omitempty"`
	VerifyCmdSource   string `json:"verify_command_source,omitempty"`
}

// listWorkspaces serves GET /workspaces: every workspace POST /requests
// would currently accept (s.allowedWorkspaces, deduplicated with every
// existing request's own Workspace), each with the console form's
// resolved-verify-command hint. A read route, gated like GET /requests
// (authorizeRead) rather than authorizeRequestWrite, since it discloses
// nothing a caller couldn't already learn from GET /requests plus its
// own filesystem read of a workspace it's about to submit against.
func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRead(r) {
		writeError(w, http.StatusForbidden, "workspaces endpoint is not authorized")
		return
	}

	seen := map[string]bool{}
	var paths []string
	addIfNew := func(p string) {
		canonical, err := workspace.CanonicalPath(p)
		if err != nil || seen[canonical] {
			return
		}
		seen[canonical] = true
		paths = append(paths, canonical)
	}
	for _, p := range s.allowedWorkspaces {
		addIfNew(p)
	}
	existing, err := request.List(s.dataDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list existing requests")
		return
	}
	for _, req := range existing {
		addIfNew(req.Workspace)
	}

	views := make([]workspaceHintView, 0, len(paths))
	for _, p := range paths {
		views = append(views, workspaceHintFor(p))
	}
	writeJSON(w, http.StatusOK, views)
}

// workspaceHintFor loads p's own .factory.yml (if any) and reports the
// verify command a POST /requests submission against it would resolve
// to with no explicit -verify-command-equivalent override -- the same
// precedence requestsubmit.ApplyProjectConfigDefaults applies, but
// evaluated read-only here (no request is created) purely to render the
// console form's hint text.
func workspaceHintFor(p string) workspaceHintView {
	view := workspaceHintView{Workspace: p}
	cfg, found, err := projectconfig.Load(p)
	if err != nil || !found {
		return view
	}
	view.HasFactoryYML = true
	if cfg.VerifyCommand != "" {
		view.ResolvedVerifyCmd = cfg.VerifyCommand
		view.VerifyCmdSource = projectconfig.FileName
	}
	return view
}
