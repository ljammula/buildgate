// The shared approve/reject-with-confirmation flow, used by both
// request_detail_screen.dart's Approve/Reject buttons and
// triage_screen.dart's a/r keyboard actions -- one implementation so the
// two surfaces can never drift on what "approve" means.
//
// Both write actions stay gated on [RunApi.canWrite]: [runApproveFlow]
// and [runRejectFlow] do nothing (return null) when it's false -- neither
// a build-time override token nor the server's own writes_enabled allows
// an unauthenticated write from this origin -- so a console with no way
// to write never even offers the confirm sheet/reason dialog for an
// action the server would 403 anyway ("restating confirmation is cheap
// insurance", extended to "an unavailable action should look
// unavailable, not fail after the fact"). A console that *can* write but
// gets refused anyway (a wider bind with no token, say) instead shows the
// server's own 403 reason via ErrorCallout -- never client-side gated
// away.
import 'package:flutter/material.dart';

import 'status.dart';

import 'api_client.dart';
import 'content_hash.dart';
import 'models.dart';
import 'operator_identity.dart';
import 'request_cost.dart';

/// The state Approve moves [state] to -- request.go's own transition
/// table (spec_review -> planning, plan_review -> building). Null for any
/// other state; ApproveConfirmSheet is only ever shown for a review
/// state, so callers only see that case in practice.
///
/// Kept as a fallback for [ApproveConfirmSheet.build] to use only when
/// [RequestSummary.approveNextState] is empty (a server predating that
/// field) -- this table has drifted from the server's own transition
/// before (it said `spec_review -> planning` for a `-draft-oracles`
/// request that actually goes to `oracle_drafting`), so the server's own
/// answer always wins when it's available.
String? nextStateAfterApprove(String state) => switch (state) {
  'spec_review' => 'planning',
  'oracle_review' => 'planning',
  'plan_review' => 'building',
  _ => null,
};

/// Resolves the operator's stored display name, prompting once via a
/// dialog when none is stored yet. Returns '' if the operator cancels
/// the prompt -- callers must treat that as "do not proceed", not as a
/// valid (empty) identity.
Future<String> ensureOperatorName(BuildContext context) async {
  final stored = getOperatorName();
  if (stored != null && stored.isNotEmpty) return stored;
  final entered = await showDialog<String>(
    context: context,
    barrierDismissible: false,
    builder: (_) => const _OperatorNameDialog(),
  );
  final name = (entered ?? '').trim();
  if (name.isNotEmpty) setOperatorName(name);
  return name;
}

class _OperatorNameDialog extends StatefulWidget {
  const _OperatorNameDialog();

  @override
  State<_OperatorNameDialog> createState() => _OperatorNameDialogState();
}

class _OperatorNameDialogState extends State<_OperatorNameDialog> {
  final _name = TextEditingController();

  @override
  void dispose() {
    _name.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: const Text('Your name'),
    content: TextField(
      key: const ValueKey('operator-name-field'),
      controller: _name,
      autofocus: true,
      decoration: const InputDecoration(
        labelText: 'Operator name',
        helperText: 'Recorded on every approve/reject you make from here.',
        helperMaxLines: 3,
      ),
    ),
    actions: [
      TextButton(
        onPressed: () => Navigator.pop(context),
        child: const Text('Cancel'),
      ),
      FilledButton(
        key: const ValueKey('operator-name-confirm-button'),
        onPressed: () {
          final value = _name.text.trim();
          if (value.isEmpty) return;
          Navigator.pop(context, value);
        },
        child: const Text('Continue'),
      ),
    ],
  );
}

/// The approve confirm sheet: restates the state transition, ticket
/// count, cost so far, and rejection count before the operator commits
/// -- approve starts sandboxed builds, so this is cheap insurance
/// against a stray click.
class ApproveConfirmSheet extends StatelessWidget {
  const ApproveConfirmSheet({
    required this.request,
    this.costSummary,
    this.skipWarning,
    this.skipsOracle = false,
    super.key,
  });

  final RequestSummary request;

  /// This approval leaves oracle_review with no oracle files (the "Approve
  /// (skip oracle)" path). Always stated on the sheet, not only in the
  /// panel behind it -- found by scripts/console-walk, 2026-09-24.
  final bool skipsOracle;

  /// Set when this approval skips the oracle stage with no oracle files after
  /// a draft that was not a deliberate none_eligible.
  final String? skipWarning;

  // Falls back to when [request] itself carries no cost_summary (the
  // detail route never returns one -- see request_cost.dart's own doc
  // comment) -- a caller with an earlier list-route response for the
  // same request (request_detail_screen.dart's own initialRequest) can
  // still show a real figure instead of "—".
  final CostSummary? costSummary;

  @override
  Widget build(BuildContext context) {
    // The server's own answer wins whenever it's present -- see
    // nextStateAfterApprove's own doc comment for why the Dart-side table
    // is a fallback only, not the primary source.
    final next = request.approveNextState.isNotEmpty
        ? request.approveNextState
        : (nextStateAfterApprove(request.state) ?? request.state);
    final cost = request.costSummary ?? costSummary;
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.all(20),
        child: SingleChildScrollView(
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                'Approve this request?',
                style: Theme.of(context).textTheme.titleLarge,
              ),
              const SizedBox(height: 12),
              Text('${stateLabel(request.state)} → ${stateLabel(next)}'),
              Text(
                request.ticketCount == 0
                    ? 'Tickets: none yet'
                    : 'Tickets: ${request.ticketCount}',
              ),
              // costSummary is only ever present here when the caller
              // reached this sheet from data GET /requests (not
              // /requests/{id}) supplied -- see request_cost.dart's own
              // doc comment for that gap. '—' is the honest answer when
              // it's unknown, never a fabricated figure.
              if (cost == null)
                const Text('Usage so far: —')
              else
                for (final line in formatUsageLines(cost))
                  Text('Usage so far: $line'),
              Text('Prior rejections: ${request.rejections.length}'),
              if (skipsOracle && skipWarning == null) ...[
                const SizedBox(height: 12),
                const Text(
                  'No oracle files: the build will have no request-level '
                  'acceptance test.',
                  key: ValueKey('approve-oracle-skip-consequence'),
                ),
              ],
              if (skipWarning != null) ...[
                const SizedBox(height: 12),
                Text(
                  skipWarning!,
                  key: const ValueKey('approve-oracle-skip-warning'),
                  style: TextStyle(color: Theme.of(context).colorScheme.error),
                ),
              ],
              const SizedBox(height: 20),
              Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  TextButton(
                    onPressed: () => Navigator.pop(context, false),
                    child: const Text('Cancel'),
                  ),
                  const SizedBox(width: 8),
                  FilledButton(
                    key: const ValueKey('approve-confirm-button'),
                    onPressed: () => Navigator.pop(context, true),
                    child: const Text('Approve'),
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

/// The reject reason dialog. Reason is required -- empty reason is
/// useless to the redrafting agent and to the audit line -- the confirm
/// button is a no-op until non-empty, the same
/// pattern request_detail_screen.dart's pre-2.4 reject dialog already
/// used.
class RejectReasonDialog extends StatefulWidget {
  const RejectReasonDialog({super.key});

  @override
  State<RejectReasonDialog> createState() => _RejectReasonDialogState();
}

class _RejectReasonDialogState extends State<RejectReasonDialog> {
  final _reason = TextEditingController();

  @override
  void dispose() {
    _reason.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: const Text('Request changes'),
    content: TextField(
      key: const ValueKey('reject-reason'),
      controller: _reason,
      decoration: const InputDecoration(labelText: 'Reason'),
      autofocus: true,
      maxLines: null,
      minLines: 3,
      keyboardType: TextInputType.multiline,
    ),
    actions: [
      TextButton(
        onPressed: () => Navigator.pop(context),
        child: const Text('Cancel'),
      ),
      FilledButton(
        key: const ValueKey('reject-confirm-button'),
        onPressed: () {
          final reason = _reason.text.trim();
          if (reason.isEmpty) return;
          Navigator.pop(context, reason);
        },
        child: const Text('Request changes'),
      ),
    ],
  );
}

/// [RejectReasonDialog]'s own shape, generalized for [runRetryFlow] and
/// [runCancelFlow] (recovery actions on a halted/quarantined request)
/// -- same "reason required, multi-line, non-dismissible" behavior, with
/// the title/label/confirm text and widget keys parameterized instead of
/// duplicating the whole dialog per action.
class _ReasonDialog extends StatefulWidget {
  const _ReasonDialog({
    required this.title,
    required this.confirmLabel,
    required this.fieldKey,
    required this.confirmKey,
  });

  final String title;
  final String confirmLabel;
  final Key fieldKey;
  final Key confirmKey;

  @override
  State<_ReasonDialog> createState() => _ReasonDialogState();
}

class _ReasonDialogState extends State<_ReasonDialog> {
  final _reason = TextEditingController();

  @override
  void dispose() {
    _reason.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: Text(widget.title),
    content: TextField(
      key: widget.fieldKey,
      controller: _reason,
      decoration: const InputDecoration(labelText: 'Reason'),
      autofocus: true,
      maxLines: null,
      minLines: 3,
      keyboardType: TextInputType.multiline,
    ),
    actions: [
      TextButton(
        onPressed: () => Navigator.pop(context),
        child: const Text('Cancel'),
      ),
      FilledButton(
        key: widget.confirmKey,
        onPressed: () {
          final reason = _reason.text.trim();
          if (reason.isEmpty) return;
          Navigator.pop(context, reason);
        },
        child: Text(widget.confirmLabel),
      ),
    ],
  );
}

/// The send-back dialog: [_ReasonDialog]'s own shape, plus a
/// choice of target ("plan", the default, or "spec") -- request.SendBack's
/// own two legal targets. Confirm is disabled until the reason is
/// non-empty, same as every other reason dialog here.
class _SendBackDialog extends StatefulWidget {
  const _SendBackDialog({required this.planAllowed, this.preferSpec = false});

  /// [RequestSummary.canSendBackToPlan]: when false the "plan" target is
  /// disabled and the dialog starts on "spec", the only target the server
  /// would accept.
  final bool planAllowed;

  /// Start on "spec" even when "plan" is allowed: a spec_conformity
  /// quarantine usually means the criterion itself needs rewording.
  final bool preferSpec;

  @override
  State<_SendBackDialog> createState() => _SendBackDialogState();
}

class _SendBackDialogState extends State<_SendBackDialog> {
  final _reason = TextEditingController();
  late String _target = (widget.planAllowed && !widget.preferSpec)
      ? 'plan'
      : 'spec';

  @override
  void dispose() {
    _reason.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: const Text('Send back'),
    content: Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        TextField(
          key: const ValueKey('send-back-reason'),
          controller: _reason,
          decoration: const InputDecoration(labelText: 'Reason'),
          autofocus: true,
          maxLines: null,
          minLines: 3,
          keyboardType: TextInputType.multiline,
        ),
        const SizedBox(height: 12),
        RadioListTile<String>(
          key: const ValueKey('send-back-target-plan'),
          contentPadding: EdgeInsets.zero,
          title: const Text('Back to planning'),
          value: 'plan',
          groupValue: _target,
          onChanged: widget.planAllowed
              ? (value) => setState(() => _target = value ?? 'plan')
              : null,
        ),
        RadioListTile<String>(
          key: const ValueKey('send-back-target-spec'),
          contentPadding: EdgeInsets.zero,
          title: const Text('Back to spec drafting'),
          value: 'spec',
          groupValue: _target,
          onChanged: (value) => setState(() => _target = value ?? 'plan'),
        ),
      ],
    ),
    actions: [
      TextButton(
        onPressed: () => Navigator.pop(context),
        child: const Text('Cancel'),
      ),
      FilledButton(
        key: const ValueKey('send-back-confirm-button'),
        onPressed: () {
          final reason = _reason.text.trim();
          if (reason.isEmpty) return;
          Navigator.pop(context, (reason: reason, target: _target));
        },
        child: const Text('Send back'),
      ),
    ],
  );
}

/// The warning to show before approving oracle_review when the approval will
/// pin no oracle files after a draft that was not a deliberate none_eligible
/// (or no-drafter) outcome; null when there is nothing to warn about.
String? oracleSkipWarningFor(
  RequestSummary request,
  Map<String, String>? expectedSha256,
) {
  if (request.state != 'oracle_review') return null;
  if (expectedSha256 == null || expectedSha256.isNotEmpty) return null;
  final status = request.oracleDraftStatus;
  if (status.isEmpty ||
      status == 'none_eligible' ||
      status == 'not_implemented') {
    return null;
  }
  final detail = request.oracleDraftDetail;
  return 'Approving skips the oracle stage: the draft ended "$status" and no '
      'oracle files exist, so this request will be built with no acceptance '
      'oracle.${detail.isEmpty ? '' : ' ($detail)'}';
}

/// Runs the full approve flow: resolves the operator's identity, shows
/// the confirm sheet, then calls
/// [RunApi.approveRequest]. Returns null -- doing nothing else -- when the
/// override token isn't configured, or the operator cancels the name
/// prompt or the confirm sheet; callers must not assume a non-null result
/// is the only outcome that leaves the request unchanged. [expectedSha256]
/// overrides the map derived from [request] -- oracle_review passes the
/// hashes of the oracle files it displayed.
Future<RequestSummary?> runApproveFlow(
  BuildContext context, {
  required RunApi api,
  required RequestSummary request,
  CostSummary? costSummary,
  Map<String, String>? expectedSha256,
}) async {
  if (!api.canWrite) return null;
  final by = await ensureOperatorName(context);
  if (by.isEmpty || !context.mounted) return null;
  final confirmed = await showModalBottomSheet<bool>(
    context: context,
    builder: (_) => ApproveConfirmSheet(
      request: request,
      costSummary: costSummary,
      skipWarning: oracleSkipWarningFor(request, expectedSha256),
      skipsOracle:
          request.state == 'oracle_review' &&
          expectedSha256 != null &&
          expectedSha256.isEmpty,
    ),
  );
  if (confirmed != true || !context.mounted) return null;
  // Binds this approval to the exact content this screen fetched and
  // showed -- the server refuses if spec.md/a ticket spec changed on
  // disk between that fetch and this call arriving, rather than
  // approving content the operator never saw.
  return api.approveRequest(
    request.id,
    by: by,
    expectedSha256: expectedSha256 ?? expectedSha256For(request),
  );
}

/// Runs the full reject flow: resolves the operator's identity, requires
/// a non-empty reason, then calls
/// [RunApi.rejectRequest]. Same null-on-abandon contract as
/// [runApproveFlow].
Future<RequestSummary?> runRejectFlow(
  BuildContext context, {
  required RunApi api,
  required RequestSummary request,
}) async {
  if (!api.canWrite) return null;
  final by = await ensureOperatorName(context);
  if (by.isEmpty || !context.mounted) return null;
  // barrierDismissible: false -- an Esc/click-outside used to
  // silently discard whatever the operator had already typed, with no
  // confirmation; the dialog's own Cancel button is still available.
  final reason = await showDialog<String>(
    context: context,
    barrierDismissible: false,
    builder: (_) => const RejectReasonDialog(),
  );
  if (reason == null || reason.isEmpty || !context.mounted) return null;
  return api.rejectRequest(request.id, reason: reason, by: by);
}

/// Runs the recovery "Retry" flow: resolves the operator's identity,
/// requires a non-empty reason, then calls [RunApi.retryRequest]. Same
/// null-on-abandon contract as [runApproveFlow]/[runRejectFlow].
Future<RequestSummary?> runRetryFlow(
  BuildContext context, {
  required RunApi api,
  required RequestSummary request,
}) async {
  if (!api.canWrite) return null;
  final by = await ensureOperatorName(context);
  if (by.isEmpty || !context.mounted) return null;
  final reason = await showDialog<String>(
    context: context,
    barrierDismissible: false,
    builder: (_) => const _ReasonDialog(
      title: 'Retry this request',
      confirmLabel: 'Retry',
      fieldKey: ValueKey('retry-reason'),
      confirmKey: ValueKey('retry-confirm-button'),
    ),
  );
  if (reason == null || reason.isEmpty || !context.mounted) return null;
  return api.retryRequest(request.id, reason: reason, by: by);
}

/// Runs the resume_review "Resume" / "Rebuild from scratch" flow: resolves
/// the operator's identity, asks for confirmation (a rebuild from scratch
/// is a fresh, paid run), then calls [RunApi.resumeRequest] with [from]
/// (`round` or `scratch`). Same null-on-abandon contract as
/// [runRetryFlow]/[runCancelFlow].
Future<RequestSummary?> runResumeFlow(
  BuildContext context, {
  required RunApi api,
  required RequestSummary request,
  required String from,
}) async {
  if (!api.canWrite) return null;
  final by = await ensureOperatorName(context);
  if (by.isEmpty || !context.mounted) return null;
  final step = request.resume?.fromState ?? '';
  final rerun = step.isNotEmpty && step != 'building';
  final scratch = !rerun && from == 'scratch';
  final confirmed = await showDialog<bool>(
    context: context,
    barrierDismissible: false,
    builder: (_) => AlertDialog(
      title: Text(
        rerun
            ? 'Rerun this step'
            : scratch
            ? 'Rebuild from scratch'
            : 'Resume this request',
      ),
      content: Text(
        rerun
            ? 'Run the lost ${stateLabel(step)} step again.'
            : scratch
            ? 'Discard the lost build and rebuild the ticket from the '
                  'start. This is a fresh, paid run.'
            : 'Continue the lost step where the worker stopped.',
      ),
      actions: [
        TextButton(
          key: const ValueKey('resume-dismiss-button'),
          onPressed: () => Navigator.pop(context, false),
          child: const Text('Back'),
        ),
        FilledButton(
          key: const ValueKey('resume-confirm-button'),
          onPressed: () => Navigator.pop(context, true),
          child: Text(
            rerun
                ? 'Rerun step'
                : scratch
                ? 'Rebuild'
                : 'Resume',
          ),
        ),
      ],
    ),
  );
  if (confirmed != true || !context.mounted) return null;
  return api.resumeRequest(request.id, from: from, by: by);
}

/// Runs the recovery "Cancel" flow: [runRetryFlow]'s own shape,
/// calling [RunApi.cancelRequest] instead.
Future<RequestSummary?> runCancelFlow(
  BuildContext context, {
  required RunApi api,
  required RequestSummary request,
}) async {
  if (!api.canWrite) return null;
  final by = await ensureOperatorName(context);
  if (by.isEmpty || !context.mounted) return null;
  final reason = await showDialog<String>(
    context: context,
    barrierDismissible: false,
    builder: (_) => const _ReasonDialog(
      title: 'Cancel this request',
      confirmLabel: 'Cancel request',
      fieldKey: ValueKey('cancel-reason'),
      confirmKey: ValueKey('cancel-confirm-button'),
    ),
  );
  if (reason == null || reason.isEmpty || !context.mounted) return null;
  return api.cancelRequest(request.id, reason: reason, by: by);
}

/// Runs the recovery "Send back" flow: resolves the operator's
/// identity, requires a non-empty reason plus a plan/spec target, then
/// calls [RunApi.rejectRequest] with `to` set -- the console's own route
/// to internal/request.SendBack. Same null-on-abandon contract as
/// [runRetryFlow]/[runCancelFlow].
Future<RequestSummary?> runSendBackFlow(
  BuildContext context, {
  required RunApi api,
  required RequestSummary request,
}) async {
  if (!api.canWrite) return null;
  final by = await ensureOperatorName(context);
  if (by.isEmpty || !context.mounted) return null;
  final result = await showDialog<({String reason, String target})>(
    context: context,
    barrierDismissible: false,
    builder: (_) => _SendBackDialog(
      planAllowed: request.canSendBackToPlan,
      preferSpec: request.quarantineCheck == 'spec_conformity',
    ),
  );
  if (result == null || result.reason.isEmpty || !context.mounted) {
    return null;
  }
  return api.rejectRequest(
    request.id,
    reason: result.reason,
    by: by,
    to: result.target,
  );
}
