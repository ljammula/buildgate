import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart' show Clipboard, ClipboardData;

import 'api_client.dart';
import 'approve_reject.dart';
import 'content_hash.dart' show expectedSha256For, sha256Hex;
import 'text_escape.dart' show escapeInvisible;
import 'diff_screen.dart';
import 'elapsed.dart';
import 'error_display.dart';
import 'markdown_view.dart';
import 'models.dart';
import 'oracle_review_panel.dart';
import 'request_list_screen.dart';
import 'run_detail_screen.dart';
import 'run_list_screen.dart' show StateBadge;
import 'status.dart' show stateLabel;
import 'text_diff.dart';
import 'ticket_rollup.dart';

/// Detail view for one request: the header chip/badge,
/// spec.md's or a ticket's plan file rendered as monospace text, the
/// Approve/Reject actions (review states only, calling
/// RunApi.approveRequest/rejectRequest), and links to each ticket's own
/// run and PR.
class RequestDetailScreen extends StatefulWidget {
  const RequestDetailScreen({
    required this.api,
    required this.requestId,
    this.initialRequest,
    super.key,
  });

  final RunApi api;
  final String requestId;
  final RequestSummary? initialRequest;

  @override
  State<RequestDetailScreen> createState() => _RequestDetailScreenState();
}

class _RequestDetailScreenState extends State<RequestDetailScreen> {
  RequestSummary? _request;
  Object? _error;
  Object? _actionError;
  // A failed resume, with the refusal state it answered (_resumeKey): shown
  // only while the request still has that state, so a poll that changes the
  // reasons clears it.
  Object? _resumeError;
  String _resumeKey = '';
  bool _acting = false;
  bool _loading = false;
  // True only once GET /requests/{id} has itself returned successfully
  // -- see _RequestSections.detailLoaded's own doc comment for why
  // `_request != null` alone (true from initState via initialRequest)
  // isn't enough to gate Approve/Reject on.
  bool _detailLoaded = false;
  // What the plan_review ticket-oracle panel has displayed (null until it
  // first reports); plan approval waits for it and sends its hashes.
  TicketOracleShown? _ticketOracle;
  // Bumped after a failed approval so the ticket-oracle panel is rebuilt
  // from scratch: whatever was refused (a file changed since it was shown)
  // must be listed and opened again.
  int _ticketOracleNonce = 0;
  // GET /requests/{id} now returns cost_summary directly, so
  // _request.costSummary is populated once _load completes. Kept as a
  // getter with initialRequest as a
  // fallback purely for the brief window before that first load resolves
  // -- e.g. when the operator navigates here straight from the board and
  // the confirm sheet renders before the detail fetch returns.
  CostSummary? get _costSummary =>
      _request?.costSummary ?? widget.initialRequest?.costSummary;

  // Live updates: subscribes to the same board-wide /requests/events
  // stream RequestListScreen already uses, filtered client-side to this
  // screen's own id -- there is no per-id server route, so every event is
  // received and only a matching one is applied. Reconnects/backs off on
  // its own (see RunApi.watchRequests' own doc comment); a permanent
  // (4xx) failure is swallowed here rather than surfaced as a hard error,
  // since the manual Refresh button and _load's own retry remain the
  // fallback -- a live-update outage must never block reading or acting
  // on the last successfully loaded request.
  StreamSubscription<RequestSummary>? _liveSubscription;

  @override
  void initState() {
    super.initState();
    _request = widget.initialRequest;
    _load();
    _liveSubscription = widget.api.watchRequests().listen(
      (event) {
        if (!mounted || event.id != widget.requestId) return;
        final current = _request;
        if (current != null &&
            current.updatedAt == event.updatedAt &&
            current.state == event.state) {
          return;
        }
        // The event is the board's summary shape -- no spec, ticket
        // content or revisions -- so it is only a signal to re-fetch the
        // full detail, never a replacement for it. Applying it directly
        // (an earlier version of this live-update handling) blanked the
        // Spec section on a live redraft and set _detailLoaded on content
        // the operator never saw, reopening the approve-what-you-saw gap
        // _load's own reset closes (found by scripts/console-walk,
        // 2026-09-24). An open
        // editor survives the re-fetch: its text lives in its own
        // controller and no section key includes updatedAt.
        _reloadForEvent();
      },
      onError: (Object _) {
        // Permanent failure (rotated token, pruned board route on an
        // older server) -- nothing to do here; see this field's own doc
        // comment for why this stays silent rather than surfacing an
        // error banner on top of otherwise-working manual refresh.
      },
      cancelOnError: true,
    );
  }

  @override
  void dispose() {
    _liveSubscription?.cancel();
    super.dispose();
  }

  // Coalesces live events: at most one re-fetch in flight, plus one
  // follow-up if more events arrived meanwhile.
  bool _eventReloadRunning = false;
  bool _eventReloadPending = false;

  Future<void> _reloadForEvent() async {
    if (_eventReloadRunning) {
      _eventReloadPending = true;
      return;
    }
    _eventReloadRunning = true;
    try {
      do {
        _eventReloadPending = false;
        await _load();
      } while (_eventReloadPending && mounted);
    } finally {
      _eventReloadRunning = false;
    }
  }

  Future<void> _load() async {
    // Reset readiness at the start of every load, not just the first one.
    // Found in review: without this, a Refresh that starts (or fails)
    // after the first successful load left Approve/Reject enabled
    // against the stale displayed content -- an operator could approve
    // while a fresher fetch was still in flight or had failed, hashing
    // and approving whatever the server's own copy of the file was by
    // then, not what they last saw on screen.
    setState(() {
      _loading = true;
      _detailLoaded = false;
    });
    try {
      final request = await widget.api.getRequest(widget.requestId);
      if (mounted) {
        setState(() {
          _request = request;
          _error = null;
          _detailLoaded = true;
        });
      }
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  // _approve/_showReject go through approve_reject.dart's shared flow:
  // resolve the operator's stored name (prompting once if unset), show
  // the confirm sheet/require a reason, then call
  // the write endpoint -- see runApproveFlow/runRejectFlow's own doc
  // comments for the null-on-abandon contract (no override token
  // configured, or the operator cancels a step) both share.
  Future<void> _approve([Map<String, String>? expectedSha256]) async {
    final request = _request;
    if (request == null || !_detailLoaded) return;
    Map<String, String>? expected = expectedSha256;
    if (expected == null && request.state == 'plan_review') {
      final oracle = _ticketOracle;
      if (oracle == null || !oracle.complete) return;
      expected = {...expectedSha256For(request), ...oracle.hashes};
    }
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      // approveRequest's own response carries the same enriched shape
      // GET /requests/{id} does (title, spec, ticket content included) --
      // no separate refetch needed, the same way RunDetailScreen's own
      // _showOverride uses overrideRun's response directly rather than
      // re-fetching after it.
      final updated = await runApproveFlow(
        context,
        api: widget.api,
        request: request,
        costSummary: _costSummary,
        expectedSha256: expected,
      );
      if (updated != null && mounted) setState(() => _request = updated);
    } on Object catch (error) {
      if (mounted) {
        setState(() {
          _actionError = error;
          _ticketOracle = null;
          _ticketOracleNonce++;
        });
      }
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  Future<void> _showReject() async {
    final request = _request;
    if (request == null || !_detailLoaded) return;
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      final updated = await runRejectFlow(
        context,
        api: widget.api,
        request: request,
      );
      if (updated != null && mounted) setState(() => _request = updated);
    } on Object catch (error) {
      if (mounted) setState(() => _actionError = error);
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  // _retry/_cancel: the same shape as _showReject above, recovering a
  // halted/quarantined request from the console instead of only via
  // `factoryd retry`/CLI.
  Future<void> _retry() async {
    final request = _request;
    if (request == null || !_detailLoaded) return;
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      final updated = await runRetryFlow(
        context,
        api: widget.api,
        request: request,
      );
      if (updated != null && mounted) setState(() => _request = updated);
    } on Object catch (error) {
      if (mounted) setState(() => _actionError = error);
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  Future<void> _resume(String from) async {
    final request = _request;
    if (request == null || !_detailLoaded) return;
    setState(() {
      _acting = true;
      _actionError = null;
      _resumeError = null;
    });
    try {
      final updated = await runResumeFlow(
        context,
        api: widget.api,
        request: request,
        from: from,
      );
      if (updated != null && mounted) setState(() => _request = updated);
    } on Object catch (error) {
      if (mounted) {
        setState(() {
          _resumeError = error;
          _resumeKey = resumeStateKey(request);
        });
      }
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  Future<void> _cancel() async {
    final request = _request;
    if (request == null || !_detailLoaded) return;
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      final updated = await runCancelFlow(
        context,
        api: widget.api,
        request: request,
      );
      if (updated != null && mounted) setState(() => _request = updated);
    } on Object catch (error) {
      if (mounted) setState(() => _actionError = error);
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  // _sendBack: the same shape as _retry/_cancel above, sending a
  // quarantined/halted request back to planning or spec drafting instead
  // of rebuilding or abandoning it.
  Future<void> _sendBack() async {
    final request = _request;
    if (request == null || !_detailLoaded) return;
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      final updated = await runSendBackFlow(
        context,
        api: widget.api,
        request: request,
      );
      if (updated != null && mounted) setState(() => _request = updated);
    } on Object catch (error) {
      if (mounted) setState(() => _actionError = error);
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final request = _request;
    return Scaffold(
      appBar: AppBar(
        title: Text(_titleFor(request)),
        actions: [
          IconButton(
            key: const ValueKey('request-detail-refresh-button'),
            onPressed: _loading ? null : _load,
            tooltip: 'Refresh',
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: switch ((request, _error)) {
        (null, final Object error) => Center(
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: ErrorCallout(error: error),
          ),
        ),
        (null, null) => const Center(child: CircularProgressIndicator()),
        (final RequestSummary value, _) => _RequestSections(
          api: widget.api,
          request: value,
          loadError: _error,
          detailLoaded: _detailLoaded,
          onApprove: _approve,
          ticketOracle: _ticketOracle,
          ticketOracleNonce: _ticketOracleNonce,
          onTicketOracleChanged: (shown) =>
              setState(() => _ticketOracle = shown),
          onReject: _showReject,
          onRetry: _retry,
          onCancel: _cancel,
          onResume: _resume,
          onSendBack: _sendBack,
          acting: _acting,
          actionError: _actionError,
          resumeError:
              _resumeError != null && _resumeKey == resumeStateKey(value)
              ? _resumeError
              : null,
          onRequestUpdated: (updated) => setState(() => _request = updated),
        ),
      },
    );
  }

  String _titleFor(RequestSummary? request) {
    if (request == null) return 'Request detail';
    // A derived, plain-text short title, not request.md's first
    // line verbatim -- see RequestSummary.shortTitle's own doc comment.
    return request.shortTitle;
  }
}

class _RequestSections extends StatefulWidget {
  const _RequestSections({
    required this.api,
    required this.request,
    required this.loadError,
    required this.detailLoaded,
    required this.onApprove,
    required this.ticketOracle,
    required this.ticketOracleNonce,
    required this.onTicketOracleChanged,
    required this.onReject,
    required this.onRetry,
    required this.onCancel,
    required this.onResume,
    required this.onSendBack,
    required this.acting,
    required this.actionError,
    required this.resumeError,
    required this.onRequestUpdated,
  });

  final RunApi api;
  final RequestSummary request;
  final Object? loadError;
  // Whether GET /requests/{id} has itself returned successfully for the
  // request currently shown -- as opposed to `request` merely being
  // non-null because it still holds the board-row summary passed in as
  // initialRequest. Found in review: gating Approve/Reject on `request
  // != null` alone let an operator approve/reject before the spec/ticket
  // content had ever loaded (or after it failed to), the same gap fixed
  // on the triage screen. False forever if the fetch fails, which is the
  // fail-closed direction wanted here.
  final bool detailLoaded;
  final Future<void> Function([Map<String, String>? expectedSha256]) onApprove;
  final TicketOracleShown? ticketOracle;
  final int ticketOracleNonce;
  final ValueChanged<TicketOracleShown> onTicketOracleChanged;
  final VoidCallback onReject;
  // Recovery actions -- only ever invoked from the halted/
  // quarantined callout below, same shape as onReject.
  final VoidCallback onRetry;
  final VoidCallback onCancel;
  // resume_review's Resume (`round`) / Rebuild from scratch (`scratch`).
  final void Function(String from) onResume;
  // Send-back -- only ever invoked from the halted/quarantined
  // callout, same shape as onRetry/onCancel.
  final VoidCallback onSendBack;
  final bool acting;
  final Object? actionError;
  // The failed resume answering the request's current refusal state, if any.
  final Object? resumeError;
  // Called with the fresh RequestSummary a successful spec/ticket edit
  // returns (_saveSpec/_saveTicket below), so the parent screen's own
  // state -- the single source of truth every other section here reads
  // from -- picks up the saved content immediately, the same way
  // onApprove/onReject's own results already do.
  final ValueChanged<RequestSummary> onRequestUpdated;

  @override
  State<_RequestSections> createState() => _RequestSectionsState();
}

class _RequestSectionsState extends State<_RequestSections> {
  // Revision compare state -- opt-in, off by default: the default view
  // is still the current content, diff is opt-in.
  bool _compareEnabled = false;
  List<RevisionSummary>? _revisions;
  Object? _revisionsError;
  bool _loadingRevisions = false;
  int? _selectedRevisionIndex;
  RevisionDetail? _revisionDetail;
  Object? _revisionDetailError;
  bool _loadingRevisionDetail = false;

  Future<void> _toggleCompare(bool enabled) async {
    setState(() => _compareEnabled = enabled);
    if (enabled && _revisions == null && !_loadingRevisions) {
      await _loadRevisions();
    }
  }

  Future<void> _loadRevisions() async {
    setState(() {
      _loadingRevisions = true;
      _revisionsError = null;
    });
    try {
      final revisions = await widget.api.listRevisions(widget.request.id);
      if (mounted) setState(() => _revisions = revisions);
      // Auto-select a sole prior revision -- no reason to make an
      // operator pick from a dropdown of exactly one entry.
      if (mounted && revisions.length == 1) {
        await _selectRevision(revisions.single.index);
      }
    } on Object catch (error) {
      if (mounted) setState(() => _revisionsError = error);
    } finally {
      if (mounted) setState(() => _loadingRevisions = false);
    }
  }

  Future<void> _selectRevision(int index) async {
    setState(() {
      _selectedRevisionIndex = index;
      _revisionDetail = null;
      _revisionDetailError = null;
      _loadingRevisionDetail = true;
    });
    try {
      final detail = await widget.api.getRevision(widget.request.id, index);
      if (mounted) setState(() => _revisionDetail = detail);
    } on Object catch (error) {
      if (mounted) setState(() => _revisionDetailError = error);
    } finally {
      if (mounted) setState(() => _loadingRevisionDetail = false);
    }
  }

  // _currentContentFor resolves what a revision's snapshotted path is
  // being compared against: spec.md against the request's own current
  // spec, a ticket spec path against that ticket's own current content.
  // `path` is the relative key SnapshotRevision uses on the Go side
  // (internal/request/revisions.go), e.g. "tickets/001.spec.md"; a
  // ticket's own `specPath` is an absolute filesystem path in production
  // (set in cmd/factoryd/request_driver.go), so match by suffix rather
  // than exact equality -- the console never sees the request's data
  // directory to normalize the other way.
  String _currentContentFor(String path) {
    if (path == 'spec.md') return widget.request.spec;
    for (final ticket in widget.request.tickets) {
      final specPath = ticket.specPath.replaceAll('\\', '/');
      if (specPath == path || specPath.endsWith('/$path'))
        return ticket.content;
    }
    return '';
  }

  String _diffTextFor(RevisionDetail detail) {
    final buffer = StringBuffer();
    for (final entry in detail.files.entries) {
      buffer.writeln('--- ${entry.key} (revision ${detail.index})');
      buffer.writeln('+++ ${entry.key} (current)');
      buffer.write(unifiedLineDiff(entry.value, _currentContentFor(entry.key)));
      buffer.writeln();
    }
    return buffer.toString();
  }

  // _nextActionText: the server's own single next-step sentence,
  // falling back to the pre-existing awaitingPullRequestLabel for a
  // server predating next_action -- never both at once, and never this
  // screen's own guess for any other state without a server answer.
  String? _nextActionText(RequestSummary request) {
    if (request.nextAction.isNotEmpty) return request.nextAction;
    if (request.awaitingPullRequest) return request.awaitingPullRequestLabel;
    return null;
  }

  @override
  Widget build(BuildContext context) {
    final request = widget.request;
    final api = widget.api;
    final badge = waitingBadgeLabel(request, DateTime.now());
    // Deliberately a literal check, not requestStageGroup(...) ==
    // RequestStageGroup.review: that grouping now also covers `halted`
    // (a board/needs-you concern -- see its own doc comment), but the
    // Approve/Reject actions below are legal only from these two states
    // -- the server's own Approve/Reject refuse any other state,
    // including halted. Conflating the two showed Approve/Reject buttons
    // for a halted request that would just 500 on click.
    // plan approval pins every ticket's materialized oracle files, so it
    // waits for the ticket-oracle panel to have shown them all.
    final awaitingTicketOracle =
        request.state == 'plan_review' && request.tickets.isNotEmpty;
    final isReviewState =
        request.state == 'spec_review' ||
        request.state == 'oracle_review' ||
        request.state == 'plan_review';

    final ticketsSection = _Section(
      title: 'Tickets',
      children: [
        for (final ticket in request.tickets)
          // Keyed by ticket index, not the list position alone
          // -- without a key, Flutter's own element-reuse-by-type
          // rule kept the *first* State object (and its already-
          // fetched Run) attached whenever a ticket's own runId
          // changed (a retry landing a new run) without the ticket
          // list itself reordering, so a retried ticket's new run
          // never showed until a full screen remount.
          _TicketLinks(
            key: ValueKey('ticket-links-${ticket.index}'),
            api: api,
            ticket: ticket,
          ),
      ],
    );
    // Once tickets are building (or stopped), their runs are what the
    // operator is following, so they go right under the stepper instead
    // of below the full plan text.
    const runPhaseStates = {'building', 'pr_review', 'halted', 'quarantined'};
    final ticketsFirst =
        request.tickets.isNotEmpty && runPhaseStates.contains(request.state);

    return ListView(
      padding: const EdgeInsets.all(16),
      children: [
        // The single factory-authored "Next" line: the one
        // sentence an operator reads to know what to do, rendered
        // prominently above everything else -- replaces this screen's own
        // prior hard-coded next-step copy (awaitingPullRequestLabel)
        // wherever the server supplies its own answer. Suppressed for
        // halted/quarantined (below): the recovery callout already shows
        // this same sentence as its own explanation, right above Retry/
        // Cancel -- a second, identical banner above it would be noise,
        // not a second answer.
        // Also suppressed in review states: the Review section's own
        // Approve/Request changes buttons are the next action there, and
        // next_action's text names the CLI equivalents (it is shared with
        // `factoryd watch`), which read as the wrong instruction on a
        // screen that has the buttons (console-walk, 2026-09-24).
        if (request.state != 'halted' &&
            request.state != 'quarantined' &&
            request.state != 'resume_review' &&
            !isReviewState)
          if (_nextActionText(request) case final next? when next.isNotEmpty)
            _NextActionBanner(text: next),
        // Pipeline stepper: "where is my ask in the pipeline?" at a
        // glance, above everything else on this screen -- see
        // _PipelineStepper's own doc comment.
        _Section(
          title: 'Pipeline',
          children: [_PipelineStepper(request: request)],
        ),
        // Halted/quarantined recovery callout: a kind-specific
        // explanation plus Retry/Cancel actions and the copyable CLI
        // equivalent, so a request stuck here never reads as "waiting on
        // you" with nothing actually offered.
        if (request.state == 'quarantined' || request.state == 'halted')
          _RecoveryCallout(
            api: api,
            request: request,
            acting: widget.acting,
            onRetry: widget.onRetry,
            onCancel: widget.onCancel,
            onSendBack: widget.onSendBack,
          ),
        if (request.state == 'resume_review')
          _ResumeCallout(
            api: api,
            request: request,
            acting: widget.acting,
            actionError: widget.actionError,
            resumeError: widget.resumeError,
            onResume: widget.onResume,
            onCancel: widget.onCancel,
          ),
        if (ticketsFirst) ticketsSection,
        _Section(
          title: 'Request',
          children: [
            _Field(
              'State',
              Wrap(
                spacing: 8,
                crossAxisAlignment: WrapCrossAlignment.center,
                children: [
                  RequestStageChip(
                    state: request.state,
                    awaitingPullRequest: request.awaitingPullRequest,
                    waitingOn: request.waitingOn,
                    needsYou:
                        requestStageGroupOf(request) ==
                        RequestStageGroup.review,
                  ),
                  if (request.runningJob case final job?)
                    Chip(
                      key: const ValueKey('request-detail-active-job'),
                      avatar: const Icon(Icons.memory, size: 18),
                      label: Text(job.label),
                    ),
                  if (badge != null)
                    Chip(
                      key: const ValueKey('request-detail-waiting-badge'),
                      label: Text(
                        badge,
                        style: const TextStyle(color: Colors.white),
                      ),
                      backgroundColor: Colors.deepOrange,
                    ),
                ],
              ),
            ),
            _TextField('Project', request.project),
            _TextField('Workspace', request.workspace),
            // Local time, full UTC on hover/long-press -- see
            // LocalTimeText's own doc comment (elapsed.dart).
            _Field('Submitted', LocalTimeText(request.submittedAt)),
            _Field('Updated', LocalTimeText(request.updatedAt)),
            if (request.ticketCount > 0)
              _TextField(
                'Ticket',
                '${request.ticketIndex} / ${request.ticketCount}',
              ),
            // The "Next" banner above now carries this screen's own single
            // next-step line -- this field stays only for the
            // Detail/Error line, never a second, possibly-conflicting
            // "Next".
            if (request.error.isNotEmpty)
              _TextField(
                request.awaitingPullRequest ? 'Detail' : 'Error',
                request.error,
              ),
            if (widget.loadError != null)
              _TextField('Live updates', 'Refresh failed: ${widget.loadError}'),
            // Ticket fan-out roll-up, shown on the
            // detail header for the same states the board row shows it
            // for -- see TicketRollupStrip's own doc comment for how
            // "building" is approximated for a ticket with a runId but no
            // prState yet.
            if (request.state == 'building' || request.state == 'pr_review')
              Padding(
                padding: const EdgeInsets.only(top: 4),
                child: TicketRollupStrip(request: request),
              ),
          ],
        ),
        // Audit line: who approved this request and
        // when, straight from fields request.Request already carries on
        // every GET /requests/{id} response (ApprovedBy/ApprovedAt --
        // models.dart's RequestSummary.approvedBy/approvedAt). The
        // rejection history below is the structured Rejections field --
        // an earlier text-parsing approach for this was never implemented
        // (GET /requests/{id} never returned request.md's content to
        // parse), so this is that section's first real implementation.
        if (request.approvedBy.isNotEmpty || request.rejections.isNotEmpty)
          _Section(
            title: 'Audit',
            children: [
              if (request.approvedBy.isNotEmpty)
                _TextField(
                  'Approved',
                  'Approved by ${request.approvedBy} at '
                      '${formatLocalTimestamp(request.approvedAt)}',
                ),
              if (request.rejections.isNotEmpty)
                _RejectionHistory(rejections: request.rejections),
            ],
          ),
        if (request.oracleSkipWarning.isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(bottom: 8),
            child: Text(
              'Warning: ${request.oracleSkipWarning}'
              '${request.oracleDraftDetail.isEmpty ? '' : ' (${request.oracleDraftDetail})'}',
              key: const ValueKey('oracle-skip-warning'),
              style: TextStyle(color: Theme.of(context).colorScheme.error),
            ),
          ),
        if (request.state == 'oracle_drafting')
          _Section(
            title: 'Oracle drafting',
            children: [OracleDraftingSection(request: request)],
          ),
        if (isReviewState)
          _Section(
            title: 'Review',
            children: [
              if (!api.canWrite)
                _TextField(
                  'Note',
                  'This console cannot write here: the server has '
                      'writes disabled for this origin and no override '
                      'token is configured for this console build. Ask '
                      'the operator running `factoryd serve` to bind it '
                      'to loopback (or configure an override token) to '
                      'enable approve/reject from here.',
                ),
              if (api.canWrite && !widget.detailLoaded)
                _TextField(
                  'Note',
                  'Loading the full spec/plan content before enabling '
                      'approve/reject...',
                ),
              if (widget.actionError != null)
                _TextField(
                  'Error',
                  'Could not update request: ${escapeInvisible(_errorMessage(widget.actionError!))}',
                ),
              if (request.state == 'oracle_review')
                OracleReviewPanel(
                  key: ValueKey('oracle-review-${request.id}'),
                  api: api,
                  request: request,
                  canAct: !widget.acting && widget.detailLoaded,
                  onApprove: widget.onApprove,
                ),
              if (awaitingTicketOracle)
                TicketOraclePanel(
                  key: ValueKey(
                    'ticket-oracle-${request.id}-${request.updatedAt}-'
                    '${widget.ticketOracleNonce}',
                  ),
                  api: api,
                  request: request,
                  onChanged: widget.onTicketOracleChanged,
                ),
              Wrap(
                spacing: 8,
                children: [
                  if (request.state != 'oracle_review')
                    FilledButton.icon(
                      key: const ValueKey('approve-request-button'),
                      onPressed:
                          (widget.acting ||
                              !api.canWrite ||
                              !widget.detailLoaded ||
                              // disabled while an editor is dirty --
                              // an in-flight edit must be saved or
                              // cancelled before approving/rejecting the
                              // content it's editing.
                              _anyEditorOpen ||
                              (awaitingTicketOracle &&
                                  widget.ticketOracle?.complete != true))
                          ? null
                          : widget.onApprove,
                      icon: const Icon(Icons.check_circle_outline),
                      label: const Text('Approve'),
                    ),
                  OutlinedButton.icon(
                    key: const ValueKey('reject-request-button'),
                    onPressed:
                        (widget.acting ||
                            !api.canWrite ||
                            !widget.detailLoaded ||
                            _anyEditorOpen)
                        ? null
                        : widget.onReject,
                    icon: const Icon(Icons.cancel_outlined),
                    label: const Text('Request changes'),
                  ),
                ],
              ),
              if (_anyEditorOpen)
                const Padding(
                  padding: EdgeInsets.only(top: 4),
                  child: Text(
                    'Approve/Request changes are disabled while an edit is '
                    'open -- Save or Cancel it first.',
                    key: ValueKey('editor-dirty-note'),
                  ),
                ),
            ],
          ),
        // Revision compare: only offered once
        // there's something to compare against for the *current* stage's
        // document -- a request rejected once at spec_review still
        // showed "Compare with rejected revision" at plan_review, where no
        // ticket revision exists at all (SnapshotRevision only snapshots
        // the document that was actually rejected). A rejection whose own
        // fromState matches the current state is exactly the signal a
        // revision of *this* document exists, without an eager
        // listRevisions fetch just to decide whether to show the toggle.
        if (request.rejections.any((r) => r.stage == request.state))
          _Section(
            title: 'Revisions',
            children: [
              SwitchListTile(
                key: const ValueKey('compare-revision-toggle'),
                contentPadding: EdgeInsets.zero,
                title: const Text('Compare with rejected revision'),
                value: _compareEnabled,
                onChanged: _toggleCompare,
              ),
              if (_compareEnabled) ..._buildCompareBody(),
            ],
          ),
        ..._contentSections(request),
        if (request.tickets.isNotEmpty && !ticketsFirst) ticketsSection,
      ],
    );
  }

  List<Widget> _buildCompareBody() {
    if (_loadingRevisions) {
      return const [
        Padding(
          padding: EdgeInsets.symmetric(vertical: 8),
          child: LinearProgressIndicator(),
        ),
      ];
    }
    if (_revisionsError != null) {
      return [
        _TextField('Error', 'Could not load revisions: $_revisionsError'),
      ];
    }
    final revisions = _revisions ?? const [];
    if (revisions.isEmpty) {
      return const [Text('No rejected revisions recorded.')];
    }
    return [
      DropdownButton<int>(
        key: const ValueKey('revision-select'),
        value: _selectedRevisionIndex,
        hint: const Text('Select a revision'),
        items: [
          for (final revision in revisions)
            DropdownMenuItem(
              value: revision.index,
              child: Text(
                'Revision ${revision.index} — rejected by ${revision.by} '
                'at ${formatLocalTimestamp(revision.at)}',
              ),
            ),
        ],
        onChanged: (value) {
          if (value != null) _selectRevision(value);
        },
      ),
      if (_loadingRevisionDetail)
        const Padding(
          padding: EdgeInsets.symmetric(vertical: 8),
          child: LinearProgressIndicator(),
        ),
      if (_revisionDetailError != null)
        _TextField('Error', 'Could not load revision: $_revisionDetailError'),
      if (_revisionDetail case final detail?)
        SizedBox(
          key: const ValueKey('revision-diff'),
          height: 400,
          child: UnifiedDiffView(diff: _diffTextFor(detail)),
        ),
    ];
  }

  // _contentSections renders whichever file this request has to show:
  // each ticket's own plan file (once planning has produced tickets --
  // the plan_review state and every state after it) takes precedence
  // over the request-level spec.md, since spec.md stops being the
  // operative document once its tickets exist; falling back to spec.md
  // for spec_drafting/spec_review, before any ticket exists.
  //
  // Each section is editable in place (review-and-approve-in-place) only
  // while the request is in the review state that section's own PUT route
  // accepts -- spec_review for spec.md, plan_review for a ticket -- and
  // only with an override token configured, the same gate Approve/Reject
  // above already apply; an edit control that would just 403/409 on
  // every click is worse than no control at all.
  List<Widget> _contentSections(RequestSummary request) {
    final ticketsWithContent = request.tickets
        .where((t) => t.content.isNotEmpty)
        .toList();
    if (ticketsWithContent.isNotEmpty) {
      final editable = request.state == 'plan_review' && widget.api.canWrite;
      return [
        for (final ticket in ticketsWithContent)
          _FileContentSection(
            key: ValueKey('ticket-content-${ticket.index}'),
            title: 'Ticket ${ticket.index} plan',
            path: ticket.specPath,
            fullPath: ticket.fullPath,
            content: ticket.content,
            editable: editable,
            onSave: (content, baseSha256) =>
                _saveTicket(ticket.index, content, baseSha256),
            onEditingChanged: _onEditingChanged,
            onFetchCurrent: () => _fetchCurrentTicket(ticket.index),
          ),
      ];
    }
    if (request.spec.isNotEmpty) {
      return [
        _FileContentSection(
          key: const ValueKey('spec-content'),
          title: 'Spec',
          path: 'spec.md',
          fullPath: request.specFullPath,
          content: request.spec,
          editable: request.state == 'spec_review' && widget.api.canWrite,
          onSave: _saveSpec,
          onEditingChanged: _onEditingChanged,
          onFetchCurrent: _fetchCurrentSpec,
        ),
        // Parsed acceptance criteria: a numbered list straight from
        // the current spec text, with a count, so an operator can check
        // "does the plan still cover every criterion" without hunting
        // through the raw markdown above.
        _AcceptanceCriteriaList(spec: request.spec),
      ];
    }
    return const [];
  }

  // _saveSpec/_saveTicket call the new PUT routes and, on success, feed
  // the response straight back to the parent screen -- PUT
  // /requests/{id}/spec|tickets/{n} returns the same enriched
  // requestDetailView shape GET /requests/{id} does (see internal/api's
  // buildRequestDetailView), so this refreshes the whole screen with the
  // saved content exactly the way _approve's own response already does,
  // without a second round trip. Any error (wrong state, 422 validation,
  // or a 409 conflict -- see [RunApiException.currentSha256]) propagates
  // to the caller (_FileContentSectionState's Save handler), which
  // renders it inline rather than leaving the edit silently lost.
  Future<void> _saveSpec(String content, String baseSha256) async {
    final updated = await widget.api.updateRequestSpec(
      widget.request.id,
      content,
      baseSha256: baseSha256,
    );
    widget.onRequestUpdated(updated);
  }

  Future<void> _saveTicket(int index, String content, String baseSha256) async {
    final updated = await widget.api.updateRequestTicket(
      widget.request.id,
      index,
      content,
      baseSha256: baseSha256,
    );
    widget.onRequestUpdated(updated);
  }

  // _fetchCurrentSpec/_fetchCurrentTicket (409 conflict resolution) --
  // called from the conflict callout to fetch what's actually on disk
  // right now, so the operator can see a real diff instead of only a
  // hash. Both also feed the fresh RequestSummary back to the parent
  // screen exactly like a successful save does, so the rest of the
  // screen (title, other fields, the pipeline stepper) picks up the
  // same fetch rather than looking stale next to a just-reloaded editor.
  Future<String> _fetchCurrentSpec() async {
    final updated = await widget.api.getRequest(widget.request.id);
    widget.onRequestUpdated(updated);
    return updated.spec;
  }

  Future<String> _fetchCurrentTicket(int index) async {
    final updated = await widget.api.getRequest(widget.request.id);
    widget.onRequestUpdated(updated);
    for (final ticket in updated.tickets) {
      if (ticket.index == index) return ticket.content;
    }
    // The ticket is gone from the fresh fetch (renumbered plan, say) --
    // there is nothing left to diff against; the conflict callout falls
    // back to showing just the hash in that case.
    return '';
  }

  // _openEditors: how many _FileContentSection editors are currently
  // open across this screen -- Approve/Request changes/Retry stay
  // disabled while it's nonzero ("disable while the editor is dirty").
  // Only ever 0 or 1 in practice (a request shows at most one spec/ticket
  // content section at a time), but summed rather than hard-coded to a
  // bool so a future screen showing several at once stays correct.
  int _openEditors = 0;

  bool get _anyEditorOpen => _openEditors > 0;

  void _onEditingChanged(bool editing) {
    setState(() => _openEditors += editing ? 1 : -1);
  }
}

/// The rejection-history expandable: every structured Rejection this
/// request has recorded, display-only.
/// [Rejection.fromState] is shown for operator context and never used to
/// route anything -- the same guardrail the Go field it mirrors carries.
class _RejectionHistory extends StatelessWidget {
  const _RejectionHistory({required this.rejections});

  final List<Rejection> rejections;

  @override
  Widget build(BuildContext context) {
    return ExpansionTile(
      key: const ValueKey('rejection-history'),
      tilePadding: EdgeInsets.zero,
      title: Text('Rejection history (${rejections.length})'),
      children: [
        for (final rejection in rejections)
          Padding(
            padding: const EdgeInsets.only(bottom: 8),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  rejection.forStage == null
                      ? 'Rejected by ${rejection.by} at '
                            '${formatLocalTimestamp(rejection.at)} '
                            '(from ${rejection.fromState})'
                      : 'Sent back by ${rejection.by} at '
                            '${formatLocalTimestamp(rejection.at)} '
                            '(from ${rejection.fromState}, for ${rejection.forStage})',
                  style: Theme.of(context).textTheme.bodyMedium,
                ),
                SelectableText(rejection.reason),
              ],
            ),
          ),
      ],
    );
  }
}

/// What a failed resume answered: the generation and refusal reasons it was
/// made against. A poll that changes either clears the shown error.
String resumeStateKey(RequestSummary request) =>
    '${request.resume?.generation}|${request.resume?.refused.join('\n')}';

/// The resume_review callout: the step a lost worker left behind, why, and
/// the three decisions (Resume from the last round, Rebuild from scratch,
/// Cancel). Nothing reruns until the operator picks one. While the server
/// recorded refusal reasons, Resume is hidden: only the rebuild or cancel
/// are possible.
class _ResumeCallout extends StatelessWidget {
  const _ResumeCallout({
    required this.api,
    required this.request,
    required this.acting,
    required this.actionError,
    required this.resumeError,
    required this.onResume,
    required this.onCancel,
  });

  final RunApi api;
  final RequestSummary request;
  final bool acting;
  // The last failed action's error: a 409 carries the refusal reasons, a
  // 503 is a retryable failure to check.
  // actionError is the shared action error: in resume_review only Cancel can
  // leave one. resumeError is a failed Resume / Rebuild / Rerun.
  final Object? actionError;
  final Object? resumeError;
  final void Function(String from) onResume;
  final VoidCallback onCancel;

  @override
  Widget build(BuildContext context) {
    final colors = Theme.of(context).colorScheme;
    final onCard = colors.onTertiaryContainer;
    final lost = request.resume?.fromState ?? '';
    // An unknown lost step is offered both decisions.
    final isBuild = lost.isEmpty || lost == 'building';
    final refused = request.resume?.refused ?? const <String>[];
    final canAct = !acting && api.canWrite;
    final prompt = request.error.isNotEmpty
        ? request.error
        : request.nextAction;
    return Padding(
      padding: const EdgeInsets.only(bottom: 20),
      child: Card(
        key: const ValueKey('resume-callout'),
        color: colors.tertiaryContainer,
        child: Padding(
          padding: const EdgeInsets.all(12),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                lost.isEmpty
                    ? 'A step was lost when the worker stopped.'
                    : 'The ${stateLabel(lost)} step was lost when the worker '
                          'stopped.',
                style: TextStyle(fontWeight: FontWeight.w600, color: onCard),
              ),
              if (prompt.isNotEmpty) ...[
                const SizedBox(height: 4),
                Text(prompt, style: TextStyle(color: onCard)),
              ],
              if (refused.isNotEmpty) ...[
                const SizedBox(height: 8),
                Text(
                  'Resume is not possible:',
                  style: TextStyle(fontWeight: FontWeight.w600, color: onCard),
                ),
                for (final (i, reason) in refused.indexed)
                  Text(
                    '- $reason',
                    key: ValueKey('resume-refusal-reason-$i'),
                    style: TextStyle(color: onCard),
                  ),
              ],
              for (final (key, label, error) in [
                ('resume-action-error', 'Could not resume', resumeError),
                ('resume-cancel-error', 'Could not cancel', actionError),
              ])
                if (error != null) ...[
                  const SizedBox(height: 8),
                  Text(
                    '$label: ${escapeInvisible(_errorMessage(error))}',
                    key: ValueKey(key),
                    style: TextStyle(
                      fontWeight: FontWeight.w600,
                      color: colors.error,
                    ),
                  ),
                ],
              const SizedBox(height: 8),
              Wrap(
                spacing: 8,
                runSpacing: 8,
                children: [
                  // Round and scratch are the same for a drafting or planning
                  // step: one button reruns it.
                  if (!isBuild)
                    FilledButton.icon(
                      key: const ValueKey('resume-request-button'),
                      onPressed: canAct ? () => onResume('round') : null,
                      icon: const Icon(Icons.replay),
                      label: const Text('Rerun step'),
                    )
                  else ...[
                    if (refused.isEmpty)
                      FilledButton.icon(
                        key: const ValueKey('resume-request-button'),
                        onPressed: canAct ? () => onResume('round') : null,
                        icon: const Icon(Icons.play_arrow),
                        label: const Text('Resume'),
                      ),
                    FilledButton.tonalIcon(
                      key: const ValueKey('resume-scratch-button'),
                      onPressed: canAct ? () => onResume('scratch') : null,
                      icon: const Icon(Icons.replay),
                      label: const Text('Rebuild from scratch'),
                    ),
                  ],
                  OutlinedButton.icon(
                    key: const ValueKey('resume-cancel-button'),
                    onPressed: canAct ? onCancel : null,
                    icon: const Icon(Icons.block),
                    label: const Text('Cancel'),
                  ),
                ],
              ),
            ],
          ),
        ),
      ),
    );
  }
}

/// The halted/quarantined recovery callout: a kind-specific
/// explanation (this request's own [RequestSummary.nextAction] when the
/// server supplies one, falling back to a generic explanation), Retry and
/// Cancel actions (console routes, not CLI-only anymore), the copyable CLI
/// equivalent for an operator who prefers it, and -- quarantined only --
/// a link to the quarantined ticket's own run-override form. Never shows
/// "waiting on you" with nothing to do: Retry/Cancel are always offered
/// here regardless of [RunApi.canWrite] (disabled, not hidden, when
/// writes aren't available -- the operator still sees what the recovery
/// actions *are*).
class _RecoveryCallout extends StatelessWidget {
  const _RecoveryCallout({
    required this.api,
    required this.request,
    required this.acting,
    required this.onRetry,
    required this.onCancel,
    required this.onSendBack,
  });

  final RunApi api;
  final RequestSummary request;
  final bool acting;
  final VoidCallback onRetry;
  final VoidCallback onCancel;
  // Send back to planning/spec drafting -- offered only when
  // request.canSendBack says the server would actually accept it (no
  // ticket accepted yet, and not a HaltOracleMaterialize halt, which has
  // its own narrower recovery).
  final VoidCallback onSendBack;

  bool get _quarantined => request.state == 'quarantined';

  // Prefers the ticket at the request's own current TicketIndex (the one
  // whose build actually triggered the quarantine); falls back to the
  // first ticket with a run at all, for a shape this view can't otherwise
  // resolve precisely without a per-ticket run fetch.
  RequestTicket? get _ticket {
    for (final ticket in request.tickets) {
      if (ticket.index == request.ticketIndex && ticket.runId.isNotEmpty) {
        return ticket;
      }
    }
    for (final ticket in request.tickets) {
      if (ticket.runId.isNotEmpty) return ticket;
    }
    return null;
  }

  @override
  Widget build(BuildContext context) {
    final ticket = _quarantined ? _ticket : null;
    final colors = Theme.of(context).colorScheme;
    // accepted · awaiting PR is `halted` underneath, but nothing failed:
    // the build is accepted and only the pull request is missing, so it
    // gets a neutral card and a retry label that says it rebuilds (a
    // retry from here is a fresh, paid run -- request.go's retry path).
    final awaitingPR = request.awaitingPullRequest;
    final headline = _quarantined
        ? 'This request is quarantined.'
        : awaitingPR
        ? 'Built and verified; no pull request was opened.'
        : 'This request is halted.';
    final explanation = request.nextAction.isNotEmpty
        ? request.nextAction
        : 'Retry the request to move it forward again, or cancel it to '
              'abandon it.';
    final onCard = awaitingPR
        ? colors.onSecondaryContainer
        : colors.onErrorContainer;
    // A spec_conformity quarantine's usual recovery is sending it back to
    // spec (the criterion itself was wrong or unsatisfiable), so that
    // action leads, ahead of the generic Retry, and targets spec even when
    // planning would also be allowed. Otherwise the target is the one
    // canSendBackToPlan allows (C8 follow-up, operator demo, 2026-09-26).
    final specConformity = request.quarantineCheck == 'spec_conformity';
    final sendBackTarget = (specConformity || !request.canSendBackToPlan)
        ? 'spec'
        : 'plan';
    final sendBackLabel = sendBackTarget == 'plan'
        ? 'Send back to planning'
        : 'Send back to spec';
    final sendBackIsPrimary = request.canSendBack && specConformity;
    // The CLI line mirrors whichever action leads, so copying it never runs
    // a different recovery than the page suggests.
    final cliEquivalent = sendBackIsPrimary
        ? 'factoryd reject -to $sendBackTarget -reason "<what to change>" ${request.id}'
        : 'factoryd retry ${request.id}';
    return Padding(
      padding: const EdgeInsets.only(bottom: 20),
      child: Card(
        key: const ValueKey('recovery-callout'),
        color: awaitingPR ? colors.secondaryContainer : colors.errorContainer,
        child: Padding(
          padding: const EdgeInsets.all(12),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                headline,
                style: TextStyle(fontWeight: FontWeight.w600, color: onCard),
              ),
              const SizedBox(height: 4),
              Text(explanation, style: TextStyle(color: onCard)),
              const SizedBox(height: 8),
              Wrap(
                spacing: 8,
                runSpacing: 8,
                crossAxisAlignment: WrapCrossAlignment.center,
                children: [
                  // Send back: offered only when the server says
                  // request.SendBack would actually accept it right now --
                  // no ticket accepted yet, and not a HaltOracleMaterialize
                  // halt (its own narrower `factoryd retry` already covers
                  // that). Hidden, not merely disabled, when
                  // request.canSendBack is false: unlike Retry/Cancel
                  // (always legal from here), a false canSendBack means
                  // this action is not available at all right now, not
                  // just gated on write access. Leads, as the FilledButton,
                  // when a spec_conformity quarantine makes it the only
                  // sensible recovery (sendBackIsPrimary above).
                  if (request.canSendBack && sendBackIsPrimary)
                    FilledButton.icon(
                      key: const ValueKey('send-back-request-button'),
                      onPressed: (acting || !api.canWrite) ? null : onSendBack,
                      icon: const Icon(Icons.undo),
                      label: Text(sendBackLabel),
                    ),
                  FilledButton.icon(
                    key: const ValueKey('retry-request-button'),
                    onPressed: (acting || !api.canWrite) ? null : onRetry,
                    icon: const Icon(Icons.replay),
                    label: Text(
                      awaitingPR ? 'Retry request (rebuilds)' : 'Retry request',
                    ),
                  ),
                  OutlinedButton.icon(
                    key: const ValueKey('cancel-request-button'),
                    onPressed: (acting || !api.canWrite) ? null : onCancel,
                    icon: const Icon(Icons.block),
                    label: const Text('Cancel request'),
                  ),
                  if (request.canSendBack && !sendBackIsPrimary)
                    OutlinedButton.icon(
                      key: const ValueKey('send-back-request-button'),
                      onPressed: (acting || !api.canWrite) ? null : onSendBack,
                      icon: const Icon(Icons.undo),
                      label: Text(sendBackLabel),
                    ),
                  Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      Flexible(
                        child: Text(
                          cliEquivalent,
                          style: TextStyle(
                            fontFamily: 'monospace',
                            color: onCard,
                          ),
                        ),
                      ),
                      IconButton(
                        key: const ValueKey('copy-recovery-cli'),
                        icon: const Icon(Icons.copy, size: 16),
                        tooltip: 'Copy CLI equivalent',
                        onPressed: () {
                          Clipboard.setData(ClipboardData(text: cliEquivalent));
                          ScaffoldMessenger.of(context).showSnackBar(
                            const SnackBar(content: Text('Command copied')),
                          );
                        },
                      ),
                    ],
                  ),
                ],
              ),
              // Overriding the run is a separate, optional action from
              // retry above -- found in review (twice): an earlier version
              // of this callout implied overriding was a prerequisite for
              // retry, but cmd/factoryd/retryRequest never inspects or
              // requires the ticket's own run record; a drafting/planning
              // quarantine has no ticket run to override in the first
              // place.
              if (ticket != null) ...[
                const SizedBox(height: 8),
                Text(
                  "Correcting the ticket's own run record (accepted vs. "
                  'halted) is a separate, optional action -- it does not '
                  'affect whether Retry above will work.',
                  style: TextStyle(color: onCard),
                ),
                const SizedBox(height: 8),
                OutlinedButton.icon(
                  key: const ValueKey('quarantined-override-link'),
                  onPressed: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => RunDetailScreen(
                        api: api,
                        runId: ticket.runId,
                        initialOverrideReason:
                            'Request ${request.id} ticket ${ticket.index} '
                            'quarantined',
                      ),
                    ),
                  ),
                  icon: const Icon(Icons.admin_panel_settings_outlined),
                  label: const Text("Review the ticket's run override"),
                ),
              ],
            ],
          ),
        ),
      ),
    );
  }
}

/// The "Next" banner: the single most prominent line on this
/// screen, showing the one thing to do next.
class _NextActionBanner extends StatelessWidget {
  const _NextActionBanner({required this.text});

  final String text;

  @override
  Widget build(BuildContext context) {
    final colors = Theme.of(context).colorScheme;
    return Padding(
      padding: const EdgeInsets.only(bottom: 16),
      child: Container(
        key: const ValueKey('next-action-banner'),
        width: double.infinity,
        padding: const EdgeInsets.all(12),
        decoration: BoxDecoration(
          color: colors.primaryContainer,
          borderRadius: BorderRadius.circular(4),
        ),
        child: Row(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Icon(Icons.arrow_forward, color: colors.onPrimaryContainer),
            const SizedBox(width: 8),
            Expanded(
              child: SelectableText(
                text,
                style: TextStyle(
                  color: colors.onPrimaryContainer,
                  fontWeight: FontWeight.w600,
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}

/// One ticket's own card in the request detail screen's "Tickets" section.
///
/// Phase 4 (follow-along console): the server's ticket JSON carries no
/// `run_state` field (see ticketStatus's own doc comment in
/// ticket_rollup.dart for the confirmation this is checked against, and
/// why), so this screen -- unlike the board list, which never does a
/// per-ticket fetch -- fetches this one ticket's own run with a plain
/// `GET /runs/{id}` when it has a runId, and shows that run's real
/// [StateBadge] instead of leaving the ticket's build status to the
/// board-wide roll-up's runId-implies-"building" approximation. A handful
/// of tickets on one open request is a cost this detail screen can afford;
/// a whole board of requests is not, which is why that approximation stays
/// for the list.
class _TicketLinks extends StatefulWidget {
  const _TicketLinks({required this.api, required this.ticket, super.key});

  final RunApi api;
  final RequestTicket ticket;

  @override
  State<_TicketLinks> createState() => _TicketLinksState();
}

class _TicketLinksState extends State<_TicketLinks> {
  Run? _run;
  Object? _runError;
  bool _loadingRun = false;

  @override
  void initState() {
    super.initState();
    if (widget.ticket.runId.isNotEmpty) _loadRun();
  }

  @override
  void didUpdateWidget(_TicketLinks oldWidget) {
    super.didUpdateWidget(oldWidget);
    // A live update (or a manual Refresh) can hand this same
    // ticket a *different* runId -- a retry landing a fresh run under the
    // same ticket index, most notably. Even with the stable per-index key
    // above (so this State object survives across rebuilds instead of
    // being discarded), the fetched Run itself must be re-fetched, or a
    // retried ticket would keep showing its previous run's now-stale
    // state indefinitely.
    if (oldWidget.ticket.runId != widget.ticket.runId) {
      _run = null;
      _runError = null;
      if (widget.ticket.runId.isNotEmpty) _loadRun();
    }
  }

  Future<void> _loadRun() async {
    setState(() => _loadingRun = true);
    try {
      final run = await widget.api.getRun(widget.ticket.runId);
      if (mounted) setState(() => _run = run);
    } on Object catch (error) {
      if (mounted) setState(() => _runError = error);
    } finally {
      if (mounted) setState(() => _loadingRun = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final ticket = widget.ticket;
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              'Ticket ${ticket.index}',
              style: Theme.of(context).textTheme.titleMedium,
            ),
            const SizedBox(height: 4),
            Wrap(
              spacing: 8,
              crossAxisAlignment: WrapCrossAlignment.center,
              children: [
                if (_run case final run?) ...[
                  StateBadge(state: run.state),
                  StallChip(run: run),
                ] else if (_loadingRun)
                  const SizedBox.square(
                    dimension: 16,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                else if (_runError != null)
                  Icon(
                    Icons.error_outline,
                    size: 16,
                    color: Theme.of(context).colorScheme.error,
                  ),
                if (ticket.prState.isNotEmpty && ticket.prUrl.isNotEmpty)
                  PrStateChip(prState: ticket.prState),
              ],
            ),
            if (ticket.prUrl.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 4),
                child: SelectableText('PR: ${ticket.prUrl}'),
              ),
            if (ticket.runId.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 4),
                child: OutlinedButton.icon(
                  key: ValueKey('ticket-run-link-${ticket.index}'),
                  onPressed: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) =>
                          RunDetailScreen(api: widget.api, runId: ticket.runId),
                    ),
                  ),
                  icon: const Icon(Icons.open_in_new),
                  label: const Text('View run'),
                ),
              ),
          ],
        ),
      ),
    );
  }
}

/// Renders one spec/plan file's content, with a raw/rendered toggle.
/// Raw (the pre-existing plain monospace box) by default -- this
/// screen's own established view, and every existing
/// caller/test's expectation, both unchanged unless the operator opts
/// into "Rendered" -- never applied to a diff (diff_screen.dart's
/// UnifiedDiffView, used unchanged by this screen's own revision-compare
/// view above, always stays raw text regardless of this toggle).
// _parseAcceptanceCriteria: the numbered items under a "## Acceptance
// criteria" heading in [spec] (spec.md's own convention -- see
// ValidateSpecSkeleton on the Go side for the structure this mirrors),
// stopping at the next "## " heading or end of text. Tolerant: an absent
// heading, or one with no numbered items under it, returns an empty list
// rather than throwing -- this is a display aid, never a validator.
List<String> _parseAcceptanceCriteria(String spec) {
  final lines = spec.split('\n');
  var inSection = false;
  final items = <String>[];
  final numbered = RegExp(r'^\s*\d+[.)]\s+(.*)$');
  for (final line in lines) {
    final heading = line.trimRight();
    if (RegExp(r'^#{1,6}\s+', caseSensitive: false).hasMatch(heading)) {
      if (inSection) break;
      inSection = RegExp(
        r'^#{1,6}\s+acceptance criteria\s*$',
        caseSensitive: false,
      ).hasMatch(heading);
      continue;
    }
    if (!inSection) continue;
    final match = numbered.firstMatch(line);
    if (match != null) items.add(match.group(1)!.trim());
  }
  return items;
}

/// The parsed acceptance-criteria list, shown under the spec editor
/// with a count -- lets an operator check plan/oracle coverage against
/// the spec's own numbering without hunting through the raw markdown.
class _AcceptanceCriteriaList extends StatelessWidget {
  const _AcceptanceCriteriaList({required this.spec});

  final String spec;

  @override
  Widget build(BuildContext context) {
    final criteria = _parseAcceptanceCriteria(spec);
    if (criteria.isEmpty) return const SizedBox.shrink();
    final theme = Theme.of(context);
    return Padding(
      key: const ValueKey('acceptance-criteria-list'),
      padding: const EdgeInsets.only(top: 8, bottom: 8),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            'Acceptance criteria (${criteria.length})',
            style: theme.textTheme.titleSmall,
          ),
          for (var i = 0; i < criteria.length; i++)
            Padding(
              padding: const EdgeInsets.only(top: 2),
              child: Text('${i + 1}. ${criteria[i]}'),
            ),
        ],
      ),
    );
  }
}

class _FileContentSection extends StatefulWidget {
  const _FileContentSection({
    required this.title,
    required this.path,
    required this.content,
    this.fullPath = '',
    this.editable = false,
    this.onSave,
    this.onEditingChanged,
    this.onFetchCurrent,
    super.key,
  });

  final String title;
  final String path;
  final String content;
  // Home-relativized real path on the factoryd serve host (see
  // internal/api's homeRelativePath) -- empty on a server predating this
  // field, or if this process's own home directory couldn't be
  // determined server-side. Only correct when the console's viewer and
  // that host share a home directory, true for every deployment this
  // repo documents today (single-engineer, co-located); falls back to
  // just showing `path` (the request-relative fragment) when empty.
  final String fullPath;
  // Whether this section offers an Edit toggle at all (review-and-
  // approve-in-place) -- see _contentSections' own doc comment for when
  // each of spec/ticket is editable. False hides the Edit control
  // entirely rather than showing one that would just fail server-side.
  final bool editable;
  // Saves edited content via the matching PUT route
  // (_RequestSectionsState._saveSpec/_saveTicket) and, on success,
  // notifies the rest of the screen so it picks up the change -- see
  // those methods' own doc comment. Required when editable is true;
  // never called otherwise. [baseSha256] is the sha256 of the content
  // this editor was opened with -- the server refuses with 409 if
  // the file changed underneath since.
  final Future<void> Function(String content, String baseSha256)? onSave;

  // Reports every open/close of this section's own editor (disables
  // Approve/Request changes/Retry while the editor is dirty) -- the
  // parent screen aggregates this across every editable section to gate
  // its own write buttons. Never called for a non-editable section (it
  // never opens an editor at all).
  final ValueChanged<bool>? onEditingChanged;

  // Fetches this file's current content straight from the server
  // (409 conflict resolution) -- called only after a 409, to build a real
  // diff of "what's on disk now" vs. "what the operator has typed"
  // instead of only showing a hash. The implementation
  // (_RequestSectionsState._fetchCurrentSpec/_fetchCurrentTicket) also
  // feeds the fresh RequestSummary back to the rest of the screen, the
  // same way a successful save already does.
  final Future<String> Function()? onFetchCurrent;

  @override
  State<_FileContentSection> createState() => _FileContentSectionState();
}

class _FileContentSectionState extends State<_FileContentSection> {
  bool _raw = true;
  bool _editing = false;
  bool _saving = false;
  Object? _saveError;
  TextEditingController? _editController;
  // The hash of widget.content at the moment editing started -- sent
  // as base_sha256 so the server can detect a conflicting change made
  // elsewhere while this editor was open, rather than silently
  // overwriting it.
  String? _baseSha256;
  // The server's current content for this file, fetched only after a 409
  // -- null before a conflict, or while/if that fetch hasn't
  // resolved yet. Diffed against the operator's own unsaved text in the
  // conflict callout below.
  String? _conflictCurrentText;
  bool _loadingConflict = false;
  Object? _conflictFetchError;

  @override
  void dispose() {
    _editController?.dispose();
    super.dispose();
  }

  @override
  void didUpdateWidget(_FileContentSection oldWidget) {
    super.didUpdateWidget(oldWidget);
    // Close the editor the moment this section stops being editable --
    // e.g. the
    // request moved on to the next state (an approval landed, a live
    // update arrived) while an edit was open. Report the closure so the
    // parent's dirty count stays correct.
    if (_editing && !widget.editable) {
      _editing = false;
      _saveError = null;
      widget.onEditingChanged?.call(false);
    }
  }

  void _startEdit() {
    setState(() {
      _editController = TextEditingController(text: widget.content);
      _baseSha256 = sha256Hex(widget.content);
      _saveError = null;
      _editing = true;
    });
    widget.onEditingChanged?.call(true);
  }

  void _cancelEdit() {
    setState(() {
      _editing = false;
      _saveError = null;
    });
    widget.onEditingChanged?.call(false);
  }

  Future<void> _save() async {
    final onSave = widget.onSave;
    final controller = _editController;
    final baseSha256 = _baseSha256;
    if (onSave == null || controller == null || baseSha256 == null) return;
    setState(() {
      _saving = true;
      _saveError = null;
    });
    try {
      await onSave(controller.text, baseSha256);
      if (mounted) {
        setState(() => _editing = false);
        widget.onEditingChanged?.call(false);
      }
    } on Object catch (error) {
      // Keep the operator's text on a conflict -- _editing stays true,
      // the controller is untouched.
      if (mounted) setState(() => _saveError = error);
      if (error is RunApiException && error.statusCode == 409) {
        // Fetch what's actually on disk now, to diff against the
        // operator's own unsaved text rather than showing only a hash.
        unawaited(_loadConflictDiff());
      }
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  Future<void> _loadConflictDiff() async {
    final onFetchCurrent = widget.onFetchCurrent;
    if (onFetchCurrent == null) return;
    setState(() {
      _loadingConflict = true;
      _conflictFetchError = null;
    });
    try {
      final text = await onFetchCurrent();
      if (mounted) setState(() => _conflictCurrentText = text);
    } on Object catch (error) {
      if (mounted) setState(() => _conflictFetchError = error);
    } finally {
      if (mounted) setState(() => _loadingConflict = false);
    }
  }

  // "Discard mine and reload": drops the operator's unsaved edit and
  // returns to the (by now already-refreshed, via onFetchCurrent's own
  // onRequestUpdated call) read view.
  void _discardConflictAndReload() {
    setState(() {
      _editing = false;
      _saveError = null;
      _conflictCurrentText = null;
      _conflictFetchError = null;
    });
    widget.onEditingChanged?.call(false);
  }

  // "Keep editing (base = current)": the operator has now seen the
  // current server content in the diff below and chooses to keep their
  // own text anyway -- re-bases base_sha256 to the hash the 409 itself
  // reported, so the next Save is an explicit, informed overwrite of
  // what they just saw, not a blind retry of the same stale base.
  void _keepEditingRebased() {
    final error = _saveError;
    final newBase = error is RunApiException ? error.currentSha256 : null;
    if (newBase == null) return;
    setState(() {
      _baseSha256 = newBase;
      _saveError = null;
      _conflictCurrentText = null;
      _conflictFetchError = null;
    });
  }

  @override
  Widget build(BuildContext context) {
    return _Section(
      title: widget.title,
      children: [
        Row(
          children: [
            Expanded(
              child: Text(
                widget.fullPath.isNotEmpty ? widget.fullPath : widget.path,
                style: Theme.of(context).textTheme.bodySmall,
                overflow: TextOverflow.ellipsis,
              ),
            ),
            if (widget.fullPath.isNotEmpty)
              IconButton(
                key: const ValueKey('copy-full-path'),
                icon: const Icon(Icons.copy, size: 16),
                tooltip: 'Copy path to edit this file',
                onPressed: () {
                  Clipboard.setData(ClipboardData(text: widget.fullPath));
                  ScaffoldMessenger.of(
                    context,
                  ).showSnackBar(const SnackBar(content: Text('Path copied')));
                },
              ),
            if (widget.editable && !_editing)
              TextButton.icon(
                key: ValueKey('edit-content-button-${widget.path}'),
                onPressed: _startEdit,
                icon: const Icon(Icons.edit_outlined, size: 16),
                label: const Text('Edit'),
              ),
            if (!_editing) ...[
              const Text('Raw'),
              Switch(
                key: const ValueKey('markdown-raw-toggle'),
                value: _raw,
                onChanged: (value) => setState(() => _raw = value),
              ),
            ],
          ],
        ),
        const SizedBox(height: 4),
        if (_editing)
          ..._buildEditor()
        else if (_raw)
          Container(
            key: const ValueKey('markdown-raw-content'),
            width: double.infinity,
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              border: Border.all(color: Theme.of(context).dividerColor),
              borderRadius: BorderRadius.circular(4),
            ),
            child: SelectableText(
              widget.content,
              style: const TextStyle(fontFamily: 'monospace'),
            ),
          )
        else
          Container(
            key: const ValueKey('markdown-rendered-content'),
            width: double.infinity,
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              border: Border.all(color: Theme.of(context).dividerColor),
              borderRadius: BorderRadius.circular(4),
            ),
            child: MarkdownView(data: widget.content),
          ),
      ],
    );
  }

  // _buildEditor is the edit-in-place surface (review-and-approve-in-
  // place): a monospace multiline TextField pre-filled with the current
  // content, Save (calls widget.onSave, showing a 422's message inline on
  // failure) and Cancel below it.
  List<Widget> _buildEditor() {
    return [
      TextField(
        key: ValueKey('edit-content-field-${widget.path}'),
        controller: _editController,
        maxLines: null,
        minLines: 12,
        style: const TextStyle(fontFamily: 'monospace'),
        decoration: const InputDecoration(border: OutlineInputBorder()),
      ),
      if (_saveError case final RunApiException error
          when error.statusCode == 409)
        // The conflict shape: someone else changed this file while
        // the editor was open. The operator's text above is untouched;
        // this fetches and diffs what's actually on disk now, then lets
        // them either discard their own edit or keep it, explicitly
        // re-based on the current content they just saw.
        Padding(
          padding: const EdgeInsets.only(top: 4),
          child: Container(
            key: ValueKey('edit-content-conflict-${widget.path}'),
            width: double.infinity,
            padding: const EdgeInsets.all(8),
            decoration: BoxDecoration(
              color: Theme.of(context).colorScheme.errorContainer,
              borderRadius: BorderRadius.circular(4),
            ),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  'This file changed on disk since you started editing '
                  '(current hash: ${error.currentSha256 ?? 'unknown'}). '
                  'Your edit above is unchanged.',
                  style: TextStyle(
                    color: Theme.of(context).colorScheme.onErrorContainer,
                  ),
                ),
                if (_loadingConflict)
                  const Padding(
                    padding: EdgeInsets.only(top: 8),
                    child: LinearProgressIndicator(),
                  ),
                if (_conflictFetchError != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 8),
                    child: Text(
                      'Could not load the current content to diff: '
                      '${_errorMessage(_conflictFetchError!)}',
                      style: TextStyle(
                        color: Theme.of(context).colorScheme.onErrorContainer,
                      ),
                    ),
                  ),
                if (_conflictCurrentText case final currentText?)
                  Padding(
                    padding: const EdgeInsets.only(top: 8),
                    child: SizedBox(
                      key: ValueKey(
                        'edit-content-conflict-diff-${widget.path}',
                      ),
                      height: 240,
                      child: UnifiedDiffView(
                        diff:
                            '--- current (on disk)\n'
                            '+++ yours (unsaved)\n'
                            '${unifiedLineDiff(currentText, _editController?.text ?? '')}',
                      ),
                    ),
                  ),
                const SizedBox(height: 8),
                Wrap(
                  spacing: 8,
                  children: [
                    OutlinedButton(
                      key: ValueKey(
                        'edit-content-conflict-discard-${widget.path}',
                      ),
                      onPressed: _discardConflictAndReload,
                      child: const Text('Discard mine and reload'),
                    ),
                    FilledButton(
                      key: ValueKey(
                        'edit-content-conflict-keep-${widget.path}',
                      ),
                      onPressed: error.currentSha256 == null
                          ? null
                          : _keepEditingRebased,
                      child: const Text('Keep editing (base = current)'),
                    ),
                  ],
                ),
              ],
            ),
          ),
        )
      else if (_saveError != null)
        Padding(
          padding: const EdgeInsets.only(top: 4),
          child: Text(
            'Could not save: ${_errorMessage(_saveError!)}',
            key: ValueKey('edit-content-error-${widget.path}'),
            style: TextStyle(color: Theme.of(context).colorScheme.error),
          ),
        ),
      const SizedBox(height: 8),
      Row(
        mainAxisAlignment: MainAxisAlignment.end,
        children: [
          TextButton(
            key: ValueKey('cancel-content-button-${widget.path}'),
            onPressed: _saving ? null : _cancelEdit,
            child: const Text('Cancel'),
          ),
          const SizedBox(width: 8),
          FilledButton(
            key: ValueKey('save-content-button-${widget.path}'),
            onPressed: _saving ? null : _save,
            child: Text(_saving ? 'Saving...' : 'Save'),
          ),
        ],
      ),
    ];
  }
}

// _errorMessage extracts the server's own error text from a caught
// RunApiException (see that class's own `message` getter), instead of its
// toString's raw "Run API request failed (422): {...}" JSON envelope --
// the same reasoning error_display.dart's describeError applies, kept
// local here since this inline save error has no room for that file's
// full headline/next-step/details treatment.
String _errorMessage(Object error) => switch (error) {
  // A 503 means the server could not check, not that it refused.
  RunApiException(isRetryable: true) =>
    '${error.message} (temporary: try again)',
  RunApiException() => error.message,
  _ => error.toString(),
};

// The reject reason dialog moved to approve_reject.dart's
// RejectReasonDialog, shared with triage_screen.dart's own reject flow
// -- see runRejectFlow's doc comment.

class _Section extends StatelessWidget {
  const _Section({required this.title, required this.children});

  final String title;
  final List<Widget> children;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 20),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(title, style: Theme.of(context).textTheme.titleLarge),
          const SizedBox(height: 8),
          ...children,
        ],
      ),
    );
  }
}

class _Field extends StatelessWidget {
  const _Field(this.label, this.value);

  final String label;
  final Widget value;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          SizedBox(width: 140, child: Text(label)),
          Expanded(child: value),
        ],
      ),
    );
  }
}

class _TextField extends _Field {
  _TextField(String label, String value) : super(label, SelectableText(value));
}

/// The canonical, linear pipeline every request moves through -- the
/// terminal states (quarantined/halted/cancelled) are deliberately not
/// steps of their own here; a terminal request instead marks whichever
/// step it was on when it stopped (see _PipelineStepper's own doc
/// comment) as failed.
const _pipelineSteps = [
  'submitted',
  'spec_drafting',
  'spec_review',
  'oracle_drafting',
  'oracle_review',
  'planning',
  'plan_review',
  'building',
  'pr_review',
  'done',
];

/// The two opt-in oracle steps (`factoryd submit -draft-oracles`). A request
/// submitted without the flag never visits them, so the stepper hides them
/// entirely unless its own history (or current state) reached one (C1,
/// 2026-09-26: previously shown "skipped" once the request moved past
/// where they'd sit, which only added a step nobody could act on).
const _optionalOracleSteps = {'oracle_drafting', 'oracle_review'};

const _pipelineStepLabels = {
  'submitted': 'Submitted',
  'spec_drafting': 'Spec drafting',
  'spec_review': 'Spec review',
  'oracle_drafting': 'Oracle drafting',
  'oracle_review': 'Oracle review',
  'planning': 'Planning',
  'plan_review': 'Plan review',
  'building': 'Building',
  'pr_review': 'PR review',
  'done': 'Done',
};

enum _StepStatus { done, current, pending, failed, needsYou }

/// The operator's own ask: "where is my ask in the pipeline?" -- a single
/// horizontally-scrolling row over _pipelineSteps (C1, 2026-09-26: a
/// wrapping Wrap pushed "Done" onto a second line), each one showing a
/// glyph plus, underneath, the history entry that actually reached it
/// (when, and by whom/what) rather than a bare state word. Requires
/// request.history to know which state comes from which history entry;
/// a request predating that field (or one that has never left its
/// initial state) just falls back to `state`/`enteredAt` for the current
/// step, with every other step pending.
class _PipelineStepper extends StatelessWidget {
  const _PipelineStepper({required this.request});

  final RequestSummary request;

  @override
  Widget build(BuildContext context) {
    final terminal =
        request.state == 'quarantined' ||
        request.state == 'halted' ||
        request.state == 'cancelled';
    // For a terminal request, the step that actually failed is the one
    // the last history entry moved OUT of, not the terminal state
    // itself (which isn't a step in _pipelineSteps at all). A legacy or
    // just-created terminal request with no history has nothing to point
    // at -- every step then renders pending, which is honest: this
    // screen simply doesn't know which step it was on.
    // resume_review is not a step: the lost step is the one waiting on the
    // operator.
    final resumeReview = request.state == 'resume_review';
    final activeState = resumeReview
        ? (request.resume?.fromState ?? '')
        : terminal
        ? (request.history.isNotEmpty ? request.history.last.from : '')
        : request.state;
    // A -draft-oracles request shows its oracle steps from submission
    // on: the operator should see the stages still ahead of them.
    final visitedOracleStage =
        request.draftOracles ||
        _optionalOracleSteps.contains(request.state) ||
        request.history.any((e) => _optionalOracleSteps.contains(e.to));
    // The oracle steps are opt-in (`-draft-oracles`). A request that never
    // visited them (no -draft-oracles) hides them entirely, at every point
    // in its pipeline -- there is nothing for the operator to see there,
    // so a "skipped" step earned no place in a stepper an operator demo
    // found already wrapping (C1, 2026-09-26).
    final steps = [
      for (final step in _pipelineSteps)
        if (visitedOracleStage || !_optionalOracleSteps.contains(step)) step,
    ];
    final currentIndex = steps.indexOf(activeState);
    // The failed step's own detail is the terminal transition itself
    // (its at/by/reason -- the halt/quarantine error), not whatever
    // entry originally reached that step: that's the actually useful
    // thing to show under a glyph marked failed.
    final failureEntry = terminal && request.history.isNotEmpty
        ? request.history.last
        : null;

    // A single row, scrolling horizontally rather than wrapping: at
    // _pipelineSteps' full length (10 steps) a wrapping Wrap pushed
    // "Done" onto its own second line even at a normal desktop width
    // (C1, operator demo, 2026-09-26).
    return SingleChildScrollView(
      scrollDirection: Axis.horizontal,
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          for (var i = 0; i < steps.length; i++)
            _PipelineStepView(
              key: ValueKey('pipeline-step-${steps[i]}'),
              step: steps[i],
              status: i < currentIndex
                  ? _StepStatus.done
                  : i > currentIndex
                  ? _StepStatus.pending
                  : resumeReview
                  ? _StepStatus.needsYou
                  : !terminal
                  ? _StepStatus.current
                  : request.awaitingPullRequest
                  // Accepted, only the pull request is missing: waiting on
                  // the operator, not a failure.
                  ? _StepStatus.needsYou
                  : _StepStatus.failed,
              entry: (terminal && i == currentIndex)
                  ? failureEntry
                  : _lastEntryReaching(steps[i]),
              request: request,
            ),
        ],
      ),
    );
  }

  // The most recent history entry whose `to` reached step -- a request
  // can revisit an earlier step (a rejection sends spec_review back to
  // spec_drafting, say), so the latest visit is the one worth showing.
  RequestTransition? _lastEntryReaching(String step) {
    for (final entry in request.history.reversed) {
      if (entry.to == step) return entry;
    }
    return null;
  }
}

class _PipelineStepView extends StatelessWidget {
  const _PipelineStepView({
    required this.step,
    required this.status,
    required this.entry,
    required this.request,
    super.key,
  });

  final String step;
  final _StepStatus status;
  final RequestTransition? entry;
  final RequestSummary request;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final emphasize =
        status == _StepStatus.current ||
        status == _StepStatus.failed ||
        status == _StepStatus.needsYou;
    return SizedBox(
      width: 130,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              _glyph(theme),
              const SizedBox(width: 6),
              Expanded(
                child: Text(
                  _pipelineStepLabels[step] ?? step,
                  style: theme.textTheme.bodyMedium?.copyWith(
                    fontWeight: emphasize ? FontWeight.bold : FontWeight.normal,
                  ),
                ),
              ),
            ],
          ),
          const SizedBox(height: 2),
          Padding(
            padding: const EdgeInsets.only(left: 22),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: _detail(theme),
            ),
          ),
        ],
      ),
    );
  }

  Icon _glyph(ThemeData theme) {
    final colors = theme.colorScheme;
    switch (status) {
      case _StepStatus.done:
        return Icon(Icons.check_circle, size: 16, color: colors.primary);
      case _StepStatus.current:
        return Icon(
          Icons.radio_button_checked,
          size: 16,
          color: colors.primary,
        );
      case _StepStatus.failed:
        return Icon(Icons.error, size: 16, color: colors.error);
      case _StepStatus.needsYou:
        return Icon(Icons.pending_actions, size: 16, color: colors.tertiary);
      case _StepStatus.pending:
        return Icon(
          Icons.radio_button_unchecked,
          size: 16,
          color: colors.outline,
        );
    }
  }

  // Never a bare state word with nothing under it (the operator's own
  // requirement): the building step, while current, shows ticket
  // progress and elapsed time instead of (or in addition to) its own
  // history entry, since that's the more useful live signal; every other
  // step shows its reaching history entry's when/who/why, falling back
  // to enteredAt for a current/failed step with no history at all
  // (a legacy record).
  List<Widget> _detail(ThemeData theme) {
    final style = theme.textTheme.bodySmall;
    final subtleStyle = style?.copyWith(color: theme.colorScheme.outline);
    final widgets = <Widget>[];
    if (step == 'building' && status == _StepStatus.current) {
      final elapsed = formatDuration(elapsedBetween(request.enteredAt, null));
      widgets.add(
        Text(
          request.ticketCount > 0
              ? 'ticket ${request.ticketIndex}/${request.ticketCount} · $elapsed'
              : elapsed,
          style: style,
        ),
      );
      return widgets;
    }
    final e = entry;
    if (e == null) {
      if (status == _StepStatus.current ||
          status == _StepStatus.failed ||
          status == _StepStatus.needsYou) {
        widgets.add(Text(_formatWhen(request.enteredAt), style: style));
      }
      return widgets;
    }
    widgets.add(Text('${_formatWhen(e.at)} · ${e.by}', style: style));
    if (e.reason.isNotEmpty) {
      // Capped: a halt reason can run to a paragraph, and the full text is
      // already on the page (recovery callout / Detail row). Uncapped, it
      // pushed Tickets and "View run" off screen (console-walk, 2026-09-24).
      widgets.add(
        Tooltip(
          message: e.reason,
          child: Text(
            e.reason,
            style: subtleStyle,
            maxLines: 4,
            overflow: TextOverflow.ellipsis,
          ),
        ),
      );
    }
    return widgets;
  }
}

// _formatWhen renders an RFC3339 timestamp as a local "HH:mm" when it
// falls on today, or "MMM d HH:mm" otherwise -- the pipeline stepper's
// own compact per-step timestamp. Falls back to the raw value
// for an empty/unparseable timestamp rather than throwing.
String _formatWhen(String at) {
  final parsed = tryParseTimestamp(at);
  if (parsed == null) return at;
  final local = parsed.toLocal();
  final now = DateTime.now();
  String two(int n) => n.toString().padLeft(2, '0');
  final hm = '${two(local.hour)}:${two(local.minute)}';
  final sameDay =
      local.year == now.year &&
      local.month == now.month &&
      local.day == now.day;
  if (sameDay) return hm;
  const months = [
    'Jan',
    'Feb',
    'Mar',
    'Apr',
    'May',
    'Jun',
    'Jul',
    'Aug',
    'Sep',
    'Oct',
    'Nov',
    'Dec',
  ];
  return '${months[local.month - 1]} ${local.day} $hm';
}
