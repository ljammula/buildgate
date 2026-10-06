import 'package:flutter/material.dart';

import 'api_client.dart';
import 'error_display.dart';
import 'models.dart';
import 'request_cost.dart';

/// A project's acceptance/override-rate and quarantine-cause figures, by
/// project id alone -- the console counterpart to `factoryd override-rate`
/// (CLI-only, repo-wide today), scoped per project. See the fable
/// adoption review's own recommendation 4: this is the number a team lead
/// decides whether to route real tickets here on, so it has to be visible
/// without reading a durable-run-record JSON file by hand.
///
/// Strictly observability, same as ProjectReleaseScreen: nothing here
/// mutates anything.
class ProjectStatsScreen extends StatefulWidget {
  const ProjectStatsScreen({required this.api, this.initialProject, super.key});

  final RunApi api;

  /// Pre-fills the project id field and triggers an immediate lookup
  /// (making `/projects/{p}/stats` addressable by URL, same pattern as
  /// ProjectReleaseScreen's own `initialProject`) -- null (the default)
  /// keeps this screen's pre-existing manual-entry behavior for a caller
  /// that reaches it without already knowing which project.
  final String? initialProject;

  @override
  State<ProjectStatsScreen> createState() => _ProjectStatsScreenState();
}

class _ProjectStatsScreenState extends State<ProjectStatsScreen> {
  late final _controller = TextEditingController(
    text: widget.initialProject ?? '',
  );
  ProjectStats? _stats;
  Object? _error;
  bool _loading = false;

  @override
  void initState() {
    super.initState();
    if (widget.initialProject != null && widget.initialProject!.isNotEmpty) {
      _load();
    }
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    final project = _controller.text.trim();
    if (project.isEmpty) return;
    // Same reasoning as ProjectReleaseScreen's own _load: clear the prior
    // project's figures before this lookup resolves, so a failed lookup
    // for a different project can never leave project A's numbers on
    // screen next to project B's error.
    setState(() {
      _loading = true;
      _stats = null;
      _error = null;
    });
    try {
      final stats = await widget.api.getProjectStats(project);
      if (mounted) {
        setState(() {
          _stats = stats;
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
    final stats = _stats;
    final error = _error;
    return Scaffold(
      appBar: AppBar(title: const Text('Project stats')),
      body: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Expanded(
                  child: TextField(
                    key: const ValueKey('project-stats-input'),
                    controller: _controller,
                    decoration: const InputDecoration(
                      labelText: 'Project id',
                      hintText: 'the id factoryd kill-switch -project takes',
                    ),
                    onSubmitted: (_) => _loading ? null : _load(),
                  ),
                ),
                const SizedBox(width: 12),
                FilledButton(
                  key: const ValueKey('project-stats-load-button'),
                  onPressed: _loading ? null : _load,
                  child: const Text('Load'),
                ),
              ],
            ),
            const SizedBox(height: 16),
            if (_loading) const LinearProgressIndicator(),
            if (error != null)
              Padding(
                padding: const EdgeInsets.only(top: 16),
                child: ErrorCallout(
                  key: const ValueKey('project-stats-error'),
                  error: error,
                  // GET /projects/{project}/stats is start-token-gated
                  // (authorizeStart).
                  startClass: true,
                ),
              ),
            if (stats != null)
              Expanded(child: _ProjectStatsSections(stats: stats)),
          ],
        ),
      ),
    );
  }
}

class _ProjectStatsSections extends StatelessWidget {
  const _ProjectStatsSections({required this.stats});

  final ProjectStats stats;

  @override
  Widget build(BuildContext context) {
    final overrideRate = stats.overrideRatePercent;
    final medianTokens = stats.medianAcceptedTokens;
    return ListView(
      children: [
        _Field('Project', SelectableText(stats.project)),
        _Field('Total runs', SelectableText('${stats.totalRuns}')),
        _Field('Accepted', SelectableText('${stats.accepted}')),
        _Field('Halted', SelectableText('${stats.halted}')),
        _Field(
          'Override rate (accepted)',
          SelectableText(
            key: const ValueKey('project-stats-override-rate'),
            overrideRate == null
                ? 'No accepted runs yet'
                : '$overrideRate% (${stats.acceptedViaOverride} of ${stats.accepted})',
          ),
        ),
        _Field(
          'Median accepted',
          SelectableText(
            key: const ValueKey('project-stats-median-cost'),
            medianTokens == null
                ? 'No accepted runs yet'
                : formatMedianAcceptedTokens(medianTokens),
          ),
        ),
        const SizedBox(height: 8),
        Text(
          'Quarantined by cause',
          style: Theme.of(context).textTheme.titleMedium,
        ),
        const SizedBox(height: 8),
        if (stats.quarantinedByCause.isEmpty)
          const SelectableText('No quarantined runs recorded.')
        else
          for (final entry in stats.quarantinedByCause.entries)
            _Field(entry.key, SelectableText('${entry.value}')),
      ],
    );
  }
}

// Mirrors ProjectReleaseScreen's own private _Field layout primitive; that
// one stays private to its file, so this screen keeps a matching copy
// rather than widening its public surface.
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
