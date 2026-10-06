// The console's own "start a request" form: the
// equivalent of new_run_screen.dart's "New run" form, but for POST
// /requests instead of POST /runs -- the higher-level entry point
// that drives spec drafting, planning, and building, not a single
// already-specced ticket.
import 'package:flutter/material.dart';

import 'api_client.dart';
import 'approve_reject.dart' show ensureOperatorName;
import 'error_display.dart';
import 'models.dart';
import 'request_detail_screen.dart';

class NewRequestScreen extends StatefulWidget {
  const NewRequestScreen({required this.api, this.onCreated, super.key});

  final RunApi api;

  /// Reports the newly created request's id to the router (see
  /// RequestBoardRouterDelegate.openRequestDetail) so it can switch its
  /// own deep-link state to `/requests/<id>` -- unlike NewRunScreen's
  /// plain `Navigator.push`, an imperative push from a router-managed
  /// page does not update the browser's address bar
  /// (request_board_route.dart's own doc comment), and
  /// scripts/console-walk/walk.mjs needs the URL to actually change so
  /// it can derive the new request's id from it. Null (mostly for tests
  /// that construct this screen outside the router) falls back to a
  /// plain imperative push instead.
  final ValueChanged<String>? onCreated;

  @override
  State<NewRequestScreen> createState() => _NewRequestScreenState();
}

class _NewRequestScreenState extends State<NewRequestScreen> {
  final _formKey = GlobalKey<FormState>();
  final _workspaceController = TextEditingController();
  final _textController = TextEditingController();
  final _verifyCommandController = TextEditingController();
  final _fullSuiteCommandController = TextEditingController();
  final _preflightProfileController = TextEditingController();
  bool _draftOracles = false;
  bool _advancedExpanded = false;

  List<WorkspaceHint>? _workspaces;
  Object? _workspacesError;
  bool _loadingWorkspaces = true;
  // The dropdown's own selection, kept independent of
  // _workspaceController's text: a dropdown-selected path always mirrors
  // into the text field (see _selectWorkspace), but the operator can
  // still hand-edit the text field afterward (a path GET /workspaces
  // never listed) without the dropdown's stale selection fighting it.
  String? _selectedWorkspace;

  Object? _error;
  bool _submitting = false;

  @override
  void initState() {
    super.initState();
    _loadWorkspaces();
  }

  @override
  void dispose() {
    _workspaceController.dispose();
    _textController.dispose();
    _verifyCommandController.dispose();
    _fullSuiteCommandController.dispose();
    _preflightProfileController.dispose();
    super.dispose();
  }

  Future<void> _loadWorkspaces() async {
    setState(() {
      _loadingWorkspaces = true;
      _workspacesError = null;
    });
    try {
      final workspaces = await widget.api.listWorkspaces();
      if (mounted) setState(() => _workspaces = workspaces);
    } on Object catch (error) {
      if (mounted) setState(() => _workspacesError = error);
    } finally {
      if (mounted) setState(() => _loadingWorkspaces = false);
    }
  }

  // _selectWorkspace mirrors a dropdown pick into the free-text field and
  // pre-fills the Advanced section's verify-command hint field with what
  // the server already resolved for it -- an operator who picks a known
  // workspace never has to separately retype its verify command, but can
  // still overwrite it explicitly (see createRequestBody's own
  // "explicit wins" precedence, server-side).
  void _selectWorkspace(WorkspaceHint? hint) {
    if (hint == null) return;
    setState(() {
      _selectedWorkspace = hint.workspace;
      _workspaceController.text = hint.workspace;
    });
  }

  WorkspaceHint? _hintFor(String workspace) {
    for (final hint in _workspaces ?? const <WorkspaceHint>[]) {
      if (hint.workspace == workspace) return hint;
    }
    return null;
  }

  Future<void> _submit() async {
    if (!_formKey.currentState!.validate()) return;
    final by = await ensureOperatorName(context);
    if (by.isEmpty) return;
    setState(() {
      _submitting = true;
      _error = null;
    });
    try {
      final created = await widget.api.createRequest(
        workspace: _workspaceController.text.trim(),
        text: _textController.text.trim(),
        verifyCommand: _verifyCommandController.text.trim(),
        fullSuiteCommand: _fullSuiteCommandController.text.trim(),
        preflightProfile: _preflightProfileController.text.trim(),
        draftOracles: _draftOracles,
        by: by,
      );
      if (!mounted) return;
      if (widget.onCreated case final onCreated?) {
        onCreated(created.id);
      } else {
        await Navigator.of(context).pushReplacement(
          MaterialPageRoute<void>(
            builder: (_) => RequestDetailScreen(
              api: widget.api,
              requestId: created.id,
              initialRequest: created,
            ),
          ),
        );
      }
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    } finally {
      if (mounted) setState(() => _submitting = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final currentHint = _hintFor(_workspaceController.text.trim());
    return Scaffold(
      appBar: AppBar(title: const Text('New request')),
      body: Form(
        key: _formKey,
        child: ListView(
          padding: const EdgeInsets.all(16),
          children: [
            const Text(
              'Start a request',
              style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600),
            ),
            const SizedBox(height: 8),
            const Text(
              'A request drives spec drafting, planning, and building end '
              'to end -- the same thing `factoryd submit` starts from a '
              'terminal.',
            ),
            const SizedBox(height: 20),
            if (_loadingWorkspaces)
              const Padding(
                padding: EdgeInsets.only(bottom: 12),
                child: LinearProgressIndicator(
                  key: ValueKey('workspaces-loading'),
                ),
              ),
            if (_workspacesError case final error?) ...[
              ErrorCallout(
                key: const ValueKey('workspaces-error'),
                error: error,
              ),
              const SizedBox(height: 12),
            ],
            if ((_workspaces ?? const []).isNotEmpty) ...[
              DropdownButtonFormField<String>(
                key: const ValueKey('new-request-workspace-dropdown'),
                initialValue: _selectedWorkspace,
                decoration: const InputDecoration(
                  labelText: 'Known workspace',
                  hintText: 'Pick one, or type a path below',
                ),
                items: [
                  for (final hint in _workspaces!)
                    DropdownMenuItem(
                      value: hint.workspace,
                      child: Text(
                        hint.workspace,
                        overflow: TextOverflow.ellipsis,
                      ),
                    ),
                ],
                onChanged: (value) => _selectWorkspace(_hintFor(value ?? '')),
              ),
              const SizedBox(height: 12),
            ],
            _input(
              controller: _workspaceController,
              label: 'Workspace path',
              hintText: 'A git repository root on the factoryd host',
              key: const ValueKey('new-request-workspace'),
              onChanged: (_) => setState(() {}),
            ),
            if (currentHint case final hint? when hint.hasFactoryYml)
              Padding(
                padding: const EdgeInsets.only(bottom: 12, left: 4),
                child: Text(
                  hint.resolvedVerifyCommand.isNotEmpty
                      ? 'Verify command from ${hint.verifyCommandSource}: '
                            '${hint.resolvedVerifyCommand}'
                      : 'This workspace has a .factory.yml, but no '
                            'verify_command set.',
                  key: const ValueKey('workspace-verify-hint'),
                  style: Theme.of(context).textTheme.bodySmall,
                ),
              ),
            _input(
              controller: _textController,
              label: 'Request',
              hintText: 'What should the factory build?',
              key: const ValueKey('new-request-text'),
              maxLines: 8,
            ),
            CheckboxListTile(
              key: const ValueKey('new-request-draft-oracles'),
              value: _draftOracles,
              onChanged: (value) =>
                  setState(() => _draftOracles = value ?? false),
              controlAffinity: ListTileControlAffinity.leading,
              contentPadding: EdgeInsets.zero,
              title: const Text('Draft oracles'),
              subtitle: const Text(
                'Adds an oracle-review stage between spec approval and '
                'planning, to approve/hand-write/skip acceptance-test '
                'oracles before any ticket is planned.',
              ),
            ),
            ExpansionTile(
              key: const ValueKey('new-request-advanced'),
              title: const Text('Advanced'),
              initiallyExpanded: _advancedExpanded,
              onExpansionChanged: (expanded) =>
                  setState(() => _advancedExpanded = expanded),
              childrenPadding: const EdgeInsets.only(bottom: 8),
              children: [
                _input(
                  controller: _verifyCommandController,
                  label: 'Verify command',
                  hintText:
                      currentHint?.resolvedVerifyCommand.isNotEmpty == true
                      ? 'Default: ${currentHint!.resolvedVerifyCommand}'
                      : 'Default: this workspace\'s .factory.yml '
                            'verify_command',
                  key: const ValueKey('new-request-verify-command'),
                  required: false,
                ),
                _input(
                  controller: _fullSuiteCommandController,
                  label: 'Full-suite command',
                  hintText:
                      'Repo-wide regression command; default: none, or '
                      '.factory.yml full_suite_command',
                  key: const ValueKey('new-request-full-suite-command'),
                  required: false,
                ),
                _input(
                  controller: _preflightProfileController,
                  label: 'Preflight profile',
                  hintText: 'Empty (strict), or "brownfield"',
                  key: const ValueKey('new-request-preflight-profile'),
                  required: false,
                ),
              ],
            ),
            if (!widget.api.canWrite) ...[
              const SizedBox(height: 12),
              const ErrorCallout(
                key: ValueKey('new-request-writes-disabled'),
                error:
                    'This console cannot write to the server from here -- '
                    'no override token is configured and the server did '
                    'not enable an unauthenticated console write.',
              ),
            ],
            if (_error case final error?) ...[
              const SizedBox(height: 12),
              ErrorCallout(
                key: const ValueKey('new-request-error'),
                error: error,
              ),
            ],
            const SizedBox(height: 20),
            FilledButton.icon(
              key: const ValueKey('submit-request-button'),
              onPressed: (_submitting || !widget.api.canWrite) ? null : _submit,
              icon: _submitting
                  ? const SizedBox.square(
                      dimension: 18,
                      child: CircularProgressIndicator(strokeWidth: 2),
                    )
                  : const Icon(Icons.send),
              label: Text(_submitting ? 'Submitting…' : 'Submit request'),
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
    int maxLines = 1,
    ValueChanged<String>? onChanged,
    // Every Advanced-section field is an optional override of a
    // server-resolved default (verify command, full-suite command,
    // preflight profile) -- required defaults to true so the two
    // top-level fields (workspace, request text) don't have to repeat
    // it, and every Advanced field call site passes false explicitly.
    bool required = true,
  }) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 12),
      child: TextFormField(
        key: key,
        controller: controller,
        decoration: InputDecoration(labelText: label, hintText: hintText),
        maxLines: maxLines,
        onChanged: onChanged,
        validator: !required
            ? null
            : (value) => value == null || value.trim().isEmpty
                  ? '$label is required'
                  : null,
      ),
    );
  }
}
