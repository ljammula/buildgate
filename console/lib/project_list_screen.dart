import 'package:flutter/material.dart';

import 'api_client.dart';
import 'elapsed.dart';
import 'models.dart';
import 'new_run_screen.dart';

/// The console's own "project and repository selection" screen — the
/// first of the console's initial screens. Lists every project an
/// operator has already run against (derived from
/// existing run history, not a separately maintained list) so starting
/// another run against one doesn't require retyping its workspace/spec
/// paths, while a "Custom run" action still reaches a blank intake form for
/// a project that has never been run before.
class ProjectListScreen extends StatefulWidget {
  const ProjectListScreen({required this.api, super.key});

  final RunApi api;

  @override
  State<ProjectListScreen> createState() => _ProjectListScreenState();
}

class _ProjectListScreenState extends State<ProjectListScreen> {
  List<ProjectSummary>? _projects;
  Object? _error;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    setState(() => _error = null);
    try {
      final projects = await widget.api.listProjects();
      if (mounted) setState(() => _projects = projects);
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    }
  }

  void _startCustomRun() {
    Navigator.of(context).push(
      MaterialPageRoute<void>(builder: (_) => NewRunScreen(api: widget.api)),
    );
  }

  void _startRunForProject(ProjectSummary project) {
    Navigator.of(context).push(
      MaterialPageRoute<void>(
        builder: (_) => NewRunScreen(
          api: widget.api,
          initialWorkspacePath: project.workspacePath,
          initialSpecPath: project.specPath,
          initialRepository: project.repository,
        ),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    final projects = _projects;
    final error = _error;
    return Scaffold(
      appBar: AppBar(title: const Text('Projects')),
      floatingActionButton: FloatingActionButton.extended(
        key: const ValueKey('custom-run-button'),
        onPressed: _startCustomRun,
        icon: const Icon(Icons.add),
        label: const Text('Custom run'),
      ),
      body: switch ((projects, error)) {
        (null, final Object err) => Center(
          child: Text('Could not load projects: $err'),
        ),
        (null, null) => const Center(child: CircularProgressIndicator()),
        (final List<ProjectSummary> value, _) when value.isEmpty =>
          const Center(
            child: Text(
              'No projects yet. Start a custom run to create the first one.',
            ),
          ),
        (final List<ProjectSummary> value, _) => ListView.builder(
          itemCount: value.length,
          itemBuilder: (context, index) {
            final project = value[index];
            return ListTile(
              key: ValueKey('project-${project.projectPath}'),
              title: Text(project.projectPath),
              subtitle: Text(
                'kill-switch id: ${project.project} · '
                '${project.runCount} run${project.runCount == 1 ? '' : 's'} '
                '· last run ${formatLocalTimestamp(project.lastRunAt)}',
              ),
              trailing: const Icon(Icons.play_arrow),
              onTap: () => _startRunForProject(project),
            );
          },
        ),
      },
    );
  }
}
