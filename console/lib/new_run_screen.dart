import 'package:flutter/material.dart';

import 'api_client.dart';
import 'error_display.dart';
import 'models.dart';
import 'run_detail_screen.dart';

class NewRunScreen extends StatefulWidget {
  const NewRunScreen({
    required this.api,
    this.initialWorkspacePath,
    this.initialSpecPath,
    this.initialRepository,
    super.key,
  });

  final RunApi api;
  // Prefilled from a ProjectListScreen selection (see its own doc comment)
  // so an operator returning to a project they've already run against
  // doesn't have to retype paths already on file from a prior run —
  // ProjectPath and WorkspacePath are the same value by this codebase's
  // existing convention (cmd/factoryd sets ProjectPath to the workspace
  // path), so there is no separate "project path" field to prefill. All
  // null for a from-scratch/"Custom" run, same as before this screen took
  // any constructor arguments.
  final String? initialWorkspacePath;
  final String? initialSpecPath;
  // Found via review: without this, selecting a known project still left
  // Repository — required whenever a run needs the shared per-repository
  // task queue — to be retyped every time, defeating the point of
  // prefilling from a prior run at all.
  final String? initialRepository;

  @override
  State<NewRunScreen> createState() => _NewRunScreenState();
}

class _NewRunScreenState extends State<NewRunScreen> {
  final _formKey = GlobalKey<FormState>();
  late final _ticketController = TextEditingController();
  late final _workspaceController = TextEditingController(
    text: widget.initialWorkspacePath ?? '',
  );
  late final _specController = TextEditingController(
    text: widget.initialSpecPath ?? '',
  );
  late final _repositoryController = TextEditingController(
    text: widget.initialRepository ?? '',
  );
  final _temporalAddressController = TextEditingController();
  Object? _error;
  bool _starting = false;
  // _checkResult/_checkError are deliberately independent of _error/
  // _starting above: a project-setup check is a preview, not a run
  // attempt, so checking a project must never clear (or be cleared by)
  // the outcome of actually starting one, and the two actions' own
  // in-flight indicators must not fight over one shared flag.
  ProjectCheckResponse? _checkResult;
  Object? _checkError;
  bool _checkingProject = false;

  @override
  void dispose() {
    _ticketController.dispose();
    _workspaceController.dispose();
    _specController.dispose();
    _repositoryController.dispose();
    _temporalAddressController.dispose();
    super.dispose();
  }

  // _checkProjectSetup previews the same project-bootstrap preflight
  // _startRun would otherwise only discover was unsatisfied by actually
  // starting a run and watching it fail closed (see internal/api's
  // checkProject doc comment) -- an operator onboarding a project new to
  // this convention gets an itemized answer before committing to
  // anything. Only Workspace is required here, deliberately less than
  // _startRun's own full-form validation: an operator checking a project
  // has usually not decided on a Ticket yet, and the preflight itself
  // doesn't need Spec/Temporal address at all.
  Future<void> _checkProjectSetup() async {
    final workspace = _workspaceController.text.trim();
    if (workspace.isEmpty) {
      setState(() {
        _checkResult = null;
        _checkError = 'Workspace path is required to check a project';
      });
      return;
    }
    setState(() {
      _checkingProject = true;
      _checkError = null;
      _checkResult = null;
    });
    try {
      final result = await widget.api.checkProject(
        workspace: workspace,
        repository: _repositoryController.text.trim(),
        ticket: _ticketController.text.trim(),
      );
      if (mounted) setState(() => _checkResult = result);
    } on Object catch (error) {
      if (mounted) setState(() => _checkError = error);
    } finally {
      if (mounted) setState(() => _checkingProject = false);
    }
  }

  Future<void> _startRun() async {
    if (!_formKey.currentState!.validate()) return;
    setState(() {
      _starting = true;
      _error = null;
    });
    try {
      final run = await widget.api.startRun(
        ticket: _ticketController.text.trim(),
        workspace: _workspaceController.text.trim(),
        spec: _specController.text.trim(),
        repository: _repositoryController.text.trim(),
        temporalAddress: _temporalAddressController.text.trim(),
      );
      if (!mounted) return;
      await Navigator.of(context).push(
        MaterialPageRoute<void>(
          builder: (_) =>
              RunDetailScreen(api: widget.api, runId: run.id, initialRun: run),
        ),
      );
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    } finally {
      if (mounted) setState(() => _starting = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('New run')),
      body: Form(
        key: _formKey,
        child: ListView(
          padding: const EdgeInsets.all(16),
          children: [
            const Text(
              'Start a bounded run',
              style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600),
            ),
            const SizedBox(height: 8),
            const Text(
              'Provide the ticket and the repository execution locations. '
              'The run will appear in the detail view after factoryd accepts it.',
            ),
            const SizedBox(height: 20),
            _input(
              controller: _ticketController,
              label: 'Ticket',
              key: const ValueKey('new-run-ticket'),
            ),
            _input(
              controller: _workspaceController,
              label: 'Workspace path',
              // The project-bootstrap preflight (cmd/factoryd's
              // runProjectBootstrapCheck, mandatory unless the run itself
              // sets -skip-project-check) looks for spec/spec.md,
              // spec/contract.md, and ARCHITECTURE.md next to this path's
              // own PARENT directory, not inside it -- this is the single
              // most common cause of a rejected run against a project new
              // to this convention (found live, 2026-09-08). e.g. for
              // /repo/workspace, it expects /repo/spec/spec.md.
              hintText:
                  'A git checkout; its own parent directory must hold spec/spec.md, spec/contract.md, and ARCHITECTURE.md',
              key: const ValueKey('new-run-workspace'),
            ),
            _input(
              controller: _specController,
              label: 'Spec path',
              hintText: 'Usually <workspace path>/../spec/spec.md',
              key: const ValueKey('new-run-spec'),
            ),
            _input(
              controller: _repositoryController,
              label: 'Repository',
              key: const ValueKey('new-run-repository'),
            ),
            const SizedBox(height: 4),
            Align(
              alignment: Alignment.centerLeft,
              child: OutlinedButton.icon(
                key: const ValueKey('check-project-button'),
                onPressed: _checkingProject ? null : _checkProjectSetup,
                icon: _checkingProject
                    ? const SizedBox.square(
                        dimension: 16,
                        child: CircularProgressIndicator(strokeWidth: 2),
                      )
                    : const Icon(Icons.fact_check_outlined),
                label: Text(
                  _checkingProject ? 'Checking…' : 'Check project setup',
                ),
              ),
            ),
            if (_checkError case final error?) ...[
              const SizedBox(height: 8),
              ErrorCallout(
                key: const ValueKey('check-project-error'),
                error: error,
                // POST /projects/check is start-token-gated
                // (authorizeStart) -- see error_display.dart's own
                // startClass doc comment.
                startClass: true,
              ),
            ],
            if (_checkResult case final result?) ...[
              const SizedBox(height: 8),
              Text(
                result.passed
                    ? 'Project setup looks ready.'
                    : 'Project setup is not ready yet:',
                key: const ValueKey('check-project-summary'),
                style: TextStyle(
                  fontWeight: FontWeight.w600,
                  color: result.passed
                      ? null
                      : Theme.of(context).colorScheme.error,
                ),
              ),
              // Every declared check, not only the failing ones -- an
              // operator who just fixed one artifact wants to see the
              // others already passing, not only what's still wrong.
              for (final check in result.checks)
                Padding(
                  padding: const EdgeInsets.only(top: 4, left: 4),
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Icon(
                        check.passed ? Icons.check_circle : Icons.cancel,
                        size: 16,
                        color: check.passed
                            ? Colors.green
                            : Theme.of(context).colorScheme.error,
                      ),
                      const SizedBox(width: 6),
                      Expanded(
                        child: Text(
                          check.passed
                              ? '${check.check} (${check.path})'
                              : '${check.check} (${check.path}): '
                                    '${check.reasons.join("; ")}',
                        ),
                      ),
                    ],
                  ),
                ),
            ],
            const SizedBox(height: 12),
            _input(
              controller: _temporalAddressController,
              label: 'Temporal address',
              hintText: 'localhost:7233',
              key: const ValueKey('new-run-temporal-address'),
            ),
            if (_error case final error?) ...[
              const SizedBox(height: 12),
              // A RunApiException's own messageParts (see its doc comment)
              // renders each failing check on its own line -- e.g. a
              // rejected project-bootstrap preflight names every one of
              // product_spec_frozen/program_design_structure/
              // architecture_structure/ticket_structure that failed, which
              // used to arrive here as one unparsed, escaped JSON blob
              // (found via a real console live-validation run, 2026-09-08).
              // Any other error (a network failure, a malformed URL) has
              // no such structure and renders as the single line it always
              // did. A 401/403 is carved out of the bullet-list branch
              // below despite being a RunApiException -- it has no
              // per-check structure worth bulleting, and needs the
              // startClass guidance instead (POST /runs is
              // start-token-gated).
              if (error case final RunApiException apiError
                  when apiError.statusCode != 401 && apiError.statusCode != 403)
                Text(
                  'Could not start run:\n${apiError.messageParts.map((part) => '• $part').join('\n')}',
                  key: const ValueKey('new-run-error'),
                  style: TextStyle(color: Theme.of(context).colorScheme.error),
                )
              else
                ErrorCallout(
                  key: const ValueKey('new-run-error'),
                  error: error,
                  startClass: true,
                ),
            ],
            const SizedBox(height: 20),
            FilledButton.icon(
              key: const ValueKey('start-run-button'),
              onPressed: _starting ? null : _startRun,
              icon: _starting
                  ? const SizedBox.square(
                      dimension: 18,
                      child: CircularProgressIndicator(strokeWidth: 2),
                    )
                  : const Icon(Icons.play_arrow),
              label: Text(_starting ? 'Starting…' : 'Start run'),
            ),
          ],
        ),
      ),
    );
  }

  Widget _input({
    required TextEditingController controller,
    required String label,
    required Key key,
    String? hintText,
  }) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 12),
      child: TextFormField(
        key: key,
        controller: controller,
        decoration: InputDecoration(labelText: label, hintText: hintText),
        textInputAction: TextInputAction.next,
        validator: (value) =>
            value == null || value.trim().isEmpty ? '$label is required' : null,
      ),
    );
  }
}
