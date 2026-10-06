// The batch triage view: every needsHuman request
// in one keyboard-navigable list (j/k to move) with a detail pane on the
// right, a/r approving/rejecting the focused item through the exact same
// confirm flow request_detail_screen.dart's own Approve/Reject buttons
// use (approve_reject.dart). Deliberately no multi-select, no "approve
// all" -- a deliberate guardrail: the standing "bulk approve /
// approve-all" prohibition means batch *navigation* is fine, but batch
// *approval* would make the human gate a formality.
import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import 'api_client.dart';
import 'approve_reject.dart';
import 'error_display.dart';
import 'models.dart';
import 'request_cost.dart';
import 'request_list_screen.dart'
    show
        RequestStageChip,
        RequestStageGroup,
        requestStageGroupOf,
        sortedRequests,
        waitingBadgeLabel;
import 'ticket_rollup.dart';

class TriageScreen extends StatefulWidget {
  const TriageScreen({required this.api, this.onExit, super.key});

  final RunApi api;

  /// Returns to the request board. Null hides the back button (mostly
  /// for tests that reach this screen directly rather than through the
  /// router).
  final VoidCallback? onExit;

  @override
  State<TriageScreen> createState() => _TriageScreenState();
}

class _TriageScreenState extends State<TriageScreen> {
  List<RequestSummary>? _requests;
  Object? _error;
  int _focusedIndex = 0;
  bool _acting = false;
  Object? _actionError;
  final _keyboardFocus = FocusNode(debugLabel: 'triage-keyboard');

  // The board list (listRequests) never carries spec.md or ticket content
  // -- only GET /requests/{id} does. Found in review: without fetching
  // it, an operator could press Approve/Reject from /triage having seen
  // only metadata (title, state, cost, ticket roll-up), never the actual
  // spec or plan being decided on. _focusedDetail holds that fetch's
  // result for whichever request is currently focused; approve/reject
  // are gated on it having loaded for that exact id (see
  // _detailReadyForFocus) so the operator cannot act before it appears.
  RequestSummary? _focusedDetail;
  String? _focusedDetailId;
  Object? _detailError;

  @override
  void initState() {
    super.initState();
    _load();
  }

  @override
  void dispose() {
    _keyboardFocus.dispose();
    super.dispose();
  }

  bool get _detailReadyForFocus {
    final requests = _requests;
    if (requests == null || _focusedIndex >= requests.length) return false;
    return _focusedDetail != null &&
        _focusedDetailId == requests[_focusedIndex].id;
  }

  Future<void> _loadFocusedDetail({bool force = false}) async {
    final requests = _requests;
    if (requests == null || requests.isEmpty) return;
    final id = requests[_focusedIndex].id;
    // Already have it (e.g. re-entered after a keyboard move-and-back) --
    // unless force is set, which _load uses on every board refresh
    // (including the manual Refresh button). Found in review: without
    // this, editing spec.md/a ticket plan while the request stayed in
    // the same review state and hitting Refresh would reload the board
    // row but keep serving the OLD fetched content in the artifact pane,
    // letting an operator approve against content they never actually
    // saw.
    if (!force && _focusedDetailId == id && _focusedDetail != null) return;
    setState(() {
      _focusedDetail = null;
      _focusedDetailId = id;
      _detailError = null;
    });
    try {
      final detail = await widget.api.getRequest(id);
      if (mounted && _focusedDetailId == id) {
        setState(() => _focusedDetail = detail);
      }
    } on Object catch (error) {
      if (mounted && _focusedDetailId == id) {
        setState(() => _detailError = error);
      }
    }
  }

  Future<void> _load() async {
    try {
      final requests = await widget.api.listRequests();
      // Literal spec_review/plan_review, not requestStageGroup(...) ==
      // RequestStageGroup.review: that grouping also covers `halted` (a
      // board/needs-you-badge concern), but this screen's whole point is
      // the a/r approve/reject decision, which the server only accepts
      // from these two states -- listing a halted request here would
      // offer an action that just 500s (the same conflation found in
      // review on request_detail_screen.dart's own isReviewState).
      // oracle_review is deliberately excluded here (and from the approve
      // action below): approving it needs the request-detail oracle panel,
      // which shows every oracle file and sends the hashes it displayed --
      // a triage keystroke would approve blind.
      final needsHuman = sortedRequests(
        requests
            .where((r) => r.state == 'spec_review' || r.state == 'plan_review')
            .toList(),
      );
      if (mounted) {
        setState(() {
          _requests = needsHuman;
          _error = null;
          if (_focusedIndex >= needsHuman.length) {
            _focusedIndex = needsHuman.isEmpty ? 0 : needsHuman.length - 1;
          }
        });
        unawaited(_loadFocusedDetail(force: true));
      }
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    }
  }

  void _move(int delta) {
    final requests = _requests;
    if (requests == null || requests.isEmpty) return;
    setState(() {
      _focusedIndex = (_focusedIndex + delta).clamp(0, requests.length - 1);
    });
    unawaited(_loadFocusedDetail());
  }

  Future<void> _approveFocused() async {
    final requests = _requests;
    if (requests == null ||
        _focusedIndex >= requests.length ||
        _acting ||
        !widget.api.canWrite ||
        !_detailReadyForFocus) {
      return;
    }
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      await runApproveFlow(context, api: widget.api, request: _focusedDetail!);
      await _load();
    } on Object catch (error) {
      if (mounted) setState(() => _actionError = error);
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  Future<void> _rejectFocused() async {
    final requests = _requests;
    if (requests == null ||
        _focusedIndex >= requests.length ||
        _acting ||
        !widget.api.canWrite ||
        !_detailReadyForFocus) {
      return;
    }
    setState(() {
      _acting = true;
      _actionError = null;
    });
    try {
      await runRejectFlow(context, api: widget.api, request: _focusedDetail!);
      await _load();
    } on Object catch (error) {
      if (mounted) setState(() => _actionError = error);
    } finally {
      if (mounted) setState(() => _acting = false);
    }
  }

  KeyEventResult _handleKey(FocusNode node, KeyEvent event) {
    if (event is! KeyDownEvent) return KeyEventResult.ignored;
    switch (event.logicalKey) {
      case LogicalKeyboardKey.keyJ:
        _move(1);
        return KeyEventResult.handled;
      case LogicalKeyboardKey.keyK:
        _move(-1);
        return KeyEventResult.handled;
      case LogicalKeyboardKey.keyA:
        _approveFocused();
        return KeyEventResult.handled;
      case LogicalKeyboardKey.keyR:
        _rejectFocused();
        return KeyEventResult.handled;
      default:
        return KeyEventResult.ignored;
    }
  }

  @override
  Widget build(BuildContext context) {
    final requests = _requests;
    return Scaffold(
      appBar: AppBar(
        leading: widget.onExit == null
            ? null
            : IconButton(
                key: const ValueKey('triage-exit-button'),
                icon: const Icon(Icons.arrow_back),
                onPressed: widget.onExit,
              ),
        title: const Text('Triage'),
        actions: [
          IconButton(
            key: const ValueKey('triage-refresh-button'),
            onPressed: _load,
            tooltip: 'Refresh',
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: Focus(
        focusNode: _keyboardFocus,
        autofocus: true,
        onKeyEvent: _handleKey,
        child: switch ((requests, _error)) {
          (null, final Object error) => Center(
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: ErrorCallout(
                key: const ValueKey('triage-load-error'),
                error: error,
              ),
            ),
          ),
          (null, null) => const Center(child: CircularProgressIndicator()),
          (final List<RequestSummary> value, _) when value.isEmpty =>
            const Center(child: Text('Nothing needs you right now.')),
          (final List<RequestSummary> value, _) => Row(
            children: [
              SizedBox(
                width: 360,
                child: ListView.separated(
                  itemCount: value.length,
                  separatorBuilder: (_, _) => const Divider(height: 1),
                  itemBuilder: (context, index) {
                    final request = value[index];
                    return ListTile(
                      key: ValueKey('triage-row-${request.id}'),
                      selected: index == _focusedIndex,
                      onTap: () {
                        setState(() => _focusedIndex = index);
                        unawaited(_loadFocusedDetail());
                      },
                      title: Text(
                        request.title.isNotEmpty ? request.title : request.id,
                      ),
                      subtitle: Text(request.project),
                      trailing: RequestStageChip(
                        state: request.state,
                        awaitingPullRequest: request.awaitingPullRequest,
                        needsYou:
                            requestStageGroupOf(request) ==
                            RequestStageGroup.review,
                      ),
                    );
                  },
                ),
              ),
              const VerticalDivider(width: 1),
              Expanded(
                child: _TriageDetailPane(
                  request: value[_focusedIndex],
                  detail: _detailReadyForFocus ? _focusedDetail : null,
                  detailError: _focusedDetailId == value[_focusedIndex].id
                      ? _detailError
                      : null,
                  hasOverrideToken: widget.api.canWrite,
                  acting: _acting,
                  actionError: _actionError,
                  onApprove: _approveFocused,
                  onReject: _rejectFocused,
                ),
              ),
            ],
          ),
        },
      ),
    );
  }
}

class _TriageDetailPane extends StatelessWidget {
  const _TriageDetailPane({
    required this.request,
    required this.detail,
    required this.detailError,
    required this.hasOverrideToken,
    required this.acting,
    required this.actionError,
    required this.onApprove,
    required this.onReject,
  });

  final RequestSummary request;
  // The fetched GET /requests/{id} response for `request`, or null while
  // it's still loading (or failed) -- see TriageScreen's own
  // _loadFocusedDetail. Approve/Reject stay disabled until this is
  // non-null so an operator always sees the actual spec/plan content
  // before deciding, not just board metadata (found in review).
  final RequestSummary? detail;
  final Object? detailError;
  final bool hasOverrideToken;
  final bool acting;
  final Object? actionError;
  final VoidCallback onApprove;
  final VoidCallback onReject;

  // The review artifact to show: spec.md for spec_review, the ticket
  // plan(s) for plan_review -- selected by d.state, not by which field
  // happens to be non-empty. Found in review: spec.md is already
  // approved and non-empty by the time a request reaches plan_review, so
  // choosing on content presence always showed the (already-decided) old
  // spec and never the ticket plans actually under review at that state.
  String get _artifactContent {
    final d = detail;
    if (d == null) return '';
    if (d.state == 'plan_review') {
      final buffer = StringBuffer();
      for (final ticket in d.tickets) {
        if (ticket.content.isEmpty) continue;
        buffer.writeln('--- ticket ${ticket.index} ---');
        buffer.writeln(ticket.content);
        buffer.writeln();
      }
      return buffer.toString();
    }
    return d.spec;
  }

  @override
  Widget build(BuildContext context) {
    final badge = waitingBadgeLabel(request, DateTime.now());
    final cost = request.costSummary;
    return Padding(
      padding: const EdgeInsets.all(16),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            request.title.isNotEmpty ? request.title : request.id,
            style: Theme.of(context).textTheme.headlineSmall,
          ),
          const SizedBox(height: 8),
          Wrap(
            spacing: 8,
            crossAxisAlignment: WrapCrossAlignment.center,
            children: [
              RequestStageChip(
                state: request.state,
                awaitingPullRequest: request.awaitingPullRequest,
                needsYou:
                    requestStageGroupOf(request) == RequestStageGroup.review,
              ),
              if (badge != null)
                Chip(
                  label: Text(
                    badge,
                    style: const TextStyle(color: Colors.white),
                  ),
                  backgroundColor: Colors.deepOrange,
                ),
            ],
          ),
          const SizedBox(height: 8),
          Text('Project: ${request.project}'),
          if (request.ticketCount > 0)
            Text('Ticket ${request.ticketIndex} / ${request.ticketCount}'),
          if (cost == null)
            const Text('Usage so far: —')
          else
            for (final line in formatUsageLines(cost))
              Text('Usage so far: $line'),
          if (request.rejections.isNotEmpty)
            Text('Prior rejections: ${request.rejections.length}'),
          if (request.tickets.isNotEmpty)
            Padding(
              padding: const EdgeInsets.only(top: 8),
              child: TicketRollupStrip(request: request),
            ),
          const SizedBox(height: 16),
          // The actual spec/plan artifact under review -- fetched
          // separately from the board list, which never carries file
          // content. Approve/Reject below stay disabled until this has
          // loaded (found in review: this pane used to render only
          // metadata, letting an operator approve blind).
          const Text(
            'Under review:',
            style: TextStyle(fontWeight: FontWeight.bold),
          ),
          const SizedBox(height: 4),
          if (detailError != null)
            ErrorCallout(
              key: const ValueKey('triage-detail-error'),
              error: detailError!,
            )
          else if (detail == null)
            const Padding(
              padding: EdgeInsets.symmetric(vertical: 8),
              child: SizedBox(
                height: 16,
                width: 16,
                child: CircularProgressIndicator(strokeWidth: 2),
              ),
            )
          else
            Expanded(
              child: Container(
                key: const ValueKey('triage-artifact-content'),
                width: double.infinity,
                padding: const EdgeInsets.all(8),
                decoration: BoxDecoration(
                  color: Theme.of(context).colorScheme.surfaceContainerHighest,
                  border: Border.all(color: Theme.of(context).dividerColor),
                ),
                child: SingleChildScrollView(
                  child: SelectableText(
                    _artifactContent.isEmpty
                        ? '(no content yet)'
                        : _artifactContent,
                    style: const TextStyle(
                      fontFamily: 'monospace',
                      fontSize: 12,
                    ),
                  ),
                ),
              ),
            ),
          const SizedBox(height: 12),
          if (actionError != null)
            Padding(
              padding: const EdgeInsets.only(bottom: 8),
              child: ErrorCallout(
                key: const ValueKey('triage-action-error'),
                error: actionError!,
              ),
            ),
          // Approve/reject only ever appear with an override token
          // configured: a console without that token would just get a
          // 403 from the
          // server on every attempt, so this screen doesn't offer the
          // action at all rather than offering one doomed to fail. Both
          // also stay disabled until `detail` has loaded, for the same
          // reason noted above.
          if (hasOverrideToken)
            Wrap(
              spacing: 8,
              children: [
                FilledButton.icon(
                  key: const ValueKey('triage-approve-button'),
                  onPressed: (acting || detail == null) ? null : onApprove,
                  icon: const Icon(Icons.check_circle_outline),
                  label: const Text('Approve (a)'),
                ),
                OutlinedButton.icon(
                  key: const ValueKey('triage-reject-button'),
                  onPressed: (acting || detail == null) ? null : onReject,
                  icon: const Icon(Icons.cancel_outlined),
                  label: const Text('Reject (r)'),
                ),
              ],
            ),
          const SizedBox(height: 8),
          const Text(
            'Keyboard: j/k move · a approve · r reject',
            style: TextStyle(color: Colors.grey),
          ),
        ],
      ),
    );
  }
}
