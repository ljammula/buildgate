import 'package:flutter/material.dart';

import 'api_client.dart';
import 'elapsed.dart';
import 'error_display.dart';
import 'models.dart';
import 'status.dart';

/// The plan's own "release" console screen: what the factory decided about
/// releasing one run, why, and the current state of the project kill switch
/// that decision was evaluated against.
///
/// Strictly observability. Nothing here acts on an allowed decision — no
/// merge, push, or deploy is wired to one anywhere in this system — and this
/// screen deliberately offers no control to engage or disengage the kill
/// switch: that stays CLI-only (`factoryd kill-switch`) so reaching for it
/// never depends on a healthy `factoryd serve` (see CLAIMS.md). Surfacing
/// its state read-only does not weaken that.
class ReleaseScreen extends StatefulWidget {
  const ReleaseScreen({required this.api, required this.runId, super.key});

  final RunApi api;
  final String runId;

  @override
  State<ReleaseScreen> createState() => _ReleaseScreenState();
}

class _ReleaseScreenState extends State<ReleaseScreen> {
  ReleaseView? _release;
  Object? _error;
  bool _loading = false;

  @override
  void initState() {
    super.initState();
    _load();
  }

  // Found via Codex review of this PR: _load previously ran only once, from
  // initState, so a screen left open across a run's acceptance or a
  // kill-switch transition kept showing whatever it first loaded --
  // including "No decision recorded" for a run that has since been
  // accepted. Reachable via the refresh action below; on error or success
  // the last successfully loaded release stays visible rather than being
  // replaced with a spinner or blanked, since a stale decision is still
  // meaningful recorded evidence, not noise to clear.
  Future<void> _load() async {
    setState(() => _loading = true);
    try {
      final release = await widget.api.getRunRelease(widget.runId);
      if (mounted) {
        setState(() {
          _release = release;
          _error = null;
        });
      }
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final release = _release;
    final error = _error;
    return Scaffold(
      appBar: AppBar(
        title: Text('Release — ${widget.runId}'),
        actions: [
          IconButton(
            key: const ValueKey('release-refresh-button'),
            onPressed: _loading ? null : _load,
            tooltip: 'Refresh',
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: switch ((release, error)) {
        // GET /runs/{id}/release is start-token-gated (authorizeStart) --
        // see error_display.dart's own startClass doc comment.
        (null, final Object err) => Center(
          child: ErrorCallout(
            key: const ValueKey('release-error'),
            error: err,
            startClass: true,
            onRetry: _loading ? null : _load,
          ),
        ),
        (null, null) => const Center(child: CircularProgressIndicator()),
        (final ReleaseView value, final Object err) => Column(
          children: [
            // Found via Codex review of this PR: a refresh failure after an
            // earlier successful load used to be silently discarded here --
            // _error was set but this arm ignored it and rendered the
            // retained _release with no indication it might now be stale.
            // If an operator refreshes right after a kill-switch transition
            // and that request fails, the screen could keep presenting the
            // old switch state as current with no warning. This still
            // never blanks the retained data (it is still meaningful
            // recorded evidence), but it says plainly that the refresh
            // failed and what's shown may be stale.
            MaterialBanner(
              key: const ValueKey('release-stale-banner'),
              content: Text(
                'Showing the last successfully loaded data -- refresh '
                'failed: ${describeError(err, startClass: true).nextStep}',
              ),
              actions: [
                // _loading-gated like the app-bar refresh button (found via
                // Codex review): left ungated, a second tap while the first
                // request was still in flight could start a concurrent
                // _load, and an out-of-order response could overwrite a
                // newer snapshot with a stale one and silently clear this
                // banner.
                TextButton(
                  onPressed: _loading ? null : _load,
                  child: const Text('Retry'),
                ),
              ],
            ),
            Expanded(child: _ReleaseSections(release: value)),
          ],
        ),
        (final ReleaseView value, null) => _ReleaseSections(release: value),
      },
    );
  }
}

class _ReleaseSections extends StatelessWidget {
  const _ReleaseSections({required this.release});

  final ReleaseView release;

  @override
  Widget build(BuildContext context) {
    final decision = release.decision;
    final recordingFailure = release.recordingFailure;
    final killSwitch = release.killSwitch;
    return ListView(
      padding: const EdgeInsets.all(16),
      children: [
        _Section(
          title: 'Release decision',
          children: [
            _Field('Project', SelectableText(release.project)),
            if (recordingFailure != null) ...[
              // The run WAS evaluated, but the decision itself could not
              // be durably recorded (e.g. an unreadable/corrupted
              // kill-switch.json) — distinct from "no decision recorded"
              // below, which means recording was never attempted.
              _Field(
                'Decision',
                Chip(
                  key: const ValueKey('release-decision-recording-failed'),
                  label: Text(
                    'release decision could not be recorded: '
                    '${recordingFailure.error} -- fix and `factoryd retry`',
                  ),
                ),
              ),
              _Field('Failed at', LocalTimeText(recordingFailure.at)),
            ] else if (decision == null) ...[
              // Absence of a decision is never an allowed one: nothing
              // records a decision until a run is accepted.
              _Field(
                'Decision',
                const Chip(
                  key: ValueKey('release-decision-none'),
                  label: Text('No decision recorded'),
                ),
              ),
              _Field(
                'Why',
                const SelectableText(
                  'No release decision has been recorded for this run. One is '
                  'recorded only when a run is accepted.',
                ),
              ),
            ] else ...[
              _Field(
                'Decision',
                StatusChip(
                  key: ValueKey(
                    decision.allowed
                        ? 'release-decision-allowed'
                        : 'release-decision-denied',
                  ),
                  status: statusForReleaseDecision(allowed: decision.allowed),
                  label: decision.allowed ? 'Allowed' : 'Denied',
                ),
              ),
              _Field('Run ID', SelectableText(decision.runId)),
              _Field('Evaluated', SelectableText(decision.evaluatedAt)),
              if (decision.reasons.isEmpty)
                _Field('Reasons', const SelectableText('None recorded'))
              else
                for (final reason in decision.reasons)
                  _Field('Reason', SelectableText(reason)),
            ],
            // Stated on the screen itself, not only in this file's doc
            // comment: an operator reading "Allowed" must not infer that
            // anything then happens.
            _Field(
              'Effect',
              const SelectableText(
                'Recorded evidence only. No merge, push, or deploy is '
                'performed from this decision.',
              ),
            ),
          ],
        ),
        _Section(
          title: 'Project kill switch',
          children: [
            _Field(
              'State',
              KillSwitchChip(
                key: ValueKey(
                  killSwitch.engaged
                      ? 'kill-switch-engaged'
                      : 'kill-switch-disengaged',
                ),
                engaged: killSwitch.engaged,
              ),
            ),
            _Field(
              'Control',
              const SelectableText(
                'Engage and disengage from the command line '
                '(factoryd kill-switch). It is deliberately not a console '
                'action, so hitting it never depends on a healthy '
                'factoryd serve.',
              ),
            ),
            if (killSwitch.history.isEmpty)
              _Field(
                'History',
                const SelectableText(
                  'Never engaged. This project has no recorded transitions.',
                ),
              )
            else
              for (final transition in killSwitch.history)
                _TransitionCard(transition: transition),
          ],
        ),
      ],
    );
  }
}

class _TransitionCard extends StatelessWidget {
  const _TransitionCard({required this.transition});

  final KillSwitchTransition transition;

  @override
  Widget build(BuildContext context) {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              transition.engaged ? 'Engaged' : 'Disengaged',
              style: Theme.of(context).textTheme.titleMedium,
            ),
            const SizedBox(height: 4),
            SelectableText('By: ${transition.by}'),
            SelectableText('At: ${formatLocalTimestamp(transition.at)}'),
            SelectableText('Reason: ${transition.reason}'),
          ],
        ),
      ),
    );
  }
}

// Mirrors RunDetailScreen's own _Section/_Field layout primitives; they are
// private to that screen, so this screen keeps its own matching pair rather
// than widening either file's public surface.
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
          SizedBox(width: 180, child: Text(label)),
          Expanded(child: value),
        ],
      ),
    );
  }
}
