import 'package:flutter/material.dart';

import 'api_client.dart';
import 'elapsed.dart';
import 'error_display.dart';
import 'models.dart';
import 'status.dart';

/// The project-level counterpart to ReleaseScreen: a project's kill switch
/// state and history, addressed by project id alone rather than derived
/// from a run. Closes the gap ReleaseScreen alone left (CLAIMS.md, Phase 1):
/// a project whose kill switch is engaged but which has no runs had no
/// console surface at all, since GET /projects only lists projects
/// aggregated out of existing run records. An operator reaches this screen
/// by typing the same project id `factoryd kill-switch -project` already
/// takes — this screen needs no run, and no project list entry, to exist.
///
/// Strictly observability, same as ReleaseScreen: no control here engages
/// or disengages the kill switch (CLI-only, deliberately, so hitting it
/// never depends on a healthy `factoryd serve`).
class ProjectReleaseScreen extends StatefulWidget {
  const ProjectReleaseScreen({
    required this.api,
    this.initialProject,
    super.key,
  });

  final RunApi api;

  /// Pre-fills the project id field and triggers an immediate lookup, so
  /// `/projects/{p}/release` is addressable by URL -- null (the default)
  /// keeps this screen's pre-existing manual-entry
  /// behavior for a caller that reaches it without already knowing which
  /// project.
  final String? initialProject;

  @override
  State<ProjectReleaseScreen> createState() => _ProjectReleaseScreenState();
}

class _ProjectReleaseScreenState extends State<ProjectReleaseScreen> {
  late final _controller = TextEditingController(
    text: widget.initialProject ?? '',
  );
  ProjectReleaseView? _release;
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
    // Clear any previously loaded project's kill-switch state before this
    // lookup resolves (found via a real GitHub Codex App review, P2): a
    // failed lookup for a *different* project used to leave the prior
    // project's data on screen alongside the new error, so this
    // safety-oriented screen could show an operator project B's error next
    // to project A's still-rendered "Disengaged" chip -- indistinguishable
    // from a genuine, current B is disengaged. Every Load press is a fresh,
    // explicit lookup (this screen has no same-project refresh action), so
    // there is no "stale but still meaningful" case worth retaining here
    // the way ReleaseScreen's own refresh does for the same run.
    setState(() {
      _loading = true;
      _release = null;
      _error = null;
    });
    try {
      final release = await widget.api.getProjectRelease(project);
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
      appBar: AppBar(title: const Text('Project release')),
      body: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Expanded(
                  child: TextField(
                    key: const ValueKey('project-release-input'),
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
                  key: const ValueKey('project-release-load-button'),
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
                  key: const ValueKey('project-release-error'),
                  error: error,
                  // GET /projects/{project}/release is start-token-gated
                  // (authorizeStart).
                  startClass: true,
                ),
              ),
            if (release != null)
              Expanded(child: _ProjectReleaseSections(release: release)),
          ],
        ),
      ),
    );
  }
}

class _ProjectReleaseSections extends StatelessWidget {
  const _ProjectReleaseSections({required this.release});

  final ProjectReleaseView release;

  @override
  Widget build(BuildContext context) {
    final killSwitch = release.killSwitch;
    return ListView(
      children: [
        _Field('Project', SelectableText(release.project)),
        _Field(
          'State',
          KillSwitchChip(
            key: ValueKey(
              killSwitch.engaged
                  ? 'project-kill-switch-engaged'
                  : 'project-kill-switch-disengaged',
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
        const SizedBox(height: 8),
        Text('History', style: Theme.of(context).textTheme.titleMedium),
        const SizedBox(height: 8),
        if (killSwitch.history.isEmpty)
          const SelectableText(
            'Never engaged. This project has no recorded transitions.',
          )
        else
          for (final transition in killSwitch.history)
            _TransitionCard(transition: transition),
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

// Mirrors ReleaseScreen's own private _Field layout primitive; that one
// stays private to its file, so this screen keeps a matching copy rather
// than widening its public surface.
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
