import 'package:flutter/material.dart';

import 'api_client.dart';
import 'models.dart';
import 'request_cost.dart';
import 'status.dart';

/// Cross-project operations view: every known project's quarantine
/// breakdown, override rate, and kill-switch state in one screen. See the
/// fable adoption review's own recommendation 5 -- at real organizational
/// scale this is the platform team's own primary screen, and its
/// differentiator over a commodity ops dashboard is showing *why* a run
/// was refused, from
/// each project's own recorded evidence (ProjectStats' own
/// quarantined-by-cause breakdown), not a log tail.
///
/// Deliberately reuses GET /projects, GET /projects/{project}/stats, and
/// GET /projects/{project}/release -- every fact this screen shows
/// already has its own single-project console screen
/// (ProjectStatsScreen/ProjectReleaseScreen); this one is purely an
/// aggregation over all of them, not a new backend concept.
///
/// Strictly observability, same as every other release-adjacent screen in
/// this console: nothing here mutates anything.
class OpsScreen extends StatefulWidget {
  const OpsScreen({required this.api, super.key});

  final RunApi api;

  @override
  State<OpsScreen> createState() => _OpsScreenState();
}

class _ProjectOpsRow {
  const _ProjectOpsRow({
    required this.summary,
    required this.stats,
    required this.release,
  });

  final ProjectSummary summary;
  final ProjectStats? stats;
  final ProjectReleaseView? release;
}

class _OpsRowsResult {
  const _OpsRowsResult(this.rows, this.startTokenFailure);

  final List<_ProjectOpsRow> rows;
  // True when at least one row's per-project stats/release fetch failed
  // with 401/403 -- both are start-token-gated routes (authorizeStart:
  // GET /projects/{project}/stats and /release), so a stale/missing start
  // token, not a broken project, is the likely cause. Surfaced once as a
  // banner (see build below) rather than repeating the guidance on every
  // affected card.
  final bool startTokenFailure;
}

class _OpsScreenState extends State<OpsScreen> {
  late Future<_OpsRowsResult> _rows;

  @override
  void initState() {
    super.initState();
    _rows = _load();
  }

  Future<_OpsRowsResult> _load() async {
    final projects = await widget.api.listProjects();
    final rows = <_ProjectOpsRow>[];
    var startTokenFailure = false;
    for (final summary in projects) {
      // Sequential, not Future.wait: this screen can list many projects,
      // and a slow/unreachable one (a project id this token isn't scoped
      // to, say) must not sink the whole page behind Future.wait's own
      // all-or-nothing failure -- each project's own stats/release
      // failure is caught and shown as "unavailable" for that row alone.
      ProjectStats? stats;
      ProjectReleaseView? release;
      try {
        stats = await widget.api.getProjectStats(summary.project);
      } on Object catch (error) {
        stats = null;
        if (_isAuthFailure(error)) startTokenFailure = true;
      }
      try {
        release = await widget.api.getProjectRelease(summary.project);
      } on Object catch (error) {
        release = null;
        if (_isAuthFailure(error)) startTokenFailure = true;
      }
      rows.add(
        _ProjectOpsRow(summary: summary, stats: stats, release: release),
      );
    }
    return _OpsRowsResult(rows, startTokenFailure);
  }

  static bool _isAuthFailure(Object error) =>
      error is RunApiException &&
      (error.statusCode == 401 || error.statusCode == 403);

  void _reload() {
    setState(() {
      _rows = _load();
    });
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Operations'),
        actions: [
          IconButton(
            onPressed: _reload,
            tooltip: 'Refresh',
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: FutureBuilder<_OpsRowsResult>(
        future: _rows,
        builder: (context, snapshot) {
          if (snapshot.connectionState != ConnectionState.done) {
            return const Center(child: CircularProgressIndicator());
          }
          if (snapshot.hasError) {
            return Center(
              child: Text(
                'Could not load operations view: ${snapshot.error}',
                key: const ValueKey('ops-error'),
              ),
            );
          }
          final result = snapshot.data!;
          final rows = result.rows;
          if (rows.isEmpty) {
            return const Center(child: Text('No projects recorded yet.'));
          }
          return Column(
            children: [
              if (result.startTokenFailure)
                Container(
                  key: const ValueKey('ops-start-token-banner'),
                  width: double.infinity,
                  padding: const EdgeInsets.all(12),
                  color: Theme.of(context).colorScheme.errorContainer,
                  child: Text(
                    'One or more projects\' stats/release could not be '
                    'read (401/403). Open the console link `factoryd '
                    'serve` printed in its own log/terminal output just '
                    'now (ends in `#t=...`) -- the start token changes '
                    'every restart.',
                    style: TextStyle(
                      color: Theme.of(context).colorScheme.onErrorContainer,
                    ),
                  ),
                ),
              Expanded(
                child: ListView.builder(
                  itemCount: rows.length,
                  itemBuilder: (context, index) =>
                      _ProjectOpsCard(row: rows[index]),
                ),
              ),
            ],
          );
        },
      ),
    );
  }
}

class _ProjectOpsCard extends StatelessWidget {
  const _ProjectOpsCard({required this.row});

  final _ProjectOpsRow row;

  @override
  Widget build(BuildContext context) {
    final stats = row.stats;
    // row.release is null when that project's own release fetch failed
    // (authorization, network, a backend error) -- coercing that to
    // "false" would render an unavailable kill switch as clear, which on
    // an operations screen is exactly the wrong direction to fail toward
    // (found via Codex review, PR #64): an operator seeing "clear" must
    // be able to trust the switch was actually confirmed off, not that
    // its state simply couldn't be read. A third, explicit "unknown"
    // chip keeps that distinction visible instead of collapsing it.
    final engaged = row.release?.killSwitch.engaged;
    return Card(
      margin: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
      child: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Expanded(
                  child: Text(
                    row.summary.project,
                    style: Theme.of(context).textTheme.titleMedium,
                  ),
                ),
                KillSwitchChip(
                  key: ValueKey(
                    engaged == null
                        ? 'ops-kill-switch-unknown-${row.summary.project}'
                        : engaged
                        ? 'ops-kill-switch-engaged-${row.summary.project}'
                        : 'ops-kill-switch-disengaged-${row.summary.project}',
                  ),
                  engaged: engaged,
                ),
              ],
            ),
            const SizedBox(height: 8),
            if (stats == null)
              const SelectableText('Stats unavailable for this project.')
            else ...[
              SelectableText(
                'Accepted ${stats.accepted} / ${stats.totalRuns} runs'
                '${stats.overrideRatePercent != null ? " -- ${stats.overrideRatePercent}% via override" : ""}',
              ),
              if (stats.quarantinedByCause.isNotEmpty)
                SelectableText(
                  'Quarantined by cause: ${stats.quarantinedByCause.entries.map((e) => "${e.key} (${e.value})").join(", ")}',
                ),
              if (stats.medianAcceptedTokens != null)
                SelectableText(
                  'Median accepted: '
                  '${formatMedianAcceptedTokens(stats.medianAcceptedTokens)}',
                ),
            ],
          ],
        ),
      ),
    );
  }
}
