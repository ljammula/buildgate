import 'dart:convert';
import 'dart:async';

import 'package:flutter/material.dart';

import 'api_client.dart';
import 'diff_screen.dart';
import 'elapsed.dart';
import 'error_display.dart';
import 'models.dart';
import 'open_in_new_tab.dart';
import 'release_screen.dart';
import 'request_cost.dart';
import 'request_detail_screen.dart';
import 'run_list_screen.dart';

class RunDetailScreen extends StatefulWidget {
  const RunDetailScreen({
    required this.api,
    required this.runId,
    this.initialRun,
    this.initialOverrideReason,
    super.key,
  });

  final RunApi api;
  final String runId;
  final Run? initialRun;

  // Pre-fills the override dialog's reason field:
  // request_detail_screen.dart's "Quarantined" callout links here with the
  // request/ticket context already known, so the operator doesn't have to
  // retype what brought them here.
  final String? initialOverrideReason;

  @override
  State<RunDetailScreen> createState() => _RunDetailScreenState();
}

class _RunDetailScreenState extends State<RunDetailScreen> {
  Run? _run;
  Object? _error;
  Object? _overrideError;
  bool _overriding = false;
  StreamSubscription<Run>? _events;
  // The run's own request, fetched once for its title (no link between a
  // run and its request existed before): a nice-to-have receipt for the app
  // bar subtitle, not something any gate or navigation depends on, so a
  // failed fetch is tolerated silently by _maybeLoadLinkedRequest below.
  RequestSummary? _linkedRequest;
  String? _linkedRequestIdLoaded;
  final GlobalKey<_LogPaneState> _logPaneKey = GlobalKey<_LogPaneState>();

  @override
  void initState() {
    super.initState();
    _run = widget.initialRun;
    if (_run case final run?) {
      _maybeLoadLinkedRequest(run);
      _watch(run);
    } else {
      _load();
    }
  }

  Future<void> _load() async {
    try {
      final run = await widget.api.getRun(widget.runId);
      if (!mounted) return;
      setState(() => _run = run);
      _maybeLoadLinkedRequest(run);
      _watch(run);
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    }
  }

  // Fetches run.requestId's own RequestSummary once per requestId --
  // guarded by _linkedRequestIdLoaded so a live /events update doesn't
  // refetch it on every tick. A failure here is silent: the "Open
  // request" action still works from run.requestId alone even without
  // the title.
  void _maybeLoadLinkedRequest(Run run) {
    if (run.requestId.isEmpty || _linkedRequestIdLoaded == run.requestId) {
      return;
    }
    _linkedRequestIdLoaded = run.requestId;
    widget.api
        .getRequest(run.requestId)
        .then((request) {
          if (mounted) setState(() => _linkedRequest = request);
        })
        .catchError((Object _) {});
  }

  void _watch(Run run) {
    if (run.isTerminal) return;
    _events?.cancel();
    _events = widget.api
        .watchRun(widget.runId)
        .listen(
          (updated) {
            if (mounted) setState(() => _run = updated);
            _maybeLoadLinkedRequest(updated);
          },
          onError: (Object error) {
            if (mounted) setState(() => _error = error);
          },
        );
  }

  Future<void> _showOverride() async {
    final values = await showDialog<_OverrideValues>(
      context: context,
      builder: (_) =>
          _OverrideDialog(initialReason: widget.initialOverrideReason),
    );
    if (values == null || !mounted) return;

    setState(() {
      _overriding = true;
      _overrideError = null;
    });
    try {
      final updated = await widget.api.overrideRun(
        widget.runId,
        by: values.by,
        reason: values.reason,
        state: values.state,
      );
      await _events?.cancel();
      _events = null;
      if (mounted) {
        setState(() => _run = updated);
        _watch(updated);
      }
    } on Object catch (error) {
      if (mounted) setState(() => _overrideError = error);
    } finally {
      if (mounted) setState(() => _overriding = false);
    }
  }

  @override
  void dispose() {
    _events?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final run = _run;
    final linkedRequest = _linkedRequest;
    return Scaffold(
      appBar: AppBar(
        title: Text(run?.ticket ?? 'Run detail'),
        // The request's own title, once fetched -- shown alongside
        // the raw ticket id rather than in place of it, so an operator
        // who already knows the ticket id isn't left without it.
        bottom: linkedRequest == null
            ? null
            : PreferredSize(
                preferredSize: const Size.fromHeight(20),
                child: Padding(
                  padding: const EdgeInsets.only(bottom: 6),
                  child: Text(
                    linkedRequest.title,
                    style: Theme.of(context).textTheme.bodySmall,
                  ),
                ),
              ),
        actions: [
          if (run != null && run.requestId.isNotEmpty)
            IconButton(
              key: const ValueKey('open-request-button'),
              onPressed: () => Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) => RequestDetailScreen(
                    api: widget.api,
                    requestId: run.requestId,
                    initialRequest: linkedRequest,
                  ),
                ),
              ),
              tooltip: 'Open request',
              icon: const Icon(Icons.open_in_new),
            ),
        ],
      ),
      body: switch ((run, _error)) {
        (null, final Object error) => Center(
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: ErrorCallout(error: error, onRetry: _load),
          ),
        ),
        (null, null) => const Center(child: CircularProgressIndicator()),
        (final Run value, _) => _RunSections(
          api: widget.api,
          run: value,
          streamError: _error,
          onOverride: _showOverride,
          overriding: _overriding,
          overrideError: _overrideError,
          logPaneKey: _logPaneKey,
        ),
      },
    );
  }
}

class _RunSections extends StatelessWidget {
  const _RunSections({
    required this.api,
    required this.run,
    required this.streamError,
    required this.onOverride,
    required this.overriding,
    required this.overrideError,
    required this.logPaneKey,
  });

  final RunApi api;
  final Run run;
  final Object? streamError;
  final VoidCallback onOverride;
  final bool overriding;
  final Object? overrideError;
  final GlobalKey<_LogPaneState> logPaneKey;

  // Enables the build-log pane and scrolls it into view -- the "Open log"
  // action next to an attempt's log path: reuses the existing
  // viewer instead of opening a new tab, preferring an in-app pane
  // when one already exists.
  void _openLog(BuildContext context) {
    logPaneKey.currentState?.enable();
    final logContext = logPaneKey.currentContext;
    if (logContext != null) {
      Scrollable.ensureVisible(
        logContext,
        duration: const Duration(milliseconds: 200),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    final diff = run.diffStat;
    final files = run.changedFiles;
    return ListView(
      padding: const EdgeInsets.all(16),
      // A large cacheExtent (for the "Open log" action, jumping from an
      // Attempts card back up to the Build log section): Flutter's
      // default Sliver-backed ListView only keeps children within ~250
      // logical pixels of the viewport mounted, so logPaneKey's
      // GlobalKey<_LogPaneState> would otherwise go null (both the state
      // to enable and the context to scroll to) once an operator has
      // scrolled far enough past it to reach the Attempts section below.
      // This screen's own content is bounded (one run's evidence, not an
      // unbounded feed), so keeping the whole thing mounted is cheap.
      // scrollCacheExtent's replacement type (ScrollCacheExtent) isn't
      // exported from package:flutter's public widgets/material surface in
      // this SDK version; the plain double form still works identically.
      // ignore: deprecated_member_use
      cacheExtent: 5000,
      children: [
        // Timeline (Phase 4, follow-along console): first, above every
        // other section -- "what's it doing right now" is the thing an
        // operator watching a run wants before any of the evidence below,
        // which only exists once a stage has actually finished.
        _Section(
          title: 'Timeline',
          children: [_Timeline(api: api, run: run)],
        ),
        _Section(
          title: 'Run',
          children: [
            _Field('State', StateBadge(state: run.state)),
            _TextField('Run ID', run.id),
            _Field('Created', LocalTimeText(run.createdAt)),
            _Field('Updated', LocalTimeText(run.updatedAt)),
            // Model id and tokens spent across this run's own attempts
            // (internal/api's runView.ByModel) -- the operator explicitly
            // does not want a dollar figure here, only model+tokens.
            if (run.byModel.isNotEmpty)
              _TextField(
                'Usage',
                run.byModel
                    .map(
                      (m) =>
                          formatModelUsageLine(m, complete: run.tokensComplete),
                    )
                    .join(', '),
              ),
            if (streamError != null)
              _TextField('Live updates', 'Disconnected: $streamError'),
            // Temporal UI link: only offered when both the server's
            // own console-config.json advertises a temporal_ui_url and
            // this run recorded its own workflow id -- either missing
            // means there is nothing to link to.
            if ((api.temporalUiUrl?.isNotEmpty ?? false) &&
                run.temporalWorkflowId.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: OutlinedButton.icon(
                  key: const ValueKey('temporal-ui-button'),
                  onPressed: () => openInNewTab(
                    '${api.temporalUiUrl}/namespaces/default/workflows/'
                    '${Uri.encodeComponent(run.temporalWorkflowId)}',
                  ),
                  icon: const Icon(Icons.open_in_new),
                  label: const Text('Open in Temporal UI'),
                ),
              ),
          ],
        ),
        _Section(
          title: 'Build log',
          children: [_LogPane(key: logPaneKey, api: api, runId: run.id)],
        ),
        if (run.state == 'quarantined')
          _Section(
            title: 'Operator override',
            children: [
              _TextField(
                'Action',
                'Move this quarantined run to an accepted or halted state.',
              ),
              if (overrideError != null)
                _TextField('Error', 'Could not override run: $overrideError'),
              // Gated on api.hasOverrideToken specifically, not api.canWrite
              // (an adversarial review, 2026-09-24, found): unlike the
              // request-pipeline write buttons canWrite covers, the server's
              // own authorizeOverride (internal/api/server.go) never grants
              // the loopback no-token relaxation to POST /runs/{id}/
              // override -- it always requires the bearer token. A button
              // enabled on canWrite alone would let an operator click
              // Override on a loopback console with no override token
              // configured and get a 403 back, with no token this console
              // has any way to supply.
              FilledButton.icon(
                key: const ValueKey('override-run-button'),
                onPressed: (overriding || !api.hasOverrideToken)
                    ? null
                    : onOverride,
                icon: overriding
                    ? const SizedBox.square(
                        dimension: 18,
                        child: CircularProgressIndicator(strokeWidth: 2),
                      )
                    : const Icon(Icons.admin_panel_settings),
                label: Text(overriding ? 'Applying…' : 'Override run'),
              ),
            ],
          ),
        _Section(
          title: 'Commit and artifact evidence',
          children: [
            _TextField('Base SHA', run.baseSha),
            _TextField('Result SHA', run.resultSha ?? 'Not available'),
            _TextField('Spec SHA-256', run.specSha256),
            _TextField(
              'Committed by factoryd',
              run.committedByFactoryd ? 'Yes' : 'No',
            ),
          ],
        ),
        _Section(
          title: 'Attempts',
          children: run.attempts.isEmpty
              ? [_TextField('Attempts', 'None')]
              : [
                  for (final attempt in run.attempts)
                    _AttemptCard(
                      attempt: attempt,
                      onOpenLog: () => _openLog(context),
                    ),
                ],
        ),
        _Section(
          title: 'Gate results',
          children: run.gateResults.isEmpty
              ? [_TextField('Gate results', 'None')]
              : [
                  for (final gate in run.gateResults)
                    _EvidenceCard(
                      title: gate.check,
                      lines: [
                        'Command: ${gate.command.join(' ')}',
                        'Passed: ${gate.passed ? 'Yes' : 'No'}',
                        'Exit code: ${gate.exitCode}',
                        'Duration: ${gate.durationMs} ms',
                        'Log SHA-256: ${gate.logSha256}',
                      ],
                    ),
                ],
        ),
        if (run.composePhases.isNotEmpty)
          _Section(
            title: 'Compose services',
            children: [
              for (final phase in run.composePhases)
                _EvidenceCard(
                  title: phase.phase.isEmpty ? 'run' : phase.phase,
                  lines: phase.enabled
                      ? [
                          for (final svc in phase.services)
                            '${svc.name}: ${svc.image} at ${svc.address}'
                                '${svc.digest.isEmpty ? '' : ' (${svc.digest})'}',
                        ]
                      : ['Not launched: ${phase.disabledReason}'],
                ),
            ],
          ),
        _Section(
          title: 'Changed files',
          children: [
            if (files == null)
              _TextField('Files', 'Not collected')
            else if (files.isEmpty)
              _TextField('Files', 'None')
            else
              for (final file in files) _TextField('File', file),
            _TextField(
              'Diff stat',
              diff == null
                  ? 'Not available'
                  : '${diff.filesChanged} files, '
                        '+${diff.insertions}, -${diff.deletions}',
            ),
            // Gated on diffAvailable, not merely resultSha != null — found
            // via review: a run whose evidence collection warned-and-
            // continued on the diff step (or predates this field) still
            // has a resultSha but no snapshot to fetch, and the button
            // would otherwise open a screen that can only show an error.
            if (run.diffAvailable)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: OutlinedButton.icon(
                  key: const ValueKey('view-diff-button'),
                  onPressed: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => DiffScreen(api: api, runId: run.id),
                    ),
                  ),
                  icon: const Icon(Icons.difference_outlined),
                  label: const Text('View diff'),
                ),
              ),
          ],
        ),
        if (run.notifications.isNotEmpty)
          _Section(
            title: 'Notifications',
            children: [
              for (final notification in run.notifications)
                _EvidenceCard(
                  title: notification.state,
                  lines: [
                    notification.reason,
                    'Sent: ${formatLocalTimestamp(notification.sentAt)}',
                    'Ticket: ${notification.ticket}',
                    'Run ID: ${notification.runId}',
                  ],
                ),
            ],
          ),
        // Always offered, not only for an accepted run: the release screen
        // reports the project kill switch's state and history too, which is
        // meaningful for any run — and a run with no decision recorded is
        // itself the answer to "was this released?", stated there
        // explicitly rather than left to inference from a missing button.
        _Section(
          title: 'Release',
          children: [
            _TextField(
              'Decision',
              'The factory-owned release decision for this run, and the '
                  'project kill switch it was evaluated against.',
            ),
            Padding(
              padding: const EdgeInsets.only(top: 8),
              child: OutlinedButton.icon(
                key: const ValueKey('view-release-button'),
                onPressed: () => Navigator.of(context).push(
                  MaterialPageRoute<void>(
                    builder: (_) => ReleaseScreen(api: api, runId: run.id),
                  ),
                ),
                icon: const Icon(Icons.gavel_outlined),
                label: const Text('View release decision'),
              ),
            ),
          ],
        ),
        if (run.overrides.isNotEmpty)
          _Section(
            title: 'Overrides',
            children: [
              for (final override in run.overrides)
                _EvidenceCard(
                  title: '${override.priorState} → ${override.newState}',
                  lines: [
                    'By: ${override.by}',
                    'At: ${formatLocalTimestamp(override.at)}',
                    'Reason: ${override.reason}',
                  ],
                ),
            ],
          ),
      ],
    );
  }
}

/// The collapsible live build-log pane: off by
/// default, opt-in per run so a hundred open tabs are not a hundred
/// active `GET /runs/{id}/log?follow=1` tail streams. A viewer only --
/// there is deliberately no "send input"/"restart" affordance here, and
/// the log's own text is worker-authored/untrusted content, rendered as
/// plain [SelectableText] with no HTML interpretation and no automatic
/// link detection, matching [watchRunLog]'s own trust-boundary doc
/// comment.
class _LogPane extends StatefulWidget {
  const _LogPane({required this.api, required this.runId, super.key});

  final RunApi api;
  final String runId;

  @override
  State<_LogPane> createState() => _LogPaneState();
}

class _LogPaneState extends State<_LogPane> {
  // Called from an attempt's "Open log" action via
  // _RunSections.logPaneKey -- turns the pane on the same way the switch
  // itself would, so "open the log" from an attempt card reuses this
  // existing viewer instead of a second one.
  void enable() => _setEnabled(true);

  // Caps how much log text this pane keeps/renders: a long-running build
  // otherwise grows both memory and the SelectableText rebuild cost
  // without bound for as long as the tab stays open -- a viewer that is
  // off by default is undermined if leaving it on is itself the thing
  // that degrades the tab. Keeping only the
  // most recent slice is the right trade-off for a *tail* viewer -- the
  // oldest output is also the least likely to still be relevant.
  static const _maxBufferChars = 200000;

  bool _enabled = false;
  String _text = '';
  StreamSubscription<String>? _subscription;
  Object? _error;
  bool _closed = false;

  @override
  void dispose() {
    _subscription?.cancel();
    super.dispose();
  }

  void _setEnabled(bool enabled) {
    setState(() => _enabled = enabled);
    if (enabled) {
      _connect();
    } else {
      _subscription?.cancel();
      _subscription = null;
    }
  }

  void _connect() {
    _text = '';
    _error = null;
    _closed = false;
    _subscription = widget.api
        .watchRunLog(widget.runId)
        .listen(
          (chunk) {
            if (!mounted) return;
            setState(() {
              final combined = _text + chunk;
              _text = combined.length > _maxBufferChars
                  ? combined.substring(combined.length - _maxBufferChars)
                  : combined;
            });
          },
          onError: (Object error) {
            if (mounted) setState(() => _error = error);
          },
          onDone: () {
            if (mounted) setState(() => _closed = true);
          },
        );
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        SwitchListTile(
          key: const ValueKey('log-pane-toggle'),
          contentPadding: EdgeInsets.zero,
          title: const Text('Show live build log'),
          subtitle: const Text(
            'Viewer only -- plain text, no input is ever sent from here.',
          ),
          value: _enabled,
          onChanged: _setEnabled,
        ),
        if (_enabled) ...[
          if (_error != null)
            Padding(
              padding: const EdgeInsets.only(bottom: 8),
              child: ErrorCallout(error: _error!, onRetry: _connect),
            ),
          if (_closed)
            const Padding(
              padding: EdgeInsets.only(bottom: 8),
              child: Text('Log stream closed.'),
            ),
          Container(
            key: const ValueKey('log-pane-content'),
            width: double.infinity,
            height: 320,
            padding: const EdgeInsets.all(8),
            decoration: BoxDecoration(
              color: Theme.of(context).colorScheme.surfaceContainerHighest,
              border: Border.all(color: Theme.of(context).dividerColor),
            ),
            child: SingleChildScrollView(
              child: SelectableText(
                _text.isEmpty ? '(no log output yet)' : readableBuildLog(_text),
                style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
              ),
            ),
          ),
        ],
      ],
    );
  }
}

/// The exit line for an attempt. A combined review launch exits
/// [combinedReviewExitBase] + bits (bit 0: spec conformity did not pass,
/// bit 1: code review did not pass; reviewstep.CombinedExitBase in Go), so
/// its 40 reads as the pass it is rather than an unexplained failure code.
String attemptExitText(Attempt attempt) {
  final bits = attempt.exitCode - combinedReviewExitBase;
  if (attempt.kind == 'review' && bits >= 0 && bits <= 3) {
    final conformity = bits & 1 == 0 ? 'passed' : 'failed';
    final codeReview = bits & 2 == 0 ? 'passed' : 'failed';
    return 'Exit code: ${attempt.exitCode} '
        '(spec conformity $conformity, code review $codeReview)';
  }
  return 'Exit code: ${attempt.exitCode}';
}

/// combined_review.py's exit-status offset; must match
/// reviewstep.CombinedExitBase.
const combinedReviewExitBase = 40;

/// The live build log as shown: a worker's `FACTORY_PROGRESS <json>`
/// protocol lines become one readable step each, the way `factoryd watch`
/// renders them ("round 1/3  started", "agent  read: main.go"); every
/// other line, and any line that does not parse (a chunk cut mid-line),
/// is kept verbatim.
String readableBuildLog(String raw) {
  const prefix = 'FACTORY_PROGRESS ';
  return raw
      .split('\n')
      .map((line) {
        if (!line.startsWith(prefix)) return line;
        try {
          final event = jsonDecode(line.substring(prefix.length));
          if (event is! Map<String, dynamic>) return line;
          return _progressStep(event) ?? line;
        } on FormatException {
          return line;
        }
      })
      .join('\n');
}

String? _progressStep(Map<String, dynamic> e) {
  final stage = e['stage'];
  final kind = e['event'];
  if (stage is! String || kind is! String) return null;
  final round = e['round'] is int ? e['round'] as int : 0;
  final maxRounds = e['max_rounds'] is int ? e['max_rounds'] as int : 0;
  final detail = (e['detail'] is String ? e['detail'] as String : '')
      .replaceAll(RegExp(r'\s+'), ' ')
      .trim();
  final outcome = e['outcome'] is String ? e['outcome'] as String : '';
  final label = stage == 'round' && round > 0
      ? 'round $round${maxRounds > 0 ? '/$maxRounds' : ''}'
      : stage;
  final String text;
  switch (kind) {
    case 'start':
      text = 'started';
    case 'end':
      text =
          stage == 'round' &&
              outcome.isNotEmpty &&
              outcome != 'pass' &&
              detail.isNotEmpty
          ? detail
          : (outcome.isEmpty ? 'done' : outcome);
    default:
      text = detail.isEmpty ? kind : detail;
  }
  return '▸ ${label.padRight(12)} $text';
}

/// The order Phase 4's Timeline renders factory stages in -- matches
/// progress-contract.md's own "Factory stages" list exactly. `gate` is
/// singular here (one row key) even though several distinct gates can each
/// emit their own start/end pair under it (detail = gate name) -- see
/// [_gateRow], which fans those out into that one row's sub-rows.
const _stageOrder = [
  'prepare_workspace',
  'preflight',
  'build',
  'post_build',
  'verify',
  'full_suite',
  'gate',
  'evidence',
  'conformity_review',
  'code_review',
  // 'review' is the combined-launch stage (reviewstep.CombinedStep):
  // when both spec-conformity and code review are enabled for a run,
  // factoryd runs ONE combined_review.py launch in place of the two
  // standalone stages above (measured live, a Flutter + Go app repo request,
  // 2026-09-28: review was 50% of buildgate's whole spend, split across
  // two separate fresh sandboxed sessions over the same diff). The two
  // standalone stages stay in this order for a run with only one review
  // enabled -- this is additive, not a replacement.
  'review',
  'commit_oracles',
  'post_oracle_commit_verify',
  'evaluate',
  'finished',
];

const _stageLabels = {
  'prepare_workspace': 'Prepare workspace',
  'preflight': 'Preflight',
  'build': 'Build',
  'post_build': 'Post-build',
  'verify': 'Verify',
  'full_suite': 'Full suite',
  'gate': 'Gates',
  'evidence': 'Evidence',
  'conformity_review': 'Conformity review',
  'code_review': 'Code review',
  'review': 'Review (conformity + code)',
  'commit_oracles': 'Commit oracles',
  'post_oracle_commit_verify': 'Re-verify after oracle commit',
  'evaluate': 'Evaluate',
  'finished': 'Finished',
};

enum _StageGlyph { pending, running, passed, failed, skipped }

/// The Timeline section (Phase 4, follow-along console): a vertical
/// stepper over the factory stages in [_stageOrder], built from
/// `GET /runs/{id}/progress`'s SSE feed (see [RunApi.watchRunProgress] and
/// [ProgressEvent]). Subscribed unconditionally, whether or not [run] is
/// currently terminal -- the server replays a finished run's full recorded
/// history before closing the connection (see [RunApi.watchRunProgress]'s
/// own doc comment), so there is no need for a separate one-shot fetch.
///
/// Worker `note` lines (source `"worker"`, relayed verbatim from the
/// sandbox's own stdout) are untrusted, display-only text -- rendered as
/// plain [SelectableText] with no link detection, the same trust boundary
/// [_LogPane]'s own doc comment describes for the build log.
class _Timeline extends StatefulWidget {
  const _Timeline({required this.api, required this.run});

  final RunApi api;
  final Run run;

  @override
  State<_Timeline> createState() => _TimelineState();
}

class _TimelineState extends State<_Timeline> {
  // Elapsed/duration text is computed from DateTime.now() at build time --
  // recomputed whenever a new progress event arrives (below) or the parent
  // RunDetailScreen's own run-state poll rebuilds this widget, rather than
  // a dedicated per-second ticker: request_list_screen.dart's own "last
  // updated Ns ago" freshness label follows the same rebuild-driven
  // pattern rather than a live clock, and a ticker here would otherwise
  // keep every widget test's pumpAndSettle() from ever settling.
  final List<ProgressEvent> _events = [];
  // Every line the feed has delivered, keyed by its own fields: the SSE
  // client replays the whole file from offset 0 on every reconnect (see
  // RunApi.watchRunProgress), so without this a transient drop would
  // append the run's entire history a second time.
  final Set<String> _seen = {};
  Object? _error;
  StreamSubscription<ProgressEvent>? _subscription;

  // Worker lines (round/agent notes) are bounded so a tab left open on a
  // long build cannot grow without limit; factory lines are few and are
  // what the stepper is built from, so they are always kept.
  static const _maxWorkerEvents = 4000;

  void _addEvent(ProgressEvent event) {
    final key =
        '${event.ts.toIso8601String()}|${event.source}|${event.stage}|'
        '${event.event}|${event.round}|${event.detail}';
    if (!_seen.add(key)) return;
    _events.add(event);
    if (event.source == 'worker') {
      var workerCount = 0;
      for (final e in _events) {
        if (e.source == 'worker') workerCount++;
      }
      if (workerCount > _maxWorkerEvents) {
        final drop = _events.indexWhere((e) => e.source == 'worker');
        if (drop >= 0) _events.removeAt(drop);
      }
    }
  }

  @override
  void initState() {
    super.initState();
    _subscription = widget.api
        .watchRunProgress(widget.run.id)
        .listen(
          (event) {
            if (mounted) setState(() => _addEvent(event));
          },
          onError: (Object error) {
            if (mounted) setState(() => _error = error);
          },
        );
  }

  @override
  void dispose() {
    _subscription?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final now = DateTime.now();
    final rows = _computeTimeline(_events, now, widget.run);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (_error != null)
          Padding(
            padding: const EdgeInsets.only(bottom: 8),
            child: ErrorCallout(error: _error!),
          ),
        _StatusStrip(events: _events, run: widget.run),
        const SizedBox(height: 8),
        for (final row in rows) _TimelineRowView(row: row),
      ],
    );
  }
}

/// The one-line "`<stage> · round n/m · elapsed mm:ss · <state badge>`"
/// strip atop the Timeline.
class _StatusStrip extends StatelessWidget {
  const _StatusStrip({required this.events, required this.run});

  final List<ProgressEvent> events;
  final Run run;

  @override
  Widget build(BuildContext context) {
    final factoryEvents = events.where((e) => e.source == 'factory').toList();
    final currentStage = factoryEvents.isNotEmpty
        ? factoryEvents.last.stage
        : '';
    // "Silence is a bug" (progress-contract.md, 2026-09-18): a server-
    // reported waiting_reason (queued behind another run, e.g.) explains
    // the silence, so it replaces the ordinary stage label rather than
    // sitting alongside it -- an operator should see *why* nothing is
    // happening, not "Pending" next to an unexplained gap.
    final waitingReason = run.waitingReason;
    final label = (waitingReason != null && waitingReason.isNotEmpty)
        ? waitingReason
        : (currentStage.isEmpty
              ? 'Pending'
              : (_stageLabels[currentStage] ?? currentStage));
    final roundEvents = events
        .where((e) => e.source == 'worker' && e.stage == 'round')
        .toList();
    final latestRound = roundEvents.isNotEmpty ? roundEvents.last : null;
    // A 'finished'/'end' factory event, when present, is the actual moment
    // this run stopped -- more precise than updatedAt, which can lag it.
    // Falls back to updatedAt for a run whose
    // progress feed wasn't (fully) fetched.
    final finishedEvent = factoryEvents
        .where((e) => e.stage == 'finished' && e.event == 'end')
        .toList();
    final terminalAt = finishedEvent.isNotEmpty
        ? finishedEvent.last.ts.toIso8601String()
        : run.updatedAt;
    // Quarantined counts as terminal here too -- see
    // Run.isTerminalForDisplay's own doc comment.
    final elapsed = elapsedBetween(
      run.createdAt,
      run.isTerminalForDisplay ? terminalAt : null,
    );
    // lastProgressAt is the newer of the server's own field and this
    // screen's live progress feed -- the feed can be
    // ahead of the last full Run fetch that field came from.
    final feedLastProgressAt = factoryEvents.isNotEmpty
        ? factoryEvents.last.ts
        : null;
    final fieldLastProgressAt = tryParseTimestamp(run.lastProgressAt ?? '');
    final lastProgressAt = _laterOf(feedLastProgressAt, fieldLastProgressAt);
    return Wrap(
      key: const ValueKey('timeline-status-strip'),
      crossAxisAlignment: WrapCrossAlignment.center,
      spacing: 12,
      children: [
        Text(label, style: Theme.of(context).textTheme.titleMedium),
        if (latestRound != null && latestRound.maxRounds > 0)
          Text('Round ${latestRound.round}/${latestRound.maxRounds}'),
        Text('Elapsed ${formatDuration(elapsed)}'),
        if (!run.isTerminalForDisplay)
          Text(
            'Last activity '
            '${formatDuration(elapsedBetween((lastProgressAt ?? tryParseTimestamp(run.createdAt))?.toIso8601String() ?? run.createdAt, null))} ago',
          ),
        StallChip(run: run),
        StateBadge(state: run.state),
      ],
    );
  }
}

/// The later of two possibly-null timestamps -- null only when both are.
DateTime? _laterOf(DateTime? a, DateTime? b) {
  if (a == null) return b;
  if (b == null) return a;
  return a.isAfter(b) ? a : b;
}

class _TimelineRow {
  _TimelineRow({
    required this.rowKey,
    required this.label,
    required this.glyph,
    this.durationText,
    this.subRows = const [],
    this.noteLines = const [],
    this.subtitle,
  });

  final String rowKey;
  final String label;
  final _StageGlyph glyph;
  final String? durationText;
  final List<_TimelineSubRow> subRows;
  final List<String> noteLines;
  // The latest factory `build` note's detail (progress-contract.md's
  // "Additions" one-line round summary, e.g. "3 rounds · r1 fail
  // (verify) · r3 pass · 41.2k tokens · $0.12") -- shown as this row's own
  // subtitle, distinct from subRows/noteLines below it.
  final String? subtitle;
}

class _TimelineSubRow {
  _TimelineSubRow({
    required this.label,
    required this.glyph,
    this.durationText,
    this.factoryAuthored = false,
  });

  final String label;
  final _StageGlyph glyph;
  final String? durationText;
  // True for a sub-row rendered from the factory's own AgentEvidence
  // (a per-round evidence summary), rendered in normal body text --
  // false for the existing worker-relayed round/agent-note lines, which
  // stay in their current secondary style (untrusted, display-only).
  final bool factoryAuthored;
}

class _RoundInfo {
  _RoundInfo(this.round, this.maxRounds);

  final int round;
  final int maxRounds;
  DateTime? start;
  DateTime? end;
  String outcome = '';
  String detail = '';
}

List<_TimelineRow> _computeTimeline(
  List<ProgressEvent> events,
  DateTime now,
  Run run,
) {
  final byStage = <String, List<ProgressEvent>>{};
  for (final event in events) {
    if (event.source != 'factory') continue;
    byStage.putIfAbsent(event.stage, () => []).add(event);
  }
  final finished = (byStage['finished'] ?? const []).any(
    (e) => e.event == 'end',
  );
  return [
    for (final stage in _stageOrder)
      // A run launched with no -reference-oracle-dir never visits these
      // two oracle-commit stages -- omit them rather than showing a
      // pending/skipped row for a stage this run will never reach (C6,
      // operator demo, 2026-09-26). A run that DID emit events for one
      // (an older record, or a race with the field landing) still shows
      // it, so real evidence is never hidden.
      if (!(run.referenceOracleDir.isEmpty &&
          _optionalOracleStages.contains(stage) &&
          (byStage[stage]?.isEmpty ?? true)))
        if (stage == 'build')
          _buildStageRow(events, finished, now, run)
        else if (stage == 'gate')
          _gateRow(byStage['gate'] ?? const [], finished, now, run)
        else
          _simpleStageRow(stage, byStage[stage] ?? const [], finished, now),
  ];
}

/// The two oracle-commit timeline stages that only ever run for a request
/// launched with `-draft-oracles` (a non-empty Run.referenceOracleDir) --
/// see _computeTimeline's own doc comment.
const _optionalOracleStages = {'commit_oracles', 'post_oracle_commit_verify'};

_TimelineRow _simpleStageRow(
  String stage,
  List<ProgressEvent> events,
  bool finished,
  DateTime now,
) {
  final start = _firstStart(events);
  final end = _lastEnd(events);
  final outcome = _endOutcome(events);
  final glyph = _glyphFor(
    start: start,
    end: end,
    outcome: outcome,
    finished: finished,
  );
  return _TimelineRow(
    rowKey: stage,
    label: _stageLabels[stage]!,
    glyph: glyph,
    durationText: _durationText(start, end, now, glyph),
  );
}

_TimelineRow _gateRow(
  List<ProgressEvent> events,
  bool finished,
  DateTime now,
  Run run,
) {
  final byGate = <String, List<ProgressEvent>>{};
  final order = <String>[];
  for (final event in events) {
    final name = event.detail.isEmpty ? '(unnamed gate)' : event.detail;
    if (!byGate.containsKey(name)) order.add(name);
    byGate.putIfAbsent(name, () => []).add(event);
  }
  // No named-gate progress events (no lint/security_audit/etc. configured)
  // does NOT mean no gates ran: the built-in policy gates (canonical_verify,
  // diff_scope, ...) are always evaluated but never emit their own `gate`
  // progress events (found live, 2026-09-26 operator walk) -- falling
  // through to _overallGlyph on an empty subRows list here read as
  // "skipped", which looked like the factory skipped its gates entirely.
  // The run's own recorded gate_results is the source of truth for those
  // checks, so use it whenever the feed has nothing to show.
  if (order.isEmpty && run.gateResults.isNotEmpty) {
    return _gateRowFromResults(run.gateResults);
  }
  final subRows = [
    for (final name in order) _subRowFor(name, byGate[name]!, finished, now),
  ];
  return _TimelineRow(
    rowKey: 'gate',
    label: _stageLabels['gate']!,
    glyph: _overallGlyph(subRows.map((r) => r.glyph), finished),
    subRows: subRows,
  );
}

/// Builds the Gates row directly from `run.gate_results` (the built-in
/// policy gates' own recorded evidence) when the progress feed has no
/// named-gate events to show -- see [_gateRow]'s doc comment.
_TimelineRow _gateRowFromResults(List<GateResult> results) {
  final failed = results.where((g) => !g.passed).toList();
  final glyph = failed.isEmpty ? _StageGlyph.passed : _StageGlyph.failed;
  final subtitle = failed.isEmpty
      ? '${results.length} passed: '
            '${results.map((g) => g.check).join(', ')}'
      : '${failed.length} failed of ${results.length}: '
            '${results.map((g) => '${g.check} (${g.passed ? 'pass' : 'fail'})').join(', ')}';
  return _TimelineRow(
    rowKey: 'gate',
    label: _stageLabels['gate']!,
    glyph: glyph,
    subtitle: subtitle,
  );
}

_TimelineSubRow _subRowFor(
  String label,
  List<ProgressEvent> events,
  bool finished,
  DateTime now,
) {
  final start = _firstStart(events);
  final end = _lastEnd(events);
  final outcome = _endOutcome(events);
  final glyph = _glyphFor(
    start: start,
    end: end,
    outcome: outcome,
    finished: finished,
  );
  return _TimelineSubRow(
    label: label,
    glyph: glyph,
    durationText: _durationText(start, end, now, glyph),
  );
}

_TimelineRow _buildStageRow(
  List<ProgressEvent> allEvents,
  bool finished,
  DateTime now,
  Run run,
) {
  final factoryEvents = allEvents
      .where((e) => e.source == 'factory' && e.stage == 'build')
      .toList();
  final start = _firstStart(factoryEvents);
  final end = _lastEnd(factoryEvents);
  final outcome = _endOutcome(factoryEvents);
  final glyph = _glyphFor(
    start: start,
    end: end,
    outcome: outcome,
    finished: finished,
  );

  // Worker rounds, in first-seen order (see progress-contract.md's own
  // "Worker stages" section for the round start/end/agent-note shape).
  final rounds = <int, _RoundInfo>{};
  final order = <int>[];
  for (final event in allEvents) {
    if (event.source != 'worker' || event.stage != 'round') continue;
    final info = rounds.putIfAbsent(event.round, () {
      order.add(event.round);
      return _RoundInfo(event.round, event.maxRounds);
    });
    if (event.event == 'start') {
      info.start = event.ts;
    } else if (event.event == 'end') {
      info.end = event.ts;
      info.outcome = event.outcome;
      info.detail = event.detail;
    }
  }
  // A terminal run's own evidence rounds (below) already carry every
  // worker round's index as a more informative "Round N · outcome ·
  // tokens · duration" line -- rendering the worker-relayed "Round N of M
  // · outcome" line for the same index too was a visible duplicate.
  // Only rounds evidence hasn't recorded (a still-running build,
  // or evidence that predates or is missing a round for some reason)
  // fall back to the worker-relayed line.
  final evidenceRoundIndexes = (run.agentEvidence?.rounds ?? const [])
      .map((r) => r.index)
      .toSet();
  final subRows = [
    ..._evidenceRoundSubRows(run),
    for (final round in order)
      if (!evidenceRoundIndexes.contains(round))
        _roundSubRow(rounds[round]!, now),
  ];

  // Agent notes under the current (latest) round only -- last 8, newest at
  // the bottom (see this class's own doc comment for why these render as
  // plain, untrusted text).
  final currentRound = order.isNotEmpty ? order.last : null;
  final notes = [
    for (final event in allEvents)
      if (event.source == 'worker' &&
          event.stage == 'agent' &&
          event.event == 'note' &&
          (currentRound == null || event.round == currentRound))
        event.detail,
  ];
  final lastNotes = notes.length > 8 ? notes.sublist(notes.length - 8) : notes;

  return _TimelineRow(
    rowKey: 'build',
    label: _stageLabels['build']!,
    glyph: glyph,
    durationText: _durationText(start, end, now, glyph),
    subRows: subRows,
    noteLines: lastNotes,
    subtitle: _latestBuildNote(factoryEvents),
  );
}

// The latest factory `build`/`note` event's detail (progress-contract.md's
// "Additions" one-line round summary, written once after evidence
// collection) -- "latest" in case a chained/reconciled run ever produced
// more than one, though today's call sites emit at most one per run.
String? _latestBuildNote(List<ProgressEvent> factoryEvents) {
  String? note;
  for (final event in factoryEvents) {
    if (event.event == 'note' && event.detail.isNotEmpty) note = event.detail;
  }
  return note;
}

// One sub-row per AgentEvidenceRound, factory-authored from BUILD_EVIDENCE.json
// -- only once the run is terminal and evidence was actually collected
// (see run.Run.AgentEvidence's own doc comment: recorded after policy
// gates decide the run's terminal state, so a still-running build has
// none of this yet).
List<_TimelineSubRow> _evidenceRoundSubRows(Run run) {
  if (!run.isTerminal) return const [];
  final rounds = run.agentEvidence?.rounds ?? const [];
  return [
    for (final rd in rounds)
      _TimelineSubRow(
        label: [
          'Round ${rd.index}',
          rd.outcome,
          if (rd.tokens > 0) '${formatTokenCount(rd.tokens)} tokens',
          '${rd.durationS.round()}s',
        ].join(' · '),
        glyph: rd.outcome == 'pass' ? _StageGlyph.passed : _StageGlyph.failed,
        factoryAuthored: true,
      ),
  ];
}

_TimelineSubRow _roundSubRow(_RoundInfo round, DateTime now) {
  final base = 'Round ${round.round} of ${round.maxRounds}';
  final label = round.detail.isNotEmpty
      ? '$base · ${round.detail}'
      : (round.outcome.isNotEmpty ? '$base · ${round.outcome}' : base);
  final glyph = round.end == null
      ? _StageGlyph.running
      : (round.outcome == 'fail' ? _StageGlyph.failed : _StageGlyph.passed);
  final durationText = round.start == null
      ? null
      : formatDuration((round.end ?? now).difference(round.start!));
  return _TimelineSubRow(
    label: label,
    glyph: glyph,
    durationText: durationText,
  );
}

DateTime? _firstStart(List<ProgressEvent> events) {
  for (final event in events) {
    if (event.event == 'start') return event.ts;
  }
  return null;
}

DateTime? _lastEnd(List<ProgressEvent> events) {
  DateTime? end;
  for (final event in events) {
    if (event.event == 'end') end = event.ts;
  }
  return end;
}

String _endOutcome(List<ProgressEvent> events) {
  var outcome = '';
  for (final event in events) {
    if (event.event == 'end') outcome = event.outcome;
  }
  return outcome;
}

/// Derives one stage/gate/round's display glyph from its own start/end
/// events. `finished` (the run's own progress feed has a
/// `stage: "finished", event: "end"` line) distinguishes "not yet reached"
/// ([_StageGlyph.pending]) from "the run ended without this stage ever
/// appearing" ([_StageGlyph.skipped]) -- progress-contract.md's own "a stage
/// that is skipped by config simply never appears".
_StageGlyph _glyphFor({
  required DateTime? start,
  required String outcome,
  required DateTime? end,
  required bool finished,
}) {
  if (end != null) {
    if (outcome == 'fail') return _StageGlyph.failed;
    // The `finished` stage's own outcome is the terminal run state
    // (accepted/quarantined/halted), not pass/fail -- only "accepted"
    // reads as passed.
    if (outcome == 'pass' || outcome == 'accepted' || outcome.isEmpty) {
      return _StageGlyph.passed;
    }
    return _StageGlyph.failed;
  }
  if (start != null) return _StageGlyph.running;
  return finished ? _StageGlyph.skipped : _StageGlyph.pending;
}

String? _durationText(
  DateTime? start,
  DateTime? end,
  DateTime now,
  _StageGlyph glyph,
) {
  if (start == null) return null;
  if (end != null) return formatDuration(end.difference(start));
  if (glyph == _StageGlyph.running) {
    return formatDuration(now.difference(start));
  }
  return null;
}

_StageGlyph _overallGlyph(Iterable<_StageGlyph> glyphs, bool finished) {
  final list = glyphs.toList();
  if (list.isEmpty) return finished ? _StageGlyph.skipped : _StageGlyph.pending;
  if (list.any((g) => g == _StageGlyph.failed)) return _StageGlyph.failed;
  if (list.any((g) => g == _StageGlyph.running)) return _StageGlyph.running;
  if (list.every((g) => g == _StageGlyph.passed)) return _StageGlyph.passed;
  return _StageGlyph.pending;
}

class _TimelineRowView extends StatelessWidget {
  const _TimelineRowView({required this.row});

  final _TimelineRow row;

  @override
  Widget build(BuildContext context) {
    // container: one screen-reader/Playwright node per stage, instead of
    // every row's text merging upward into a single Timeline-wide label.
    return Semantics(
      key: ValueKey('timeline-row-${row.rowKey}'),
      container: true,
      child: Padding(
        padding: const EdgeInsets.symmetric(vertical: 4),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                _GlyphIcon(glyph: row.glyph),
                const SizedBox(width: 8),
                Text(row.label, style: Theme.of(context).textTheme.bodyMedium),
                if (row.durationText != null) ...[
                  const SizedBox(width: 8),
                  Text(
                    row.durationText!,
                    style: Theme.of(context).textTheme.bodySmall,
                  ),
                ],
              ],
            ),
            if (row.subtitle != null)
              Padding(
                padding: const EdgeInsets.only(left: 32, top: 2),
                child: Text(
                  row.subtitle!,
                  style: Theme.of(context).textTheme.bodySmall,
                ),
              ),
            for (final sub in row.subRows)
              Padding(
                padding: const EdgeInsets.only(left: 32, top: 2),
                child: Row(
                  children: [
                    _GlyphIcon(glyph: sub.glyph, size: 14),
                    const SizedBox(width: 6),
                    Expanded(
                      child: Text(
                        sub.label,
                        style: sub.factoryAuthored
                            ? Theme.of(context).textTheme.bodyMedium
                            : Theme.of(context).textTheme.bodySmall,
                      ),
                    ),
                    if (sub.durationText != null)
                      Text(
                        sub.durationText!,
                        style: Theme.of(context).textTheme.bodySmall,
                      ),
                  ],
                ),
              ),
            if (row.noteLines.isNotEmpty)
              // Its own node: merged into the row, the SelectableText kept
              // the whole row out of the web accessibility tree.
              Semantics(
                container: true,
                child: Padding(
                  padding: const EdgeInsets.only(left: 32, top: 4),
                  child: SelectableText(
                    key: const ValueKey('timeline-agent-notes'),
                    row.noteLines.join('\n'),
                    style: const TextStyle(
                      fontFamily: 'monospace',
                      fontSize: 11,
                    ),
                  ),
                ),
              ),
          ],
        ),
      ),
    );
  }
}

class _GlyphIcon extends StatelessWidget {
  const _GlyphIcon({required this.glyph, this.size = 18});

  final _StageGlyph glyph;
  final double size;

  @override
  Widget build(BuildContext context) {
    return switch (glyph) {
      // Each glyph's label is its status word, so a stage's outcome is not
      // conveyed by colour alone.
      _StageGlyph.running => SizedBox.square(
        dimension: size,
        child: const CircularProgressIndicator(
          strokeWidth: 2,
          semanticsLabel: 'Running',
        ),
      ),
      _StageGlyph.passed => Icon(
        Icons.check_circle,
        size: size,
        color: Colors.green,
        semanticLabel: 'Passed',
      ),
      _StageGlyph.failed => Icon(
        Icons.error,
        size: size,
        color: Colors.red,
        semanticLabel: 'Failed',
      ),
      _StageGlyph.skipped => Icon(
        Icons.remove_circle_outline,
        size: size,
        color: Colors.grey,
        semanticLabel: 'Skipped',
      ),
      _StageGlyph.pending => Icon(
        Icons.circle_outlined,
        size: size,
        color: Colors.grey,
        semanticLabel: 'Pending',
      ),
    };
  }
}

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

class _OverrideValues {
  const _OverrideValues({
    required this.by,
    required this.reason,
    required this.state,
  });
  final String by;
  final String reason;
  final String state;
}

class _OverrideDialog extends StatefulWidget {
  const _OverrideDialog({this.initialReason});

  final String? initialReason;

  @override
  State<_OverrideDialog> createState() => _OverrideDialogState();
}

class _OverrideDialogState extends State<_OverrideDialog> {
  final _by = TextEditingController();
  late final _reason = TextEditingController(text: widget.initialReason ?? '');
  String _state = 'accepted';

  @override
  void dispose() {
    _by.dispose();
    _reason.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: const Text('Override quarantined run'),
    content: Column(
      mainAxisSize: MainAxisSize.min,
      children: [
        TextField(
          key: const ValueKey('override-by'),
          controller: _by,
          decoration: const InputDecoration(labelText: 'Operator'),
        ),
        TextField(
          key: const ValueKey('override-reason'),
          controller: _reason,
          decoration: const InputDecoration(labelText: 'Reason'),
        ),
        DropdownButtonFormField<String>(
          key: const ValueKey('override-state'),
          value: _state,
          decoration: const InputDecoration(labelText: 'New state'),
          items: const [
            DropdownMenuItem(value: 'accepted', child: Text('Accepted')),
            DropdownMenuItem(value: 'halted', child: Text('Halted')),
          ],
          onChanged: (value) {
            if (value != null) setState(() => _state = value);
          },
        ),
      ],
    ),
    actions: [
      TextButton(
        onPressed: () => Navigator.pop(context),
        child: const Text('Cancel'),
      ),
      FilledButton(
        onPressed: () {
          if (_by.text.trim().isEmpty || _reason.text.trim().isEmpty) return;
          Navigator.pop(
            context,
            _OverrideValues(
              by: _by.text.trim(),
              reason: _reason.text.trim(),
              state: _state,
            ),
          );
        },
        child: const Text('Apply'),
      ),
    ],
  );
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
          SizedBox(width: 180, child: Text(label)),
          Expanded(child: value),
        ],
      ),
    );
  }
}

class _TextField extends _Field {
  _TextField(String label, String value) : super(label, SelectableText(value));
}

/// One Attempts-section card: exit code/timing are shown up front,
/// the raw `docker run` argv is collapsed by default (it's long and
/// rarely what an operator wants first), and the log path is an "Open
/// log" action into the existing build-log pane rather than plain text.
/// Renders an attempt's session-config role, harness, model, and
/// thinking/effort evidence (P0e) as one line, e.g. "Role: execution ·
/// Harness: pifork · Model: gpt-5.6-luna · Thinking: max (sent: high)" --
/// null when the attempt carries none of role/harness/thinking/model at all (verify/full_suite_verify
/// attempts, and any run recorded before these fields existed), so an
/// older or non-model-calling attempt renders exactly as before this
/// line was added. The "(sent: X)" suffix appears only when the relay
/// actually observed a different effort than what this attempt's role
/// was told to use -- the same silent-clamp signal
/// `factoryd watch`/`status` surface as "(requested X, sent Y)".
String? _attemptModelLine(Attempt attempt) {
  if (attempt.role.isEmpty &&
      attempt.harness.isEmpty &&
      attempt.thinking.isEmpty &&
      attempt.relayWorkerModelId.isEmpty) {
    return null;
  }
  final parts = <String>[];
  if (attempt.role.isNotEmpty) {
    parts.add('Role: ${attempt.role}');
  }
  if (attempt.harness.isNotEmpty) {
    parts.add('Harness: ${attempt.harness}');
  }
  if (attempt.relayWorkerModelId.isNotEmpty) {
    parts.add('Model: ${attempt.relayWorkerModelId}');
  }
  if (attempt.thinking.isNotEmpty) {
    final effort = attempt.relayReasoningEffort;
    var thinking = 'Thinking: ${attempt.thinking}';
    // Compares expectedEffort (what Pi's own thinkingLevelMap translation
    // actually turns thinking into), not thinking directly: a model whose
    // thinkingLevelMap legitimately renames "max" to "xhigh" must not
    // read as a clamp just because thinking ("max") differs from effort
    // ("xhigh") -- found via review, the false positive comparing
    // thinking directly produced.
    final expected = attempt.expectedEffort;
    if (expected.isNotEmpty && effort.isNotEmpty && expected != effort) {
      thinking += ' (sent: $effort)';
    }
    if (attempt.relayReasoningEffortAnomaly) {
      thinking += ' (anomaly)';
    }
    parts.add(thinking);
  }
  return parts.join(' · ');
}

class _AttemptCard extends StatelessWidget {
  const _AttemptCard({required this.attempt, required this.onOpenLog});

  final Attempt attempt;
  final VoidCallback onOpenLog;

  @override
  Widget build(BuildContext context) {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              attempt.kind.isEmpty ? 'Attempt' : 'Attempt · ${attempt.kind}',
              style: Theme.of(context).textTheme.titleMedium,
            ),
            const SizedBox(height: 4),
            SelectableText(attemptExitText(attempt)),
            SelectableText(
              'Started: ${formatLocalTimestamp(attempt.startedAt)}',
            ),
            SelectableText(
              'Finished: ${formatLocalTimestamp(attempt.finishedAt)}',
            ),
            if (_attemptModelLine(attempt) case final line?)
              SelectableText(line),
            Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Expanded(child: SelectableText('Log: ${attempt.logPath}')),
                TextButton(
                  key: ValueKey('attempt-open-log-button-${attempt.startedAt}'),
                  onPressed: onOpenLog,
                  child: const Text('Open log'),
                ),
              ],
            ),
            Theme(
              data: Theme.of(
                context,
              ).copyWith(dividerColor: Colors.transparent),
              child: ExpansionTile(
                key: ValueKey('attempt-command-tile-${attempt.startedAt}'),
                tilePadding: EdgeInsets.zero,
                title: const Text('Command'),
                children: [
                  Align(
                    alignment: Alignment.centerLeft,
                    child: SelectableText(
                      attempt.command.join(' '),
                      style: const TextStyle(
                        fontFamily: 'monospace',
                        fontSize: 12,
                      ),
                    ),
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class _EvidenceCard extends StatelessWidget {
  const _EvidenceCard({required this.title, required this.lines});

  final String title;
  final List<String> lines;

  @override
  Widget build(BuildContext context) {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(title, style: Theme.of(context).textTheme.titleMedium),
            const SizedBox(height: 4),
            for (final line in lines) SelectableText(line),
          ],
        ),
      ),
    );
  }
}
