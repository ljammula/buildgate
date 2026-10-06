import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;

import 'content_hash.dart' show sha256HexBytes;
import 'models.dart';
import 'text_escape.dart';

class RunApi {
  // Found via review: startRun and overrideRun are independently
  // authenticated on the server (FACTORYD_API_START_TOKEN and
  // FACTORYD_API_OVERRIDE_TOKEN are separate, optionally-distinct
  // credentials — see internal/api's own WithStartToken/WithOverrideToken
  // doc comments), but this client used to accept only one authToken and
  // send it as both endpoints' bearer token. An operator running the two
  // tokens genuinely distinct (the server's own supported configuration)
  // could therefore only ever authenticate one of the console's two write
  // operations from here unless they weakened that separation by
  // assigning both endpoints the same credential. startToken defaults to
  // authToken when omitted so existing single-token callers/tests keep
  // working unchanged.
  RunApi({
    required String baseUrl,
    http.Client? client,
    String? authToken,
    String? startToken,
    String? overrideToken,
    this.readToken,
    this.writesEnabled = false,
    this.temporalUiUrl,
    this.releasePolicyWarning,
    Duration watchInitialBackoff = const Duration(seconds: 1),
    Duration watchMaxBackoff = const Duration(seconds: 30),
  }) : // Empty baseUrl means "same origin" (make console-build's own
       // API_BASE_URL= dart-define, for a console the factoryd binary
       // serves itself -- see internal/consoleweb) -- Uri.parse('')
       // parses to an empty relative Uri, and resolving every request
       // path against it below would need this same null check anyway, so
       // it's stored as null outright rather than as a value _uri would
       // otherwise have to special-case on every call.
       _baseUri = baseUrl.isEmpty ? null : Uri.parse(baseUrl),
       _client = client ?? http.Client(),
       _startToken = startToken ?? authToken,
       _overrideToken = overrideToken ?? authToken,
       _watchInitialBackoff = watchInitialBackoff,
       _watchMaxBackoff = watchMaxBackoff;

  final Uri? _baseUri;
  final http.Client _client;
  final String? _startToken;
  final String? _overrideToken;
  // GET /console-config.json's own writes_enabled: true when the
  // server itself will accept an unauthenticated write from this origin
  // (its own loopback Host/Origin check), independent of whether a
  // build-time override token is also configured. Write buttons across
  // this console are enabled on [canWrite] (below), never on this alone
  // or on [hasOverrideToken] alone -- see [canWrite]'s own doc comment.
  final bool writesEnabled;
  // console-config.json's optional temporal_ui_url -- when set, run
  // detail can link straight to a run's own Temporal workflow history.
  // Null when unset or the server predates this field.
  final String? temporalUiUrl;
  // console-config.json's optional release_policy_warning (the
  // release-policy visibility gap): non-null/non-empty exactly when the
  // server's own configured release policy can never allow a release
  // decision. RequestListScreen renders it as a board strip so the
  // operator learns why every PR is silently withheld. Null when the
  // policy is usable or the server predates this field.
  final String? releasePolicyWarning;
  // watchRun's reconnect backoff bounds. Real (second-scale) defaults
  // above; a test constructs its own RunApi with millisecond-scale values
  // instead of waiting out real backoff delays.
  final Duration _watchInitialBackoff;
  final Duration _watchMaxBackoff;
  // FACTORYD_API_READ_TOKEN's client-side counterpart (found via a real
  // GitHub Codex App review of this PR): without this, every read route
  // this client calls (listProjects/listRuns/getRun/getRunDiff/watchRun)
  // sent no Authorization header at all, so a server configured with a
  // read token returned 403 from every one of them -- the exact
  // deployment shape that token exists to support made this console
  // unusable. Unset by default, matching the server's own
  // open-unless-configured default (see api.WithReadToken's doc comment).
  // Public (not `_readToken`) so it can be an initializing formal
  // constructor parameter of the same name.
  final String? readToken;

  // Whether an override token is configured client-side: the console-side
  // counterpart of the server's own fail-closed behavior on every
  // override-token route (approve, reject, POST /runs/{id}/override).
  // Write-adjacent widgets (the approve confirm sheet, the triage screen's
  // a/r actions, the quarantined-run callout) check this so an operator
  // without the override token sees an unmistakably absent action, not a
  // button that always fails with a 403 on tap.
  bool get hasOverrideToken =>
      _overrideToken != null && _overrideToken.isNotEmpty;

  /// Whether this console should offer a write action at all: the
  /// server itself allows an unauthenticated write from this origin
  /// ([writesEnabled], its own loopback Host/Origin check), OR a
  /// build-time override token is baked into this bundle
  /// ([hasOverrideToken]). Replaces every prior `hasOverrideToken`-only
  /// gate on a write control -- a console must never disable a write
  /// button purely because no token is configured when the server would
  /// still accept the request; the server's own 403 (with its reason) is
  /// what a genuinely disallowed write looks like, shown via
  /// [RunApiException.message]/ErrorCallout, not a greyed-out button with
  /// no explanation.
  bool get canWrite => writesEnabled || hasOverrideToken;

  /// GET /console-config.json, fetched once at startup: whether this
  /// server will accept an unauthenticated write from this console's own
  /// origin, and the Temporal UI base URL if configured. Tolerant of a
  /// server predating this route (a 404 or connection failure reads as
  /// "writes not enabled, no Temporal UI") so an older factoryd binary
  /// still serves a (read-only, exactly as before) console rather than
  /// failing to start.
  static Future<ConsoleConfig> fetchConsoleConfig(String baseUrl) async {
    final uri = baseUrl.isEmpty
        ? Uri.parse('/console-config.json')
        : Uri.parse(baseUrl).resolve('/console-config.json');
    try {
      final response = await http.get(uri);
      if (response.statusCode < 200 || response.statusCode >= 300) {
        return const ConsoleConfig(writesEnabled: false);
      }
      final decoded =
          jsonDecode(_bodyText(response)) as Map<String, dynamic>? ?? {};
      return ConsoleConfig(
        writesEnabled: decoded['writes_enabled'] as bool? ?? false,
        temporalUiUrl: decoded['temporal_ui_url'] as String?,
        releasePolicyWarning: decoded['release_policy_warning'] as String?,
      );
    } on Object {
      return const ConsoleConfig(writesEnabled: false);
    }
  }

  /// GET /daemons: whether `worker` (and any other companion
  /// daemon) is alive, so the board can warn an operator that a request
  /// stuck in a working state simply has nothing driving it forward,
  /// rather than looking like ordinary progress. Start-token gated on the
  /// server (internal/api authorizeStart, like the daemon lifecycle routes)
  /// -- sending the read token here left the board's worker strip
  /// permanently hidden behind a 403 (found live, 2026-09-24).
  Future<List<DaemonStatus>> listDaemons() async {
    final response = await _client.get(
      _uri('/daemons'),
      headers: _authHeaders(_startToken),
    );
    _requireSuccess(response);
    final values = jsonDecode(_bodyText(response)) as List<dynamic>;
    return values
        .map((value) => DaemonStatus.fromJson(value as Map<String, dynamic>))
        .toList(growable: false);
  }

  /// GET /queue-run: whether `factoryd worker` is alive against this
  /// data dir, read-token gated (unlike [listDaemons]'s start-token gated
  /// GET /daemons) and answering regardless of whether `factoryd serve`
  /// itself manages the daemon lifecycle -- see QueueRunStatus's own doc
  /// comment for why this exists as a separate route.
  Future<QueueRunStatus> getQueueRunStatus() async {
    final response = await _client.get(
      _uri('/queue-run'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return QueueRunStatus.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  Future<List<ProjectSummary>> listProjects() async {
    final response = await _client.get(
      _uri('/projects'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    final values = jsonDecode(_bodyText(response)) as List<dynamic>;
    return values
        .map((value) => ProjectSummary.fromJson(value as Map<String, dynamic>))
        .toList(growable: false);
  }

  Future<List<Run>> listRuns() async {
    final response = await _client.get(
      _uri('/runs'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    final values = jsonDecode(_bodyText(response)) as List<dynamic>;
    return values
        .map((value) => Run.fromJson(value as Map<String, dynamic>))
        .toList(growable: false);
  }

  Future<Run> getRun(String id) async {
    final response = await _client.get(
      _uri('/runs/${Uri.encodeComponent(id)}'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return Run.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// Starts a run through the authenticated control-plane endpoint.
  ///
  /// Docker containment is unconditional server-side now (there is no
  /// host-execution opt-out anywhere in factoryd), so this sends no
  /// sandbox controls of its own -- a console-started run always
  /// runs sandboxed, using whatever sandbox defaults the daemon's
  /// own -api-allowed-sandbox-images
  /// configuration provides for API-started runs. This screen still
  /// doesn't expose per-request sandbox controls; that's a
  /// separate, pre-existing gap, not something this method can paper
  /// over.
  Future<Run> startRun({
    required String ticket,
    required String workspace,
    required String spec,
    required String repository,
    required String temporalAddress,
  }) async {
    final response = await _client.post(
      _uri('/runs'),
      headers: _writeHeaders(_startToken),
      body: jsonEncode({
        'ticket': ticket,
        'workspace': workspace,
        'spec': spec,
        'repository': repository,
        'temporal_address': temporalAddress,
      }),
    );
    _requireSuccess(response);
    return Run.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// Previews the project-bootstrap preflight startRun's own project-
  /// bootstrap check would otherwise fail closed on -- without starting a
  /// run or persisting anything. ticket is optional: a project with no
  /// ticket drafted yet still gets a verdict on the three project-level
  /// artifacts alone. Uses the same start-token boundary and repository
  /// scope as startRun (see internal/api's checkProject doc comment for
  /// why this is not treated as a plain read).
  Future<ProjectCheckResponse> checkProject({
    required String workspace,
    required String repository,
    String ticket = '',
  }) async {
    final response = await _client.post(
      _uri('/projects/check'),
      headers: _writeHeaders(_startToken),
      body: jsonEncode({
        'workspace': workspace,
        'repository': repository,
        if (ticket.isNotEmpty) 'ticket': ticket,
      }),
    );
    _requireSuccess(response);
    return ProjectCheckResponse.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// Applies an authenticated operator override to a quarantined run.
  Future<Run> overrideRun(
    String id, {
    required String by,
    required String reason,
    required String state,
  }) async {
    final response = await _client.post(
      _uri('/runs/${Uri.encodeComponent(id)}/override'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({'by': by, 'reason': reason, 'state': state}),
    );
    _requireSuccess(response);
    return Run.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /requests: every request currently on disk, oldest-submitted
  /// first (see internal/request.List). Gated with the read token, the
  /// same as listRuns/getRun -- moving GET /requests(+/{id}) from
  /// override-token to read-token auth server-side (to authorizeRead) so
  /// the request board works with read credentials means this client now
  /// sends the matching credential instead of the override token
  /// approveRequest/rejectRequest below still need for their own,
  /// separately gated write routes.
  Future<List<RequestSummary>> listRequests() async {
    final response = await _client.get(
      _uri('/requests'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    final values = jsonDecode(_bodyText(response)) as List<dynamic>;
    return values
        .map((value) => RequestSummary.fromJson(value as Map<String, dynamic>))
        .toList(growable: false);
  }

  /// GET /requests/{id}: one request's full durable record. Gated with
  /// the read token -- see listRequests' own doc comment just above.
  Future<RequestSummary> getRequest(String id) async {
    final response = await _client.get(
      _uri('/requests/${Uri.encodeComponent(id)}'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// POST /requests: starts a request from the console
  /// instead of a terminal `factoryd submit`. Gated the same way
  /// approve/reject are
  /// (internal/api.Server.authorizeRequestWrite) -- the override token,
  /// not the start token startRun/checkProject use -- since this can
  /// only ever create a new request in `submitted`, the same class of
  /// write approve/reject already are, not POST /runs' stronger
  /// authorizeStart. The server enforces the actual workspace allowlist
  /// (see internal/api's workspaceAllowed doc comment); this method just
  /// sends the operator's choice and surfaces whatever the server
  /// decides via RunApiException.
  Future<RequestSummary> createRequest({
    required String workspace,
    required String text,
    String verifyCommand = '',
    String fullSuiteCommand = '',
    String preflightProfile = '',
    bool draftOracles = false,
    String? by,
  }) async {
    final response = await _client.post(
      _uri('/requests'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'workspace': workspace,
        'text': text,
        if (verifyCommand.isNotEmpty) 'verify_command': verifyCommand,
        if (fullSuiteCommand.isNotEmpty) 'full_suite_command': fullSuiteCommand,
        if (preflightProfile.isNotEmpty) 'preflight_profile': preflightProfile,
        if (draftOracles) 'draft_oracles': draftOracles,
        if (by != null && by.isNotEmpty) 'by': by,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /workspaces: every workspace POST /requests would currently
  /// accept, each with the console "New request" form's own
  /// resolved-verify-command hint. Read token, like listRequests.
  Future<List<WorkspaceHint>> listWorkspaces() async {
    final response = await _client.get(
      _uri('/workspaces'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    final values = jsonDecode(_bodyText(response)) as List<dynamic>;
    return values
        .map((value) => WorkspaceHint.fromJson(value as Map<String, dynamic>))
        .toList(growable: false);
  }

  /// GET /requests/{id}/oracle: the request-level oracle/ files with
  /// sha256, every problem approval would refuse, and the drafting status.
  /// Read token.
  Future<OracleListing> getRequestOracle(String id) async {
    final response = await _client.get(
      _uri('/requests/${Uri.encodeComponent(id)}/oracle'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return OracleListing.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /requests/{id}/oracle/{name}: one oracle file. The returned hash
  /// is computed over the bytes received, so an approval can be bound to
  /// exactly what was displayed.
  Future<OracleFileContent> getRequestOracleFile(String id, String name) =>
      _getOracleFile(
        '/requests/${Uri.encodeComponent(id)}/oracle/'
        '${Uri.encodeComponent(name)}',
      );

  /// GET /requests/{id}/tickets/{n}/oracle: ticket [n]'s materialized
  /// `<NNN>.oracle/` files (the ones plan approval pins), plan_review only.
  Future<OracleListing> getRequestTicketOracle(String id, int n) async {
    final response = await _client.get(
      _uri('/requests/${Uri.encodeComponent(id)}/tickets/$n/oracle'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return OracleListing.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /requests/{id}/tickets/{n}/oracle/{name}: one such file.
  Future<OracleFileContent> getRequestTicketOracleFile(
    String id,
    int n,
    String name,
  ) => _getOracleFile(
    '/requests/${Uri.encodeComponent(id)}/tickets/$n/oracle/'
    '${Uri.encodeComponent(name)}',
  );

  Future<OracleFileContent> _getOracleFile(String path) async {
    final response = await _client.get(
      _uri(path),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return OracleFileContent(
      text: decodeUtf8Escaping(response.bodyBytes),
      sha256: sha256HexBytes(response.bodyBytes),
    );
  }

  /// PUT /requests/{id}/oracle/RUN_COMMAND.txt: the one oracle file the
  /// console may edit, and only at oracle_review. A 422 carries the
  /// validation reason in [RunApiException.message]. Override-token gated.
  /// See [updateRequestSpec]'s own doc comment for [baseSha256]/409
  /// handling.
  Future<void> putRequestOracleRunCommand(
    String id,
    String content, {
    String? baseSha256,
  }) async {
    final response = await _client.put(
      _uri('/requests/${Uri.encodeComponent(id)}/oracle/RUN_COMMAND.txt'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'content': content,
        if (baseSha256 != null) 'base_sha256': baseSha256,
      }),
    );
    _requireSuccess(response);
  }

  /// POST /requests/{id}/approve: advances a request out of spec_review
  /// (to planning) or plan_review (to building). [by] optionally names the
  /// approving operator -- omitted (the default) sends no body at all,
  /// matching this method's behavior before [by] existed and the server's
  /// own fallback when By is absent.
  ///
  /// [expectedSha256], when provided, names -- per relPath ("spec.md" or
  /// "tickets/NNN.spec.md") -- the SHA-256 this console's own last fetch
  /// of that file's content hashed to (see content_hash.dart). The
  /// server refuses the approval if the file changed since, binding
  /// approval to the artifact shown, rather than approving content the
  /// operator never actually saw.
  Future<RequestSummary> approveRequest(
    String id, {
    String? by,
    Map<String, String>? expectedSha256,
  }) async {
    final hasBy = by != null && by.isNotEmpty;
    final hasHashes = expectedSha256 != null && expectedSha256.isNotEmpty;
    final response = await _client.post(
      _uri('/requests/${Uri.encodeComponent(id)}/approve'),
      headers: _writeHeaders(_overrideToken),
      body: (!hasBy && !hasHashes)
          ? null
          : jsonEncode({
              if (hasBy) 'by': by,
              if (hasHashes) 'expected_sha256': expectedSha256,
            }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// POST /requests/{id}/reject: sends a request in spec_review or
  /// plan_review back to the prior drafting state, with reason appended
  /// to request.md so the redraft sees it. [by] optionally names the
  /// rejecting operator, symmetric with [approveRequest]'s own [by] --
  /// omitted, the body is unchanged from before [by] existed.
  ///
  /// [to] ("plan" or "spec") sends a quarantined or halted request back
  /// instead, routed server-side to
  /// internal/request.SendBack rather than Reject -- see
  /// [RequestSummary.canSendBack]. Omitted (the default) is the prior,
  /// review-state-only behavior.
  Future<RequestSummary> rejectRequest(
    String id, {
    required String reason,
    String? by,
    String? to,
  }) async {
    final response = await _client.post(
      _uri('/requests/${Uri.encodeComponent(id)}/reject'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'reason': reason,
        if (by != null && by.isNotEmpty) 'by': by,
        if (to != null && to.isNotEmpty) 'to': to,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// PUT /requests/{id}/spec: overwrites spec.md in place with [content]
  /// while the request is in spec_review, so the operator can edit the
  /// drafted spec from the console (review-and-approve-in-place) instead
  /// of an external editor. The server re-validates [content] against the
  /// same structural check the drafting job itself applies, refusing with
  /// a 422 ([RunApiException.message] carries the reason) rather than
  /// saving something the pipeline would halt on later. Override-token
  /// gated, like [approveRequest]/[rejectRequest].
  ///
  /// [baseSha256], when given, is the sha256 of the content the editor was
  /// opened with (content_hash.dart's sha256Hex) -- the server refuses
  /// with 409 (`{"error", "current_sha256"}`, surfaced via
  /// [RunApiException.currentSha256]) if the file changed underneath the
  /// editor since -- closing the race where two edits land on stale
  /// content.
  Future<RequestSummary> updateRequestSpec(
    String id,
    String content, {
    String? baseSha256,
  }) async {
    final response = await _client.put(
      _uri('/requests/${Uri.encodeComponent(id)}/spec'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'content': content,
        if (baseSha256 != null) 'base_sha256': baseSha256,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// PUT /requests/{id}/tickets/{n}: [updateRequestSpec]'s own reasoning,
  /// one stage later -- overwrites ticket [n]'s plan file in place while
  /// the request is in plan_review. See [updateRequestSpec]'s own doc
  /// comment for [baseSha256]/409 handling.
  Future<RequestSummary> updateRequestTicket(
    String id,
    int n,
    String content, {
    String? baseSha256,
  }) async {
    final response = await _client.put(
      _uri('/requests/${Uri.encodeComponent(id)}/tickets/$n'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'content': content,
        if (baseSha256 != null) 'base_sha256': baseSha256,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// POST /requests/{id}/retry: recovers a halted/quarantined request,
  /// same identity/confirm shape as [rejectRequest]. 409 when the
  /// request is not currently in a state retry accepts.
  Future<RequestSummary> retryRequest(
    String id, {
    required String reason,
    String? by,
  }) async {
    final response = await _client.post(
      _uri('/requests/${Uri.encodeComponent(id)}/retry'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'reason': reason,
        if (by != null && by.isNotEmpty) 'by': by,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// POST /requests/{id}/resume: the operator's decision for a request in
  /// resume_review. [from] is `round` (continue the lost step) or `scratch`
  /// (rebuild the ticket). 409 when the request is not in resume_review or
  /// a round resume's preconditions do not hold ([RunApiException.message]
  /// carries the refusal reasons); 503 when the server could not check them
  /// (retryable, [RunApiException.isRetryable]).
  Future<RequestSummary> resumeRequest(
    String id, {
    required String from,
    String? by,
  }) async {
    final response = await _client.post(
      _uri('/requests/${Uri.encodeComponent(id)}/resume'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'from': from,
        if (by != null && by.isNotEmpty) 'by': by,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// POST /requests/{id}/cancel: same shape as [retryRequest], for
  /// abandoning a halted/quarantined request instead of recovering it.
  Future<RequestSummary> cancelRequest(
    String id, {
    required String reason,
    String? by,
  }) async {
    final response = await _client.post(
      _uri('/requests/${Uri.encodeComponent(id)}/cancel'),
      headers: _writeHeaders(_overrideToken),
      body: jsonEncode({
        'reason': reason,
        if (by != null && by.isNotEmpty) 'by': by,
      }),
    );
    _requireSuccess(response);
    return RequestSummary.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /requests/{id}/revisions: every rejected-spec/plan snapshot
  /// recorded for this request, oldest first. Read-token gated, like
  /// listRequests/getRequest.
  Future<List<RevisionSummary>> listRevisions(String id) async {
    final response = await _client.get(
      _uri('/requests/${Uri.encodeComponent(id)}/revisions'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    final values = jsonDecode(_bodyText(response)) as List<dynamic>;
    return values
        .map((value) => RevisionSummary.fromJson(value as Map<String, dynamic>))
        .toList(growable: false);
  }

  /// GET /requests/{id}/revisions/{n}: one revision's own recorded file
  /// contents, gated the same as [listRevisions].
  Future<RevisionDetail> getRevision(String id, int index) async {
    final response = await _client.get(
      _uri('/requests/${Uri.encodeComponent(id)}/revisions/$index'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return RevisionDetail.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// Fetches the full unified diff between a run's BaseSHA and ResultSHA.
  /// Throws [RunApiException] with status 409 if the run has no result yet.
  Future<RunDiff> getRunDiff(String id) async {
    final response = await _client.get(
      _uri('/runs/${Uri.encodeComponent(id)}/diff'),
      headers: _authHeaders(readToken),
    );
    _requireSuccess(response);
    return RunDiff.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// Fetches one run's durable release decision and the project kill-switch
  /// state it was evaluated against. Authenticated with the start token —
  /// the server treats this as a control-plane route (like GET /daemons)
  /// rather than an open read route, because the kill-switch history carries
  /// operator attribution, and it fails closed (403) when no token is
  /// configured server-side.
  Future<ReleaseView> getRunRelease(String id) async {
    final response = await _client.get(
      _uri('/runs/${Uri.encodeComponent(id)}/release'),
      headers: _authHeaders(_startToken),
    );
    _requireSuccess(response);
    return ReleaseView.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /projects/{project}/release: a project's kill-switch state and
  /// history by project id alone, no run required for that project to
  /// exist. See ProjectReleaseView's own doc comment for why this exists
  /// alongside getRunRelease.
  Future<ProjectReleaseView> getProjectRelease(String project) async {
    final response = await _client.get(
      _uri('/projects/${Uri.encodeComponent(project)}/release'),
      headers: _authHeaders(_startToken),
    );
    _requireSuccess(response);
    return ProjectReleaseView.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// GET /projects/{project}/stats: per-team acceptance/override-rate,
  /// quarantine-cause breakdown, and median accepted cost, by project id
  /// alone. See ProjectStats' own doc comment.
  Future<ProjectStats> getProjectStats(String project) async {
    final response = await _client.get(
      _uri('/projects/${Uri.encodeComponent(project)}/stats'),
      headers: _authHeaders(_startToken),
    );
    _requireSuccess(response);
    return ProjectStats.fromJson(
      jsonDecode(_bodyText(response)) as Map<String, dynamic>,
    );
  }

  /// Subscribes to a run's server-sent-event stream, reconnecting with
  /// exponential backoff for as long as the run stays nonterminal.
  ///
  /// Implemented as a streamed GET through this same [_client] rather than
  /// a native browser `EventSource` (found via a real GitHub Codex App
  /// review of this PR): `EventSource` cannot set an Authorization header
  /// at all, so an earlier version of this method sent the read bearer
  /// token as a `?token=` query parameter instead — a reusable credential
  /// placed directly in the request target, which a reverse proxy or
  /// access log sitting in front of a non-loopback deployment can persist,
  /// exposing it well beyond this one request's lifetime. A streamed
  /// `http.Request` carries the same Authorization header every other read
  /// method already sends, so the token never appears in the URL, and it
  /// works identically on every platform `package:http` supports (native
  /// `EventSource` is web-only, which is also why this repo used to need
  /// `sse_transport_web.dart`/`sse_transport_stub.dart` as a conditional
  /// import in the first place -- removed along with this change).
  ///
  /// That replacement traded away two things a native `EventSource` gives
  /// for free, both found via later GitHub Codex App review rounds on
  /// this same PR:
  ///
  /// - Automatic reconnection on a dropped connection: an intermediate
  ///   version left a transient network or proxy disconnect permanently
  ///   stale, surfacing once as an error and never subscribing again.
  ///   [_watchRunOnce] handles exactly one connection attempt, and this
  ///   wrapper retries it with exponential backoff whenever that attempt
  ///   ends without the run ever reaching a terminal state, stopping for
  ///   good only once one actually is.
  /// - Actually cancelling an in-flight connection: a version that fixed
  ///   the above still never cancelled the *active* inner subscription
  ///   when a listener (e.g. a disposed `RunDetailScreen`) cancelled this
  ///   outer stream while `_watchRunOnce` was mid-request -- since a
  ///   quarantined run can stay nonterminal indefinitely and the server
  ///   emits no heartbeat, navigating away could leave the HTTP
  ///   connection and its server-side handler alive forever. The active
  ///   [StreamSubscription] is now tracked so `onCancel` can cancel it
  ///   directly, the same way disposing a real `EventSource` would.
  ///
  /// Retrying is also not blanket-applied to every failure: a 4xx
  /// response (e.g. 403 after a rotated read token, 404 after the run is
  /// pruned) is permanent, not transient, and retrying it forever would
  /// leave the UI silently showing stale state with no visible error.
  /// Only network failures and non-4xx (5xx, or anything
  /// [_watchRunOnce] doesn't itself classify) are retried; a 4xx is
  /// forwarded to the listener as a real error and this stream then
  /// closes for good, matching `RunDetailScreen`'s own `onError` handling
  /// for a genuine, non-recoverable failure.
  ///
  /// A plain `async*` retry loop can implement neither of these
  /// correctly: Dart only checks whether a stream subscription has been
  /// cancelled at a `yield` statement, never at an ordinary `await`, and
  /// `await for` gives no handle to cancel the subscription it creates
  /// internally. A `StreamController` with an explicit `stopped` flag and
  /// a tracked, directly cancellable inner [StreamSubscription] (`.listen`
  /// instead of `await for`) closes both.
  Stream<Run> watchRun(String id) => _withReconnectBackoff(
    () => _watchRunOnce(id),
    isTerminal: (run) => run.isTerminal,
  );

  /// Subscribes to a run's progress feed (`GET /runs/{id}/progress`,
  /// progress-contract.md, Phase 4's follow-along console): SSE frames of
  /// `event: progress`/`data: <one raw JSON line>`, one per
  /// [ProgressEvent]. Built on the same [_withReconnectBackoff]/[_sseOnce]
  /// primitives [watchRun] uses -- see their doc comments for the
  /// reconnect/cancellation/permanent-failure mechanics this shares.
  ///
  /// Per the contract, the server sends every existing line first, then
  /// follows the file, and closes the connection for good once the run is
  /// terminal and the file has been drained -- a single `stage: "finished",
  /// event: "end"` line is always that last line, so it doubles as this
  /// stream's own terminal marker, the same role [Run.isTerminal] plays for
  /// [watchRun]. A run that is already terminal when this is called still
  /// gets its full recorded history this way (the server replays every
  /// existing line before closing), so callers can subscribe unconditionally
  /// rather than special-casing a one-shot fetch for a finished run.
  Stream<ProgressEvent> watchRunProgress(String id) => _withReconnectBackoff(
    () => _sseOnce(
      '/runs/${Uri.encodeComponent(id)}/progress',
      parseProgressEvent,
      closeWhenTrue: _isFinishedEvent,
    ),
    isTerminal: _isFinishedEvent,
  );

  /// Subscribes to the request board's server-sent-event stream
  /// (`GET /requests/events`): one [RequestSummary] per request whose
  /// durable record changed, forever -- unlike
  /// [watchRun], a request board has no terminal state of its own, so
  /// this stream only ever ends when the caller cancels it or a
  /// permanent (4xx) failure occurs (forwarded as an error, exactly like
  /// [watchRun]'s own permanent-failure handling).
  ///
  /// Built on the same [_withReconnectBackoff]/[_sseOnce] primitives
  /// [watchRun] uses -- see their doc comments for the reconnect,
  /// cancellation, and permanent-vs-transient-failure behavior both
  /// share. [onConnectionChange], when given, is called with `true` the
  /// moment an SSE connection is actually established (the server's 2xx
  /// response headers have arrived) and `false` whenever that connection
  /// ends and a reconnect attempt is about to begin -- RequestListScreen
  /// uses this to drive a live/last-updated/disconnected indicator that
  /// reflects real connection state, not just event recency (a quiet
  /// board with nothing to report can go a long time between events
  /// while perfectly connected).
  Stream<RequestSummary> watchRequests({
    void Function(bool)? onConnectionChange,
  }) => _withReconnectBackoff(
    () => _sseOnce(
      '/requests/events',
      parseRequestStateEvent,
      onOpen: onConnectionChange == null
          ? null
          : () => onConnectionChange(true),
    ),
    onAttemptEnded: onConnectionChange == null
        ? null
        : () => onConnectionChange(false),
  );

  /// GET /runs/{id}/log?follow=1: the run's build-loop log, tailed as it
  /// grows. Emits each chunk of text as the server writes it; the stream
  /// closes on its own once the server does
  /// (the run reaching a terminal state with nothing left to flush, per
  /// the server's own streamRunLog doc comment), or when the caller
  /// cancels first. Read-token gated, like [watchRun].
  ///
  /// Deliberately plain text with no SSE framing and no reconnect/
  /// backoff of its own -- this is an opt-in viewer a run detail screen
  /// only subscribes to while its own log pane is toggled on (so a
  /// hundred open tabs are not a hundred active tail streams), not a
  /// signal an operator depends on the continuity of the way the board's
  /// needs-human count is. A dropped connection just means toggling the
  /// pane off and back on reissues a fresh request, which -- per the
  /// server's own doc comment for this route -- returns the log's
  /// current full contents again, so nothing already seen is lost.
  ///
  /// The log is worker-authored, untrusted content (safety-contract.md's
  /// Acceptance/Filesystem trust-boundary rows) -- this client hands the
  /// raw decoded text straight to the caller with no interpretation of
  /// its own, and [RunDetailScreen]'s log pane renders it as plain
  /// [SelectableText], never as HTML or with link auto-detection.
  Stream<String> watchRunLog(String id) {
    late StreamController<String> controller;
    StreamSubscription<String>? subscription;
    // Same mid-connect cancellation as _sseOnce's own `cancelled` flag:
    // subscription is still null while `send` is in flight, so a cancel
    // then would otherwise leave the eventual ?follow=1 response attached
    // and open for good.
    var cancelled = false;

    Future<void> start() async {
      try {
        final request = http.Request(
          'GET',
          _uri('/runs/${Uri.encodeComponent(id)}/log?follow=1'),
        )..headers.addAll(_authHeaders(readToken));
        final response = await _client.send(request);
        if (cancelled) {
          unawaited(response.stream.listen((_) {}).cancel());
          return;
        }
        if (response.statusCode < 200 || response.statusCode >= 300) {
          final body = await response.stream.bytesToString();
          controller.addError(RunApiException(response.statusCode, body));
          await controller.close();
          return;
        }
        subscription = response.stream
            .transform(utf8.decoder)
            .listen(
              controller.add,
              onError: controller.addError,
              onDone: controller.close,
            );
      } on Object catch (error, stackTrace) {
        controller.addError(error, stackTrace);
        await controller.close();
      }
    }

    controller = StreamController<String>(
      onListen: start,
      onCancel: () {
        cancelled = true;
        subscription?.cancel();
      },
    );
    return controller.stream;
  }

  /// The reconnect-with-exponential-backoff wrapper behind [watchRun] and
  /// [watchRequests]: repeatedly calls [connect] for a new connection
  /// attempt, forwarding every value and (transient) error it produces,
  /// until either [isTerminal] says a produced value ends the stream for
  /// good, a permanent ([RunApiException] 4xx) error is forwarded, or the
  /// returned stream's own subscription is cancelled.
  ///
  /// [isTerminal] is optional -- [watchRequests] has no terminal state at
  /// all, so it omits it and this retries forever (matching a native
  /// `EventSource`'s own indefinite-retry behavior) until cancelled or a
  /// permanent failure ends it. [onAttemptEnded], when given, fires once
  /// per finished connection attempt that will be retried (not on the
  /// final, stream-ending attempt) -- see [watchRequests]'s own doc
  /// comment for why its caller uses this to flip its freshness
  /// indicator out of "live".
  ///
  /// Implemented as a [StreamController] with an explicit `stopped` flag
  /// and a tracked, directly cancellable inner [StreamSubscription]
  /// (`.listen`, not `await for`) rather than a plain `async*` retry
  /// loop -- see [watchRun]'s pre-extraction doc comment (still applic-
  /// able) for why: Dart only checks whether a stream subscription has
  /// been cancelled at a `yield` statement, never at an ordinary `await`,
  /// and `await for` gives no handle to cancel the subscription it
  /// creates internally.
  Stream<T> _withReconnectBackoff<T>(
    Stream<T> Function() connect, {
    bool Function(T)? isTerminal,
    void Function()? onAttemptEnded,
  }) {
    late StreamController<T> controller;
    var stopped = false;
    // A cancellable Timer, not a bare `await Future.delayed(backoff)`:
    // once a listener cancels mid-backoff, that Future has no cancel API
    // of its own, so it would still fire (and its containing test/widget
    // teardown would see it as a leaked pending timer) even though pump's
    // own `stopped` check discards its result. Tracked here so onCancel
    // can cancel it outright instead.
    Timer? pendingRetryTimer;
    // The active connection attempt's own subscription -- tracked so
    // onCancel can cancel it directly instead of only setting `stopped`,
    // which pump only ever notices *after* an event, an error, or the
    // stream's own completion, none of which a genuinely stuck
    // connection (server never closes, never sends another event) would
    // otherwise ever produce.
    StreamSubscription<T>? activeSubscription;

    Future<void> pump() async {
      var backoff = _watchInitialBackoff;
      while (!stopped) {
        var reachedTerminal = false;
        var permanentFailure = false;
        final attemptDone = Completer<void>();
        activeSubscription = connect().listen(
          (value) {
            controller.add(value);
            if (isTerminal != null && isTerminal(value)) {
              reachedTerminal = true;
            }
          },
          onError: (Object error) {
            // A 4xx is permanent -- forwarded to the listener as a real
            // error, not swallowed and retried forever, so a rotated
            // read token or a pruned run/board surfaces visibly instead
            // of leaving the UI silently stale. Anything else (a network
            // failure, a 5xx) is transient: swallowed here, exactly like
            // a native EventSource would silently retry it, with the
            // loop below reaching this same connection attempt again.
            if (error is RunApiException &&
                error.statusCode >= 400 &&
                error.statusCode < 500) {
              permanentFailure = true;
              controller.addError(error);
            }
          },
          onDone: () {
            if (!attemptDone.isCompleted) attemptDone.complete();
          },
          cancelOnError: false,
        );
        await attemptDone.future;
        activeSubscription = null;
        if (stopped) return;
        if (reachedTerminal || permanentFailure) {
          await controller.close();
          return;
        }
        onAttemptEnded?.call();
        final delayComplete = Completer<void>();
        pendingRetryTimer = Timer(backoff, delayComplete.complete);
        await delayComplete.future;
        pendingRetryTimer = null;
        if (stopped) return;
        final nextMillis = (backoff.inMilliseconds * 2).clamp(
          1,
          _watchMaxBackoff.inMilliseconds,
        );
        backoff = Duration(milliseconds: nextMillis);
      }
    }

    controller = StreamController<T>(
      onListen: pump,
      onCancel: () {
        stopped = true;
        pendingRetryTimer?.cancel();
        activeSubscription?.cancel();
      },
    );
    return controller.stream;
  }

  /// One single SSE connection attempt for [watchRun] -- parses
  /// `/runs/{id}/events`' frames as [Run]s via [parseStateEvent] and
  /// closes for good the moment one is terminal. See [_sseOnce]'s own
  /// doc comment for the connection/parsing/cancellation mechanics this
  /// shares with [watchRequests]'s own one-connection-attempt method.
  Stream<Run> _watchRunOnce(String id) => _sseOnce(
    '/runs/${Uri.encodeComponent(id)}/events',
    parseStateEvent,
    closeWhenTrue: (run) => run.isTerminal,
  );

  /// One single SSE connection attempt: connects to [path] and parses
  /// each "event: state"/"data: ..." frame (writeStateEvent's exact wire
  /// shape on the server) via [parseFrame], generalized from [watchRun]'s
  /// earlier implementation so [watchRequests] (`/requests/events`) can
  /// reuse the identical connection/parsing/cancellation mechanics rather
  /// than a second hand-rolled SSE client -- see [watchRun]'s own (now
  /// historical, still accurate) doc comment for why a plain `async*`
  /// generator can't give a caller a real cancel handle the way this
  /// `StreamController`-based shape does.
  ///
  /// [closeWhenTrue], when given, is checked against every successfully
  /// parsed value; the first one it accepts closes this stream for good
  /// (used by [_watchRunOnce] for a terminal [Run] -- [watchRequests] has
  /// no such condition and omits it, so its stream instead runs until the
  /// server closes the connection or the caller cancels).
  Stream<T> _sseOnce<T>(
    String path,
    T Function(String frame) parseFrame, {
    bool Function(T)? closeWhenTrue,
    void Function()? onOpen,
  }) {
    late StreamController<T> controller;
    StreamSubscription<String>? linesSubscription;
    // Found in review: onCancel below only cancelled linesSubscription,
    // which is still null for as long as `await _client.send(request)`
    // hasn't returned yet -- a screen disposed while the connection is
    // still being established (a realistic race for /requests/events,
    // which is opened on every board mount and never closes on its own)
    // saw nothing cancelled at all. start() would then go on to attach a
    // real listener to an already-abandoned stream once the response
    // finally arrived, leaking that HTTP connection and its server-side
    // handler for the rest of the session. The only await before
    // linesSubscription is attached is the `send` call below; checking
    // `cancelled` right after it is therefore sufficient -- nothing else
    // suspends between that check and calling `.listen`.
    var cancelled = false;

    Future<void> start() async {
      try {
        final request = http.Request('GET', _uri(path))
          ..headers.addAll(_authHeaders(readToken));
        final response = await _client.send(request);
        if (cancelled) {
          unawaited(response.stream.listen((_) {}).cancel());
          return;
        }
        if (response.statusCode < 200 || response.statusCode >= 300) {
          final body = await response.stream.bytesToString();
          controller.addError(RunApiException(response.statusCode, body));
          await controller.close();
          return;
        }
        onOpen?.call();

        // A minimal SSE line parser: accumulates the server's own
        // "event: state"/"data: ..." lines verbatim until a blank line
        // terminates one event, then hands the accumulated text to
        // [parseFrame].
        final buffer = StringBuffer();
        linesSubscription = response.stream
            .transform(utf8.decoder)
            .transform(const LineSplitter())
            .listen(
              (line) {
                if (line.isEmpty) {
                  if (buffer.isEmpty) return;
                  final frame = '$buffer\n';
                  buffer.clear();
                  final T value;
                  try {
                    value = parseFrame(frame);
                  } on Object catch (error, stackTrace) {
                    controller.addError(error, stackTrace);
                    return;
                  }
                  controller.add(value);
                  if (closeWhenTrue != null && closeWhenTrue(value)) {
                    linesSubscription?.cancel();
                    controller.close();
                  }
                  return;
                }
                buffer.writeln(line);
              },
              onError: controller.addError,
              onDone: controller.close,
            );
      } on Object catch (error, stackTrace) {
        controller.addError(error, stackTrace);
        await controller.close();
      }
    }

    controller = StreamController<T>(
      onListen: start,
      onCancel: () {
        cancelled = true;
        linesSubscription?.cancel();
      },
    );
    return controller.stream;
  }

  void close() => _client.close();

  // No base (same-origin console): path is already the whole request --
  // Uri.parse keeps it relative, which the browser resolves against the
  // page's own origin for us, same-origin, no CORS involved.
  Uri _uri(String path) => _baseUri?.resolve(path) ?? Uri.parse(path);

  Map<String, String> _writeHeaders(String? token) => {
    'Content-Type': 'application/json',
    ..._authHeaders(token),
  };

  Map<String, String> _authHeaders(String? token) => {
    if (token case final token? when token.isNotEmpty)
      'Authorization': 'Bearer $token',
  };

  static void _requireSuccess(http.Response response) {
    if (response.statusCode < 200 || response.statusCode >= 300) {
      throw RunApiException(response.statusCode, _bodyText(response));
    }
  }
}

// The server's JSON is UTF-8 but declares no charset, and package:http then
// decodes `Response.body` as Latin-1 -- non-ASCII text (a title, an oracle
// problem naming a file) would arrive as mojibake. Decode the bytes instead.
String _bodyText(http.Response response) =>
    utf8.decode(response.bodyBytes, allowMalformed: true);

Run parseStateEvent(String input) => _decodeStateEvent(input, Run.fromJson);

/// The `/requests/events` counterpart of [parseStateEvent] -- same
/// "event: state"/"data: ..." wire shape, one
/// [RequestSummary] per frame instead of one [Run].
RequestSummary parseRequestStateEvent(String input) =>
    _decodeStateEvent(input, RequestSummary.fromJson);

/// The `/runs/{id}/progress` (progress-contract.md) counterpart of
/// [parseStateEvent] -- same one-frame-per-event wire shape, but the frame
/// name is `event: progress`, not `event: state`, and each frame decodes to
/// one [ProgressEvent].
ProgressEvent parseProgressEvent(String input) =>
    _decodeStateEvent(input, ProgressEvent.fromJson, eventName: 'progress');

/// [watchRunProgress]'s own terminal marker: progress-contract.md's single
/// `stage: "finished", event: "end"` line, always the last line the server
/// ever writes for a run.
bool _isFinishedEvent(ProgressEvent event) =>
    event.stage == 'finished' && event.event == 'end';

T _decodeStateEvent<T>(
  String input,
  T Function(Map<String, dynamic>) fromJson, {
  String eventName = 'state',
}) {
  String? event;
  final data = <String>[];
  for (final line in const LineSplitter().convert(input)) {
    if (line.startsWith('event:')) {
      event = line.substring('event:'.length).trimLeft();
    } else if (line.startsWith('data:')) {
      data.add(line.substring('data:'.length).trimLeft());
    }
  }
  if (event != eventName || data.isEmpty) {
    throw FormatException('Expected an SSE $eventName event with data');
  }
  return fromJson(jsonDecode(data.join('\n')) as Map<String, dynamic>);
}

class RunApiException implements Exception {
  const RunApiException(this.statusCode, this.body);

  final int statusCode;
  final String body;

  // message extracts the server's own `{"error": "..."}` shape (see
  // internal/api's writeError) so a caller can show the operator-facing
  // text alone rather than the raw JSON envelope -- found via a real
  // console live-validation run, 2026-09-08: before this, a project-
  // bootstrap preflight failure's real diagnostic (which artifact, which
  // path, why) rendered as one unparsed, escaped JSON blob, exactly the
  // moment an operator most needs it legible. Falls back to the raw body
  // whenever it isn't that exact shape (a non-JSON body, or a JSON value
  // whose "error" field isn't a string) rather than throwing a second
  // exception out of an exception's own display path.
  String get message {
    try {
      final decoded = jsonDecode(body);
      if (decoded case {'error': final String error}) return error;
    } on FormatException {
      // Not JSON -- fall through to the raw body below.
    }
    return body;
  }

  // currentSha256 extracts a 409 conflict's own `current_sha256` field
  // (PUT /requests/{id}/spec|tickets/{n}|oracle/RUN_COMMAND.txt's own
  // conflict shape when [base_sha256] didn't match) -- null for any other
  // status code or body shape, so a caller can check `statusCode == 409 &&
  // currentSha256 != null` to distinguish a real edit conflict from an
  // unrelated 409.
  String? get currentSha256 {
    if (statusCode != 409) return null;
    try {
      final decoded = jsonDecode(body);
      if (decoded case {'current_sha256': final String hash}) return hash;
    } on FormatException {
      // Not JSON -- no hash to extract.
    }
    return null;
  }

  // messageParts splits message on the " | " delimiter
  // runProjectBootstrapCheck (cmd/factoryd/main.go) joins its own
  // per-artifact failures with, so a caller can render each failing
  // check (product_spec_frozen, program_design_structure,
  // architecture_structure, ticket_structure) as its own line instead of
  // one long run-on sentence. A message with no such delimiter (every
  // other kind of error this client surfaces) returns as its own single
  // element, so a caller can use this unconditionally rather than
  // special-casing which errors happen to be multi-part.
  /// A 503 is the server failing to check, not refusing: trying again may work.
  bool get isRetryable => statusCode == 503;

  List<String> get messageParts => message.split(' | ');

  @override
  String toString() => 'Run API request failed ($statusCode): $body';
}
