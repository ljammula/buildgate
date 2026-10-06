// The oracle review surfaces (staged-oracle plan step 7): what an operator
// sees at oracle_review (the request-level oracle/), at plan_review (each
// ticket's materialized <NNN>.oracle/), and while oracle_drafting runs.
//
// Approval pins every file of those directories by hash, and the API
// (request.ApproveShown) refuses an approval that does not name a hash for
// each -- so Approve stays disabled until every listed file is currently
// expanded with content whose hash matches the listing's, and the hashes sent
// are those of the bytes received. A collapsed file counts as not shown, and
// so does everything while the listing is stale (a failed reload).
import 'package:flutter/material.dart';

import 'api_client.dart';
import 'content_hash.dart';
import 'error_display.dart';
import 'models.dart';
import 'oracle_files.dart';
import 'text_escape.dart';

const _runCommandName = 'RUN_COMMAND.txt';
const _manifestName = 'MANIFEST.json';

/// What the drafting job's recorded status means to an operator.
String oracleDraftStatusLabel(String status) => switch (status) {
  'drafted' => 'Drafted',
  // Neutral on purpose: the server's own detail says why (a model's
  // one-pass classification, or -- for a non-Go workspace -- no model pass
  // at all), and this prefix used to claim a model judgment even when none
  // ran (found via a console operator walkthrough, 2026-09-24).
  'none_eligible' => 'No criterion was judged eligible for an automated test',
  'failed' => 'Drafting failed',
  'over_cap' => 'Drafting stopped at the per-ticket file/size cap',
  'not_implemented' => 'Automatic drafting is not available for this request',
  _ => status,
};

/// Shown while a request is in oracle_drafting: the factory is working, and
/// a previous pass's outcome (after a rejection) is still on the record.
class OracleDraftingSection extends StatelessWidget {
  const OracleDraftingSection({required this.request, super.key});

  final RequestSummary request;

  @override
  Widget build(BuildContext context) {
    return Column(
      key: const ValueKey('oracle-drafting-section'),
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const Row(
          children: [
            SizedBox(
              width: 16,
              height: 16,
              child: CircularProgressIndicator(strokeWidth: 2),
            ),
            SizedBox(width: 8),
            Expanded(
              child: Text(
                'Drafting acceptance-test oracles from the approved spec. '
                'Nothing to do yet -- this request moves to Oracle review '
                'when drafting finishes; this page updates on its own.',
              ),
            ),
          ],
        ),
        if (request.oracleDraftStatus.isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(top: 8),
            child: EscapedText(
              'Previous pass: '
              '${oracleDraftStatusLabel(request.oracleDraftStatus)}'
              '${request.oracleDraftDetail.isEmpty ? '' : ' -- ${request.oracleDraftDetail}'}',
            ),
          ),
      ],
    );
  }
}

/// The reload-failed notice shown above a stale listing.
class _StaleListingNotice extends StatelessWidget {
  const _StaleListingNotice({required this.error});

  final Object error;

  @override
  Widget build(BuildContext context) => Padding(
    key: const ValueKey('oracle-stale-listing'),
    padding: const EdgeInsets.only(bottom: 8),
    child: Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        ErrorCallout(error: error),
        const Text(
          'The files below are the last listing that loaded and may be out '
          'of date -- Approve is disabled until Reload files succeeds.',
        ),
      ],
    ),
  );
}

/// The oracle_review panel: files with hashes, raw content per file,
/// MANIFEST coverage, approval problems, RUN_COMMAND.txt editing, and an
/// Approve that stays disabled until every file has been shown.
class OracleReviewPanel extends StatefulWidget {
  const OracleReviewPanel({
    required this.api,
    required this.request,
    required this.canAct,
    required this.onApprove,
    super.key,
  });

  final RunApi api;
  final RequestSummary request;

  /// False while the parent is mid-action or its own detail load has not
  /// finished; true still requires the override token (checked here).
  final bool canAct;

  /// Called with the `expected_sha256` map of exactly what was displayed.
  final Future<void> Function(Map<String, String> expectedSha256) onApprove;

  @override
  State<OracleReviewPanel> createState() => _OracleReviewPanelState();
}

class _OracleReviewPanelState extends State<OracleReviewPanel> {
  final _store = OracleFileStore();
  OracleListing? _listing;
  Object? _listError;
  bool _loadingList = false;
  bool _editing = false;
  bool _saving = false;
  Object? _saveError;
  TextEditingController? _editController;

  String get _id => widget.request.id;

  @override
  void initState() {
    super.initState();
    _store.addListener(_onStore);
    _loadListing();
  }

  @override
  void dispose() {
    _store.removeListener(_onStore);
    _store.dispose();
    _editController?.dispose();
    super.dispose();
  }

  void _onStore() {
    if (mounted) setState(() {});
  }

  Future<void> _loadListing() async {
    setState(() {
      _loadingList = true;
      _listError = null;
    });
    try {
      final listing = await widget.api.getRequestOracle(_id);
      if (!mounted) return;
      _store.sync({
        for (final f in listing.files) f.name: f.sha256,
      }, (name) => widget.api.getRequestOracleFile(_id, name));
      setState(() => _listing = listing);
      if (listing.files.any((f) => f.name == _manifestName)) {
        _store.load(_manifestName);
      }
    } on Object catch (error) {
      if (mounted) setState(() => _listError = error);
    } finally {
      if (mounted) setState(() => _loadingList = false);
    }
  }

  void _startEdit() {
    // Invalid bytes cannot round-trip through a text field; they show as
    // U+FFFD here and the saved file will differ from what was displayed.
    final text = String.fromCharCodes(
      (_store.content(_runCommandName)?.text ?? '').runes.map(
        (r) => (r >= 0xD800 && r <= 0xDFFF) ? 0xFFFD : r,
      ),
    );
    setState(() {
      _editController = TextEditingController(text: text);
      _saveError = null;
      _editing = true;
    });
  }

  void _cancelEdit() => setState(() {
    _editing = false;
    _saveError = null;
  });

  Future<void> _save() async {
    final controller = _editController;
    if (controller == null) return;
    setState(() {
      _saving = true;
      _saveError = null;
    });
    try {
      await widget.api.putRequestOracleRunCommand(_id, controller.text);
      if (!mounted) return;
      // The saved file must be re-fetched and re-shown before approval.
      _store.reopen(_runCommandName);
      setState(() => _editing = false);
      await _loadListing();
    } on Object catch (error) {
      if (mounted) setState(() => _saveError = error);
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final listing = _listing;
    final theme = Theme.of(context);
    if (listing == null) {
      return Column(
        key: const ValueKey('oracle-review-panel'),
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          if (_listError != null)
            ErrorCallout(error: _listError!)
          else
            const LinearProgressIndicator(),
          if (_listError != null)
            TextButton(
              key: const ValueKey('oracle-reload-button'),
              onPressed: _loadingList ? null : _loadListing,
              child: const Text('Retry'),
            ),
        ],
      );
    }
    final listed = {for (final f in listing.files) f.name: f.sha256};
    final shown = _store.shown(listed);
    final unseen = listing.files.length - shown.length;
    final atReview = listing.state == 'oracle_review';
    final stale = _listError != null || _loadingList;
    final canApprove =
        widget.canAct &&
        widget.api.canWrite &&
        atReview &&
        listing.problems.isEmpty &&
        unseen == 0 &&
        !stale;
    final manifestText = _store.content(_manifestName)?.text;
    final manifest = manifestText == null
        ? null
        : parseOracleManifest(manifestText);

    return Column(
      key: const ValueKey('oracle-review-panel'),
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (_listError != null) _StaleListingNotice(error: _listError!),
        if (listing.draftStatus.isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(bottom: 8),
            child: KeyedSubtree(
              key: const ValueKey('oracle-draft-status'),
              child: EscapedText(
                'Oracle draft: ${oracleDraftStatusLabel(listing.draftStatus)}'
                '${listing.draftDetail.isEmpty ? '' : ' -- ${listing.draftDetail}'}',
              ),
            ),
          ),
        // Per-criterion eligibility verdicts: a `none_eligible`/
        // `drafted` outcome comes with a receipt -- why the drafter judged
        // each acceptance criterion (in)eligible for an automated oracle --
        // instead of only the free-text status/detail above.
        if (widget.request.oracleDraftCriteria.isNotEmpty)
          _CriteriaList(criteria: widget.request.oracleDraftCriteria),
        if (listing.proposedCommand.isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(bottom: 8),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  'Suggested RUN_COMMAND.txt (a suggestion only -- not '
                  'written to the file):',
                  style: theme.textTheme.bodySmall,
                ),
                KeyedSubtree(
                  key: const ValueKey('oracle-proposed-command'),
                  child: EscapedText(
                    listing.proposedCommand,
                    style: const TextStyle(fontFamily: 'monospace'),
                  ),
                ),
              ],
            ),
          ),
        if (listing.problems.isNotEmpty)
          _ProblemsBox(problems: listing.problems),
        if (listing.files.isEmpty)
          const Padding(
            padding: EdgeInsets.only(bottom: 8),
            child: Text(
              'No oracle files. Approving skips the oracle stage; the '
              'build then has no request-level acceptance test.',
              key: ValueKey('oracle-empty'),
            ),
          )
        else ...[
          if (manifest != null) _CoverageList(entries: manifest),
          const Padding(
            padding: EdgeInsets.only(bottom: 8),
            child: Text(
              'Before approving: check the errors.Is/sentinel assertions '
              '(never against a second, freshly-constructed error) and '
              "whether the oracle reaches beyond its own target file/"
              'package -- a diff_scope quarantine naming a file only '
              'touched to satisfy this oracle means the oracle may be '
              'wrong, not the build.',
              key: ValueKey('oracle-review-hint'),
            ),
          ),
          Padding(
            padding: const EdgeInsets.only(bottom: 4),
            child: Text(
              'Files (${shown.length} of ${listing.files.length} shown) -- '
              'open every one to enable Approve',
              style: theme.textTheme.titleSmall,
            ),
          ),
          for (final file in listing.files)
            OracleFileTile(
              keyId: file.name,
              storeKey: file.name,
              file: file,
              store: _store,
              shown: shown.containsKey(file.name),
              bodyBuilder: file.name == _runCommandName && atReview
                  ? _runCommandBody
                  : null,
            ),
        ],
        const SizedBox(height: 8),
        Wrap(
          spacing: 8,
          crossAxisAlignment: WrapCrossAlignment.center,
          children: [
            FilledButton.icon(
              key: const ValueKey('approve-request-button'),
              onPressed: canApprove
                  ? () async {
                      await widget.onApprove(oracleExpectedSha256(shown));
                      // A refused approval (files changed underneath us)
                      // leaves stale hashes behind; re-list so the panel
                      // matches the server again.
                      if (mounted) await _loadListing();
                    }
                  : null,
              icon: const Icon(Icons.check_circle_outline),
              label: Text(
                listing.files.isEmpty ? 'Approve (skip oracle)' : 'Approve',
              ),
            ),
            OutlinedButton.icon(
              key: const ValueKey('oracle-reload-button'),
              onPressed: _loadingList ? null : _loadListing,
              icon: const Icon(Icons.refresh, size: 16),
              label: const Text('Reload files'),
            ),
            if (unseen > 0)
              Text(
                'Open $unseen more file${unseen == 1 ? '' : 's'} to approve.',
                key: const ValueKey('oracle-unseen-hint'),
              ),
          ],
        ),
      ],
    );
  }

  List<Widget> _runCommandBody(OracleFileContent content) {
    if (_editing) return _editor();
    return [
      OracleContentBox(keyId: _runCommandName, content: content),
      if (widget.api.canWrite)
        Align(
          alignment: Alignment.centerRight,
          child: TextButton.icon(
            key: const ValueKey('oracle-edit-run-command'),
            onPressed: _startEdit,
            icon: const Icon(Icons.edit_outlined, size: 16),
            label: const Text('Edit'),
          ),
        ),
    ];
  }

  List<Widget> _editor() {
    final error = Theme.of(context).colorScheme.error;
    return [
      TextField(
        key: const ValueKey('oracle-run-command-field'),
        controller: _editController,
        maxLines: null,
        minLines: 4,
        style: const TextStyle(fontFamily: 'monospace'),
        decoration: const InputDecoration(border: OutlineInputBorder()),
      ),
      ValueListenableBuilder<TextEditingValue>(
        valueListenable: _editController!,
        builder: (context, value, _) {
          final escaped = escapeInvisible(value.text);
          if (escaped == value.text) return const SizedBox.shrink();
          return Padding(
            padding: const EdgeInsets.only(top: 4),
            child: KeyedSubtree(
              key: const ValueKey('oracle-run-command-preview'),
              child: EscapedText(
                'Contains invisible characters; as saved: ${value.text}',
                style: TextStyle(fontFamily: 'monospace', color: error),
              ),
            ),
          );
        },
      ),
      if (_saveError != null)
        Padding(
          padding: const EdgeInsets.only(top: 4),
          child: KeyedSubtree(
            key: const ValueKey('oracle-run-command-error'),
            child: EscapedText(
              'Could not save: ${_saveError is RunApiException ? (_saveError! as RunApiException).message : _saveError}',
              style: TextStyle(color: error),
            ),
          ),
        ),
      const SizedBox(height: 8),
      Row(
        mainAxisAlignment: MainAxisAlignment.end,
        children: [
          TextButton(
            key: const ValueKey('oracle-run-command-cancel'),
            onPressed: _saving ? null : _cancelEdit,
            child: const Text('Cancel'),
          ),
          const SizedBox(width: 8),
          FilledButton(
            key: const ValueKey('oracle-run-command-save'),
            onPressed: _saving ? null : _save,
            child: Text(_saving ? 'Saving...' : 'Save'),
          ),
        ],
      ),
    ];
  }
}

/// The per-criterion eligibility list: one line per criterion the
/// drafter judged, eligible or not, with its own reason -- surfaced so a
/// `none_eligible` outcome comes with a receipt rather than only a
/// free-text summary.
class _CriteriaList extends StatelessWidget {
  const _CriteriaList({required this.criteria});

  final List<OracleDraftCriterion> criteria;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final colors = theme.colorScheme;
    return Padding(
      key: const ValueKey('oracle-draft-criteria'),
      padding: const EdgeInsets.only(bottom: 8),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('Per-criterion verdict', style: theme.textTheme.titleSmall),
          for (final c in criteria)
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Icon(
                    c.eligible
                        ? Icons.check_circle_outline
                        : Icons.cancel_outlined,
                    size: 16,
                    color: c.eligible ? colors.primary : colors.error,
                  ),
                  const SizedBox(width: 6),
                  Expanded(
                    child: EscapedText(
                      '${c.number}. ${c.eligible ? 'Eligible' : 'Not eligible'}'
                      '${c.reason.isEmpty ? '' : ' -- ${c.reason}'}',
                    ),
                  ),
                ],
              ),
            ),
        ],
      ),
    );
  }
}

class _ProblemsBox extends StatelessWidget {
  const _ProblemsBox({required this.problems});

  final List<String> problems;

  @override
  Widget build(BuildContext context) {
    final colors = Theme.of(context).colorScheme;
    return Container(
      key: const ValueKey('oracle-problems'),
      width: double.infinity,
      margin: const EdgeInsets.only(bottom: 8),
      padding: const EdgeInsets.all(12),
      decoration: BoxDecoration(
        color: colors.errorContainer,
        borderRadius: BorderRadius.circular(4),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            'Approval is blocked until these are fixed:',
            style: TextStyle(
              fontWeight: FontWeight.bold,
              color: colors.onErrorContainer,
            ),
          ),
          for (final problem in problems)
            EscapedText(
              '- $problem',
              style: TextStyle(color: colors.onErrorContainer),
            ),
        ],
      ),
    );
  }
}

class _CoverageList extends StatelessWidget {
  const _CoverageList({required this.entries});

  final List<OracleManifestEntry> entries;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return Padding(
      key: const ValueKey('oracle-coverage'),
      padding: const EdgeInsets.only(bottom: 12),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('Criteria coverage', style: theme.textTheme.titleSmall),
          if (entries.isEmpty) const Text('MANIFEST.json lists no criteria.'),
          for (final e in entries)
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: EscapedText(_summary(e)),
            ),
        ],
      ),
    );
  }

  static String _summary(OracleManifestEntry e) {
    final head =
        '${e.criterionIndex == null ? '-' : '${e.criterionIndex}.'} '
        '${e.criterion}';
    final covered = e.oracleFile == null
        ? 'Not covered by an oracle'
              '${e.rationale.isEmpty ? '' : ': ${e.rationale}'}'
        : 'Covered by ${e.oracleFile}'
              '${e.targetPath.isEmpty ? '' : ' (tests ${e.targetPath})'}';
    final supersedes = e.supersedes.isEmpty
        ? ''
        : '\n   Supersedes: ${e.supersedes.join(', ')}';
    return '$head\n   $covered$supersedes';
  }
}

/// What a [TicketOraclePanel] has displayed: every shown file's approval key
/// (`tickets/NNN.oracle/name`) -> sha256, and whether approval may proceed
/// (all listings loaded fresh, no problems, every file shown).
class TicketOracleShown {
  const TicketOracleShown({required this.hashes, required this.complete});

  final Map<String, String> hashes;
  final bool complete;

  @override
  bool operator ==(Object other) =>
      other is TicketOracleShown &&
      other.complete == complete &&
      other.hashes.length == hashes.length &&
      other.hashes.entries.every((e) => hashes[e.key] == e.value);

  @override
  int get hashCode => Object.hash(complete, hashes.length);
}

class _TicketGroup {
  _TicketGroup(this.ticket, this.prefix);

  final RequestTicket ticket;
  final String prefix;
  OracleListing? listing;
  Object? error;
}

/// The plan_review panel: each ticket's materialized `<NNN>.oracle/` files,
/// which plan approval hash-pins. Renders nothing when no ticket has any;
/// reports what it has shown through [onChanged] so the screen's Approve can
/// wait for it and send those hashes with the spec hashes.
class TicketOraclePanel extends StatefulWidget {
  const TicketOraclePanel({
    required this.api,
    required this.request,
    required this.onChanged,
    super.key,
  });

  final RunApi api;
  final RequestSummary request;
  final ValueChanged<TicketOracleShown> onChanged;

  @override
  State<TicketOraclePanel> createState() => _TicketOraclePanelState();
}

class _TicketOraclePanelState extends State<TicketOraclePanel> {
  final _store = OracleFileStore();
  late final List<_TicketGroup> _groups;
  bool _loading = false;
  Object? _reloadError;
  TicketOracleShown? _reported;

  @override
  void initState() {
    super.initState();
    _groups = [
      for (final t in widget.request.tickets)
        if (t.specPath.isNotEmpty) _TicketGroup(t, _oraclePrefix(t.specPath)),
    ];
    _store.addListener(_onStore);
    _load();
  }

  // "…/tickets/001.spec.md" -> "tickets/001.oracle": the key plan approval
  // pins these files under (internal/request.TicketOracleDir).
  static String _oraclePrefix(String specPath) {
    final base = specPath.replaceAll('\\', '/').split('/').last;
    return 'tickets/${base.replaceFirst(RegExp(r'\.spec\.md$'), '')}.oracle';
  }

  @override
  void dispose() {
    _store.removeListener(_onStore);
    _store.dispose();
    super.dispose();
  }

  void _onStore() {
    if (mounted) setState(() {});
  }

  Future<void> _load() async {
    setState(() {
      _loading = true;
      _reloadError = null;
    });
    final fresh = <_TicketGroup, OracleListing>{};
    Object? failure;
    await Future.wait([
      for (final g in _groups)
        () async {
          try {
            fresh[g] = await widget.api.getRequestTicketOracle(
              widget.request.id,
              g.ticket.index,
            );
          } on RunApiException catch (e) {
            // A server without the route (or a ticket it does not know)
            // has no materialized files to show.
            if (e.statusCode == 404) {
              fresh[g] = const OracleListing(
                files: [],
                problems: [],
                state: '',
              );
            } else {
              failure ??= e;
            }
          } on Object catch (e) {
            failure ??= e;
          }
        }(),
    ]);
    if (!mounted) return;
    if (failure != null) {
      // Keep whatever listings loaded before: stale, but visible.
      setState(() {
        _reloadError = failure;
        _loading = false;
      });
      return;
    }
    final byKey = <String, (int, String)>{};
    final listed = <String, String>{};
    for (final g in _groups) {
      g.listing = fresh[g];
      for (final f in g.listing!.files) {
        final key = '${g.prefix}/${f.name}';
        byKey[key] = (g.ticket.index, f.name);
        listed[key] = f.sha256;
      }
    }
    _store.sync(listed, (key) {
      final (n, name) = byKey[key]!;
      return widget.api.getRequestTicketOracleFile(widget.request.id, n, name);
    });
    setState(() => _loading = false);
  }

  TicketOracleShown _current(Map<String, String> listed) {
    final shown = _store.shown(listed);
    final complete =
        !_loading &&
        _reloadError == null &&
        _groups.every(
          (g) => g.listing != null && g.listing!.problems.isEmpty,
        ) &&
        shown.length == listed.length;
    return TicketOracleShown(hashes: shown, complete: complete);
  }

  @override
  Widget build(BuildContext context) {
    final listed = {
      for (final g in _groups)
        if (g.listing != null)
          for (final f in g.listing!.files) '${g.prefix}/${f.name}': f.sha256,
    };
    final current = _current(listed);
    if (current != _reported) {
      _reported = current;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) widget.onChanged(current);
      });
    }
    final withFiles = _groups
        .where((g) => g.listing != null && g.listing!.files.isNotEmpty)
        .toList();
    final problemGroups = _groups
        .where((g) => g.listing != null && g.listing!.problems.isNotEmpty)
        .toList();
    if (withFiles.isEmpty && problemGroups.isEmpty && _reloadError == null) {
      return const SizedBox.shrink(key: ValueKey('ticket-oracle-panel'));
    }
    final theme = Theme.of(context);
    return Column(
      key: const ValueKey('ticket-oracle-panel'),
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('Ticket oracle files', style: theme.textTheme.titleMedium),
        const SizedBox(height: 4),
        const Text(
          'Approving the plan pins these acceptance tests by hash. Open '
          'every file to enable Approve.',
        ),
        if (_reloadError != null) _StaleListingNotice(error: _reloadError!),
        for (final g in problemGroups)
          _ProblemsBox(problems: g.listing!.problems),
        for (final g in withFiles) ...[
          Padding(
            padding: const EdgeInsets.only(top: 8),
            child: Text(
              'Ticket ${g.ticket.index} (${g.prefix}/)',
              style: theme.textTheme.titleSmall,
            ),
          ),
          for (final file in g.listing!.files)
            OracleFileTile(
              keyId: '${g.ticket.index}/${file.name}',
              storeKey: '${g.prefix}/${file.name}',
              file: file,
              store: _store,
              shown: current.hashes.containsKey('${g.prefix}/${file.name}'),
            ),
        ],
        Row(
          children: [
            OutlinedButton.icon(
              key: const ValueKey('ticket-oracle-reload-button'),
              onPressed: _loading ? null : _load,
              icon: const Icon(Icons.refresh, size: 16),
              label: const Text('Reload oracle files'),
            ),
            const SizedBox(width: 8),
            Text(
              '${current.hashes.length} of ${listed.length} shown',
              key: const ValueKey('ticket-oracle-count'),
            ),
          ],
        ),
      ],
    );
  }
}
