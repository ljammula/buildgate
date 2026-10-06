import 'dart:async';

import 'package:flutter/material.dart';

import 'api_client.dart';
import 'elapsed.dart';
import 'error_display.dart';
import 'models.dart';
import 'ops_screen.dart';
import 'project_list_screen.dart';
import 'project_release_screen.dart';
import 'project_stats_screen.dart';
import 'run_detail_screen.dart';
import 'status.dart';

/// How often this screen re-polls GET /runs on its own,
/// without an operator having to hit the manual refresh button — found via
/// the console-observability review, 2026-09-10: this screen previously
/// only ever reloaded on an explicit tap, so an operator leaving it open
/// while the factory built a ticket saw a stale list until they
/// remembered to refresh it themselves.
const runListAutoRefreshInterval = Duration(seconds: 5);

class RunListScreen extends StatefulWidget {
  const RunListScreen({required this.api, super.key});

  final RunApi api;

  @override
  State<RunListScreen> createState() => _RunListScreenState();
}

class _RunListScreenState extends State<RunListScreen> {
  // Nullable-value/nullable-error pairs, not a Future rebuilt on every
  // reload -- the same stale-data pattern release_screen.dart already
  // establishes (see its own doc comment): a transient refresh failure
  // must not replace an already-loaded list with an error screen, since
  // the last successfully loaded data is still meaningful, not noise to
  // clear (found via adversarial review of PR #111, should-fix 3).
  List<Run>? _runs;
  Object? _runsError;
  // requestId -> RequestSummary, for showing a run's request title
  // instead of only its raw ticket id. Best-effort only: a
  // /requests fetch failure is tolerated silently by _loadRequests below
  // (it just leaves the previous map, or null, in place) rather than
  // blocking or blanking the runs list itself -- unlike runs, this
  // is a pure enhancement over the plain ticket-id-as-title rendering,
  // not something an operator needs surfaced as "refresh failed".
  Map<String, RequestSummary>? _requestsById;
  bool _loading = false;
  Timer? _autoRefresh;

  @override
  void initState() {
    super.initState();
    _load();
    _autoRefresh = Timer.periodic(runListAutoRefreshInterval, (_) => _load());
  }

  @override
  void dispose() {
    _autoRefresh?.cancel();
    super.dispose();
  }

  // Loads runs and requests independently, each keeping its
  // own last successfully loaded value on failure -- one endpoint
  // erroring must never blank data the others already loaded
  // successfully.
  Future<void> _load() async {
    setState(() => _loading = true);
    await Future.wait([_loadRuns(), _loadRequests()]);
    if (mounted) setState(() => _loading = false);
  }

  Future<void> _loadRuns() async {
    try {
      final runs = await widget.api.listRuns();
      if (mounted) {
        setState(() {
          _runs = runs;
          _runsError = null;
        });
      }
    } on Object catch (error) {
      if (mounted) setState(() => _runsError = error);
    }
  }

  Future<void> _loadRequests() async {
    try {
      final requests = await widget.api.listRequests();
      if (mounted) {
        setState(() {
          _requestsById = {for (final r in requests) r.id: r};
        });
      }
    } on Object catch (_) {
      // Silently tolerated -- see _requestsById's own doc comment.
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Factory runs'),
        actions: [
          TextButton.icon(
            key: const ValueKey('new-run-button'),
            onPressed: () {
              Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) => ProjectListScreen(api: widget.api),
                ),
              );
            },
            icon: const Icon(Icons.add),
            label: const Text('New run'),
          ),
          // Reaches a project's kill-switch state/history by project id
          // alone — the console surface a project with no runs otherwise
          // has no way to reach (see ProjectReleaseScreen's doc comment).
          IconButton(
            key: const ValueKey('project-release-button'),
            onPressed: () {
              Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) => ProjectReleaseScreen(api: widget.api),
                ),
              );
            },
            tooltip: 'Project release',
            icon: const Icon(Icons.shield_outlined),
          ),
          // Reaches a project's acceptance/override-rate figures by project
          // id alone, same reasoning as the release button next to it (see
          // ProjectStatsScreen's own doc comment).
          IconButton(
            key: const ValueKey('project-stats-button'),
            onPressed: () {
              Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) => ProjectStatsScreen(api: widget.api),
                ),
              );
            },
            tooltip: 'Project stats',
            icon: const Icon(Icons.bar_chart_outlined),
          ),
          // Cross-project operations view -- see OpsScreen's own doc
          // comment.
          IconButton(
            key: const ValueKey('ops-button'),
            onPressed: () {
              Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) => OpsScreen(api: widget.api),
                ),
              );
            },
            tooltip: 'Operations',
            icon: const Icon(Icons.dashboard_outlined),
          ),
          IconButton(
            key: const ValueKey('refresh-button'),
            // _loading-gated, same as release_screen's own refresh button
            // (found via Codex review there): left ungated, a second tap
            // while a request is still in flight could start a concurrent
            // load whose out-of-order response overwrites a newer
            // snapshot with a stale one.
            onPressed: _loading ? null : _load,
            tooltip: 'Refresh',
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: _buildBody(context),
    );
  }

  Widget _buildBody(BuildContext context) {
    final runs = _runs;
    if (runs == null) {
      final error = _runsError;
      if (error != null) {
        return Center(
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: ErrorCallout(error: error),
          ),
        );
      }
      return const Center(child: CircularProgressIndicator());
    }
    final staleError = _runsError;

    return Column(
      children: [
        if (staleError != null)
          MaterialBanner(
            key: const ValueKey('run-list-stale-banner'),
            content: Text(
              'Showing the last successfully loaded data -- '
              'refresh failed: $staleError',
            ),
            actions: [
              TextButton(
                onPressed: _loading ? null : _load,
                child: const Text('Retry'),
              ),
            ],
          ),
        Expanded(
          child: runs.isEmpty
              ? const Center(child: Text('No runs found.'))
              : ListView.separated(
                  itemCount: runs.length,
                  separatorBuilder: (_, _) => const Divider(height: 1),
                  itemBuilder: (context, index) {
                    final run = runs[index];
                    // Elapsed since createdAt for a still-running run, or
                    // the wall-clock duration to updatedAt once terminal --
                    // Phase 4's follow-along console: an operator scanning
                    // this list for "what's taking a while" previously had
                    // only raw created/updated timestamps to do that math
                    // themselves.
                    // Quarantined counts as terminal for this display-only
                    // elapsed/last-activity calculation -- see
                    // Run.isTerminalForDisplay's own doc comment for why
                    // this differs from Run.isTerminal, which a
                    // quarantined-run screen must keep polling against.
                    final elapsed = elapsedBetween(
                      run.createdAt,
                      run.isTerminalForDisplay ? run.updatedAt : null,
                    );
                    // "Silence is a bug" (progress-contract.md, 2026-09-18):
                    // a non-terminal run's own current stage and how long
                    // since its last progress line, so an operator does not
                    // have to open the run to tell "still working on
                    // verify" apart from "nothing has happened in ten
                    // minutes".
                    final lastProgressAt = run.lastProgressAt;
                    final progressLine = run.isTerminalForDisplay
                        ? ''
                        : ' · ${run.currentStage?.isNotEmpty ?? false ? run.currentStage : 'starting'}'
                              ' · last activity '
                              '${formatDuration(elapsedBetween(lastProgressAt ?? run.createdAt, null))} ago';
                    // The run's own request title in place of the raw
                    // ticket id when one is known -- a run started
                    // directly (no requestId, or one this /requests fetch
                    // didn't resolve) keeps the ticket-id-as-title
                    // rendering unchanged.
                    final request = run.requestId.isEmpty
                        ? null
                        : _requestsById?[run.requestId];
                    return ListTile(
                      key: ValueKey(run.id),
                      title: Text(request?.title ?? run.ticket),
                      subtitle: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          if (request != null)
                            Text(
                              'Ticket ${run.ticket}',
                              style: Theme.of(context).textTheme.bodySmall,
                            ),
                          Text(
                            '${run.projectPath} · Elapsed ${formatDuration(elapsed)}'
                            '$progressLine'
                            '\nRun ID: ${run.id} · Created '
                            '${formatLocalTimestamp(run.createdAt)}',
                          ),
                          StallChip(run: run),
                        ],
                      ),
                      trailing: StateBadge(state: run.state),
                      isThreeLine: true,
                      onTap: () {
                        Navigator.of(context).push(
                          MaterialPageRoute<void>(
                            builder: (_) =>
                                RunDetailScreen(api: widget.api, runId: run.id),
                          ),
                        );
                      },
                    );
                  },
                ),
        ),
      ],
    );
  }
}

class StateBadge extends StatelessWidget {
  const StateBadge({required this.state, super.key});

  final String state;

  @override
  Widget build(BuildContext context) {
    // Display labels are unchanged; color comes from status.dart's shared
    // vocabulary, which now also maps 'ready' and
    // 'verifying' -- any state string
    // still missing from that table falls through to Status.unknown/grey,
    // rendered, never hidden, per status.dart's own doc comment.
    final label = switch (state) {
      'accepted' => 'Accepted',
      'halted' => 'Halted',
      'quarantined' => 'Quarantined',
      'ready' => 'Ready',
      'slice_running' => 'Slice running',
      'verifying' => 'Verifying',
      'queued' => 'Queued',
      _ => state,
    };
    return StatusChip(
      key: ValueKey('state-$state'),
      status: statusForToken(state),
      label: label,
    );
  }
}
