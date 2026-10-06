import 'dart:async';

import 'package:flutter/material.dart';

import 'api_client.dart';
import 'error_display.dart';
import 'models.dart';
import 'request_board_filters.dart';
import 'request_cost.dart';
import 'request_detail_screen.dart';
import 'run_list_screen.dart';
import 'status.dart';
import 'tab_title.dart';
import 'ticket_rollup.dart';

/// How often this screen re-polls GET /requests on its own -- matches
/// run_list_screen.dart's own runListAutoRefreshInterval, so an operator
/// leaving either screen open sees `factoryd worker` advance a
/// request/run without needing to remember to hit refresh themselves.
const requestListAutoRefreshInterval = Duration(seconds: 5);

/// The four groups a request's state sorts and colors by (see
/// requestStageGroup) -- the plan's own "waiting-on-you = review states;
/// working = drafting/planning/building/pr_review; done; failed =
/// quarantined/cancelled".
///
/// `.review` here means "waiting on you", not literally "in a review
/// state" -- it includes `halted`, which is not one of the two request
/// review states (spec_review/plan_review) but is genuinely
/// operator-actionable right now via the retry flow, exactly like the
/// shared status vocabulary already says (status.dart maps `halted` to
/// `Status.needsHuman`, the same bucket as spec_review/plan_review).
/// Found in review: this enum disagreed with that vocabulary, so a
/// halted request was sorted into "Finished" on the board and silently
/// excluded from the "needs you" tab-title count -- the operator action
/// it needs (retry) never surfaced as something needing them. Deliberately
/// still distinct from `quarantined`/`cancelled`, which stay `.failed`:
/// those are genuine dead ends from this console's perspective (no
/// resume affordance here), not something an operator "reviews".
enum RequestStageGroup { review, working, done, failed, other }

const _reviewStates = {
  'spec_review',
  'oracle_review',
  'plan_review',
  'halted',
  'resume_review',
};
// submitted is working, not "other": other rendered under Finished, so a
// just-submitted request queued behind another looked finished.
const _workingStates = {
  'submitted',
  'spec_drafting',
  'oracle_drafting',
  'planning',
  'building',
  'pr_review',
};
const _doneStates = {'done'};
const _failedStates = {'quarantined', 'cancelled'};

/// [requestStageGroup] for a whole request: a `pr_review` request whose
/// PRs all wait on a human (every opened PR `ready`, `stacked` on another
/// ticket's unmerged PR, or already `merged`, at least one not merged)
/// needs you, not the factory. A `draft` or `approved` PR means the
/// factory still has work to do on it.
RequestStageGroup requestStageGroupOf(RequestSummary request) {
  if (request.state == 'pr_review') {
    final prStates = [
      for (final t in request.tickets)
        if (t.prUrl.isNotEmpty && t.prState.isNotEmpty) t.prState,
    ];
    const waitingOnHuman = {'ready', 'stacked'};
    if (prStates.any(waitingOnHuman.contains) &&
        prStates.every((s) => waitingOnHuman.contains(s) || s == 'merged')) {
      return RequestStageGroup.review;
    }
  }
  return requestStageGroup(request.state);
}

RequestStageGroup requestStageGroup(String state) {
  if (_reviewStates.contains(state)) return RequestStageGroup.review;
  if (_workingStates.contains(state)) return RequestStageGroup.working;
  if (_doneStates.contains(state)) return RequestStageGroup.done;
  if (_failedStates.contains(state)) return RequestStageGroup.failed;
  return RequestStageGroup.other;
}

int _stageRank(RequestSummary request) => requestStageGroupOf(request).index;

/// Sorts requests for the board: review states first (oldest wait first),
/// then working states by updatedAt descending, then everything else
/// (done/failed/other) also by updatedAt descending. Returns a new list
/// -- does not mutate requests.
List<RequestSummary> sortedRequests(List<RequestSummary> requests) {
  final copy = List<RequestSummary>.of(requests);
  copy.sort((a, b) {
    final rankA = _stageRank(a);
    final rankB = _stageRank(b);
    if (rankA != rankB) return rankA.compareTo(rankB);
    if (requestStageGroupOf(a) == RequestStageGroup.review) {
      // Oldest wait first: ascending by waiting-since.
      return _timeKey(
        a.waitingSinceOrEnteredAt,
      ).compareTo(_timeKey(b.waitingSinceOrEnteredAt));
    }
    // Most recently updated first.
    return _timeKey(b.updatedAt).compareTo(_timeKey(a.updatedAt));
  });
  return copy;
}

// _timeKey turns an RFC3339 timestamp into a value Comparable across
// requests, with an unparseable/empty value sorting as the oldest
// possible time rather than throwing -- a request board row must always
// render in some stable order, never crash the sort over one malformed
// field.
DateTime _timeKey(String value) =>
    DateTime.tryParse(value) ?? DateTime.fromMillisecondsSinceEpoch(0);

/// The "Waiting on you · 45m" badge text for request, or null when
/// request's state is not a review state. now is passed in (rather than
/// read from DateTime.now() internally) so tests can assert an exact age
/// without a real clock.
String? waitingBadgeLabel(RequestSummary request, DateTime now) {
  if (requestStageGroupOf(request) != RequestStageGroup.review) return null;
  final since = DateTime.tryParse(request.waitingSinceOrEnteredAt);
  if (since == null) return 'Waiting on you';
  return 'Waiting on you · ${_formatAge(now.difference(since))}';
}

String _formatAge(Duration age) {
  final minutes = age.isNegative ? 0 : age.inMinutes;
  if (minutes < 60) return '${minutes}m';
  final hours = minutes ~/ 60;
  final remainder = minutes % 60;
  return remainder == 0 ? '${hours}h' : '${hours}h ${remainder}m';
}

/// The "needs you" count: how many of [requests] are
/// in a review state, derived from the same [requestStageGroup] the board
/// itself already sorts by -- no new request, no server change, just a
/// count over data this screen already polled.
int needsHumanCount(List<RequestSummary> requests) => requests
    .where((r) => requestStageGroupOf(r) == RequestStageGroup.review)
    .length;

/// How many consecutive `GET /requests/events` reconnect attempts this
/// screen tolerates before its freshness indicator calls the SSE channel
/// [RequestBoardFreshness.disconnected] rather than
/// [RequestBoardFreshness.recent] -- the point past which a blip reads
/// as a real outage rather than one dropped connection. The 5s
/// [requestListAutoRefreshInterval] poll keeps running throughout
/// regardless of this count -- it was never gated on SSE health, so it
/// is already the fallback this screen needs, just made visible here
/// rather than silently redundant.
const maxSseFailuresBeforeDisconnected = 3;

/// The board's live-connection state: whether its
/// `GET /requests/events` subscription is [live] (currently connected),
/// [recent] (mid-reconnect, but within [maxSseFailuresBeforeDisconnected]
/// attempts of its last known-good connection), or [disconnected] (past
/// that threshold, or a permanent failure) -- purely a display concept,
/// derived from [RunApi.watchRequests]'s own connection callback, and
/// never itself a reason to stop or start the 5s poll.
enum RequestBoardFreshness { live, recent, disconnected }

RequestBoardFreshness requestBoardFreshness({
  required bool sseConnected,
  required int sseFailureCount,
}) {
  if (sseConnected) return RequestBoardFreshness.live;
  if (sseFailureCount >= maxSseFailuresBeforeDisconnected) {
    return RequestBoardFreshness.disconnected;
  }
  return RequestBoardFreshness.recent;
}

/// The freshness indicator's own display text -- "live" / "last updated
/// Ns ago" / "disconnected". [now] is passed in
/// (rather than read internally) for the same reason [waitingBadgeLabel]
/// takes it: a test can assert an exact age without a real clock.
/// [lastUpdateAt] is the last time this screen actually applied new data
/// (from either the poll or an SSE event) -- null only before the very
/// first successful load, when there is nothing to report an age for yet.
String freshnessLabel(
  RequestBoardFreshness freshness,
  DateTime? lastUpdateAt,
  DateTime now,
) {
  return switch (freshness) {
    RequestBoardFreshness.live => 'Live',
    RequestBoardFreshness.disconnected => 'Disconnected',
    RequestBoardFreshness.recent =>
      lastUpdateAt == null
          ? 'Connecting…'
          : 'Last updated ${now.difference(lastUpdateAt).inSeconds}s ago',
  };
}

class RequestListScreen extends StatefulWidget {
  const RequestListScreen({
    required this.api,
    this.filters = const RequestBoardFilters(),
    this.onFiltersChanged,
    this.onOpenTriage,
    this.onOpenNewRequest,
    this.themeMode = ThemeMode.system,
    this.onThemeModeChanged,
    super.key,
  });

  final RunApi api;

  /// The board's current filter state -- owned by
  /// the caller (the request-board router delegate in real use) so it can
  /// stay the single source of truth the URL's query string is derived
  /// from. Defaults to "no filters" for callers (mostly tests) that don't
  /// need the router at all.
  final RequestBoardFilters filters;

  /// Called whenever the operator changes a filter. Null is a valid,
  /// filters-are-display-only mode (again, mostly for tests) -- this
  /// screen still applies [filters] to what it shows either way, it just
  /// has nowhere to report a change to.
  final ValueChanged<RequestBoardFilters>? onFiltersChanged;

  /// Opens the batch triage view. Null hides the
  /// "Triage" app bar button entirely (mostly for tests that don't wire
  /// up the router) rather than rendering a button that does nothing.
  final VoidCallback? onOpenTriage;

  /// Opens `/requests/new`. Null hides the "New request"
  /// app bar button entirely, matching [onOpenTriage]'s own
  /// null-hides-the-button convention.
  final VoidCallback? onOpenNewRequest;

  /// The app's current theme mode -- rendered as
  /// an app bar toggle icon reflecting the current choice.
  final ThemeMode themeMode;

  /// Called with the next theme mode when the operator taps the toggle.
  /// Null hides the toggle entirely (mostly for tests that don't wire up
  /// the router/FactoryConsole's own persistence), matching
  /// [onOpenTriage]'s own null-hides-the-button convention.
  final ValueChanged<ThemeMode>? onThemeModeChanged;

  @override
  State<RequestListScreen> createState() => _RequestListScreenState();
}

class _RequestListScreenState extends State<RequestListScreen> {
  // Same nullable-value/nullable-error, keep-stale-data-on-failure pattern
  // as run_list_screen.dart's own _RunListScreenState -- see its doc
  // comment for why a transient refresh failure must not blank an
  // already-loaded board.
  List<RequestSummary>? _requests;
  Object? _error;
  bool _loading = false;
  Timer? _autoRefresh;
  late RequestBoardFilters _filters;
  late final TextEditingController _searchController;

  // The board's live-connection state -- see
  // requestBoardFreshness/freshnessLabel's own doc comments for how these
  // three fields become the "live"/"last updated Ns ago"/"disconnected"
  // text. The 5s poll below is unconditional and unaffected by any of
  // this -- it was the plan's own "stays as fallback" mechanism before
  // this section existed, and stays exactly that now.
  StreamSubscription<RequestSummary>? _events;
  bool _sseConnected = false;
  int _sseFailureCount = 0;
  DateTime? _lastUpdateAt;

  // GET /queue-run's own answer for whether `worker` is alive, so
  // a request stuck in a working state can be told apart from ordinary
  // progress. Replaced GET /daemons (found live, 2026-09-24): that route
  // only lists daemons `factoryd serve` itself manages via its own
  // lifecycle controller and 404s otherwise, so the strip never fired for
  // the common setup where an operator runs `factoryd worker` by hand
  // instead of through `factoryd serve`. Null means "no positive evidence
  // either way" -- covers both "hasn't loaded yet" and "the call failed"
  // (including a 404 from a server predating this route) -- and is
  // deliberately never treated as "worker is down": this is a
  // best-effort operational signal, not something worth false-alarming an
  // operator over on an older server or a transient network blip. Only a
  // successful response drives the strip below.
  QueueRunStatus? _queueRunStatus;

  @override
  void initState() {
    super.initState();
    _filters = widget.filters;
    _searchController = TextEditingController(text: _filters.search);
    _load();
    _loadQueueRunStatus();
    _autoRefresh = Timer.periodic(
      requestListAutoRefreshInterval,
      (_) => _load(),
    );
    _events = widget.api
        .watchRequests(onConnectionChange: _onSseConnectionChange)
        .listen(_onRequestEvent, onError: _onSseError);
  }

  Future<void> _loadQueueRunStatus() async {
    try {
      final status = await widget.api.getQueueRunStatus();
      if (mounted) setState(() => _queueRunStatus = status);
    } on Object {
      if (mounted) setState(() => _queueRunStatus = null);
    }
  }

  // States only a live worker can advance -- StateSubmitted through
  // StateBuilding on the Go side (see cmd/factoryd's own
  // queueRunDependentState, which this mirrors): every *_review
  // human-gated state waits on the operator, not the factory, and every
  // terminal state needs nothing further.
  static const _queueRunDependentStates = {
    'submitted',
    'spec_drafting',
    'oracle_drafting',
    'planning',
    'building',
  };

  // The strip's text, or null when it should stay hidden -- shown for
  // "stale" (worker ran and stopped refreshing its heartbeat)
  // unconditionally, and for "absent" (worker has never run against
  // this data dir at all) only when a request is actually waiting on one,
  // so a fresh -data-dir with an empty board doesn't warn about a
  // worker nobody has started yet. Never shown for "alive", and never
  // shown when the call itself failed or 404d (see _queueRunStatus' own
  // doc comment).
  String? get _queueRunWarning {
    final status = _queueRunStatus;
    if (status == null) return null;
    switch (status.state) {
      case 'stale':
        final age = _heartbeatAge(status.lastHeartbeat);
        final suffix = age == null ? '' : ' (last heartbeat $age ago)';
        return "worker is not running$suffix -- requests won't advance; "
            'start `factoryd worker`';
      case 'absent':
        final requests = _requests;
        final hasWorkingRequest =
            requests != null &&
            requests.any((r) => _queueRunDependentStates.contains(r.state));
        if (!hasWorkingRequest) return null;
        return 'no worker has run against this data dir -- '
            "requests won't advance; start `factoryd worker`";
      default:
        return null;
    }
  }

  // Renders status.lastHeartbeat's RFC3339 timestamp as a short "Ns/Nm/Nh
  // ago" duration, or null when it isn't a parseable timestamp (an empty
  // string, or a heartbeat written before some future field existed) --
  // the strip then falls back to no age suffix rather than showing a
  // garbled duration.
  String? _heartbeatAge(String lastHeartbeat) {
    final updatedAt = DateTime.tryParse(lastHeartbeat);
    if (updatedAt == null) return null;
    final age = DateTime.now().difference(updatedAt);
    if (age.inHours >= 1) return '${age.inHours}h';
    if (age.inMinutes >= 1) return '${age.inMinutes}m';
    return '${age.inSeconds}s';
  }

  @override
  void didUpdateWidget(RequestListScreen oldWidget) {
    super.didUpdateWidget(oldWidget);
    // The router delegate is the source of truth for filters (e.g. after
    // a browser back/forward navigation changes the URL) -- only adopt an
    // externally changed value, so this doesn't fight the operator's own
    // in-progress typing/selection (_updateFilters below) on every
    // rebuild this screen's own polling timer triggers.
    if (widget.filters != oldWidget.filters && widget.filters != _filters) {
      setState(() {
        _filters = widget.filters;
        _searchController.text = _filters.search;
      });
    }
  }

  @override
  void dispose() {
    _autoRefresh?.cancel();
    _events?.cancel();
    _searchController.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    setState(() => _loading = true);
    try {
      final requests = await widget.api.listRequests();
      if (mounted) {
        setState(() {
          _requests = requests;
          _error = null;
          _lastUpdateAt = DateTime.now();
        });
      }
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    } finally {
      if (mounted) setState(() => _loading = false);
      // Runs whether the poll just above succeeded or failed -- the tab
      // title/favicon must reflect a poll failure
      // immediately (`(?)`), not keep showing a count that may now be
      // wrong until some later successful poll happens to overwrite it.
      updateNeedsHumanSignal(
        needsHumanCount: _requests == null ? null : needsHumanCount(_requests!),
        pollFailed: _error != null,
      );
    }
  }

  // _onRequestEvent merges one changed RequestSummary (a
  // `GET /requests/events` frame) into _requests by id -- the
  // board's own state stays a single list either poll or the SSE stream
  // can update, rather than two parallel sources of truth a caller would
  // have to reconcile itself.
  void _onRequestEvent(RequestSummary updated) {
    if (!mounted) return;
    setState(() {
      final list = List<RequestSummary>.of(_requests ?? const []);
      final index = list.indexWhere((r) => r.id == updated.id);
      if (index == -1) {
        list.add(updated);
      } else {
        list[index] = updated;
      }
      _requests = list;
      _error = null;
      _lastUpdateAt = DateTime.now();
    });
    updateNeedsHumanSignal(
      needsHumanCount: needsHumanCount(_requests!),
      pollFailed: false,
    );
  }

  void _onSseConnectionChange(bool connected) {
    if (!mounted) return;
    setState(() {
      _sseConnected = connected;
      if (connected) {
        _sseFailureCount = 0;
      } else {
        _sseFailureCount++;
      }
    });
  }

  // A permanent (4xx) failure ends watchRequests' stream for good (see
  // its own doc comment) -- this screen has no way to make it reconnect
  // short of a full reload, so it just settles into "disconnected" and
  // relies entirely on the 5s poll from here on, same as it always could.
  void _onSseError(Object error) {
    if (!mounted) return;
    setState(() {
      _sseConnected = false;
      _sseFailureCount = maxSseFailuresBeforeDisconnected;
    });
  }

  void _updateFilters(RequestBoardFilters filters) {
    setState(() => _filters = filters);
    widget.onFiltersChanged?.call(filters);
  }

  void _cycleThemeMode() {
    final onChanged = widget.onThemeModeChanged;
    if (onChanged == null) return;
    const order = [ThemeMode.system, ThemeMode.light, ThemeMode.dark];
    final next = order[(order.indexOf(widget.themeMode) + 1) % order.length];
    onChanged(next);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('Requests'),
        actions: [
          if (widget.onOpenNewRequest != null)
            TextButton.icon(
              key: const ValueKey('open-new-request-button'),
              onPressed: widget.onOpenNewRequest,
              icon: const Icon(Icons.add),
              label: const Text('New request'),
            ),
          if (widget.onOpenTriage != null)
            TextButton.icon(
              key: const ValueKey('open-triage-button'),
              onPressed: widget.onOpenTriage,
              icon: const Icon(Icons.checklist),
              label: const Text('Triage'),
            ),
          TextButton.icon(
            key: const ValueKey('run-list-button'),
            onPressed: () {
              Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) => RunListScreen(api: widget.api),
                ),
              );
            },
            icon: const Icon(Icons.list_alt),
            label: const Text('Runs'),
          ),
          if (widget.onThemeModeChanged != null)
            IconButton(
              key: const ValueKey('theme-mode-toggle'),
              onPressed: _cycleThemeMode,
              tooltip: switch (widget.themeMode) {
                ThemeMode.system => 'Theme: system (tap for light)',
                ThemeMode.light => 'Theme: light (tap for dark)',
                ThemeMode.dark => 'Theme: dark (tap for system)',
              },
              icon: Icon(switch (widget.themeMode) {
                ThemeMode.system => Icons.brightness_auto,
                ThemeMode.light => Icons.light_mode,
                ThemeMode.dark => Icons.dark_mode,
              }),
            ),
          IconButton(
            key: const ValueKey('request-list-refresh-button'),
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
    final requests = _requests;
    if (requests == null) {
      final error = _error;
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

    final sorted = sortedRequests(
      requests.where((r) => matchesRequestBoardFilters(r, _filters)).toList(),
    );
    // Split (not re-sorted) into the three section buckets -- the sort
    // within each group is unchanged: sortedRequests above is
    // still the single source of ordering, this just partitions its
    // already-correct output by display section.
    final sections = {
      for (final section in RequestBoardSection.values)
        section: sorted.where((r) => sectionForRequest(r) == section).toList(),
    };

    final freshness = requestBoardFreshness(
      sseConnected: _sseConnected,
      sseFailureCount: _sseFailureCount,
    );
    return Column(
      children: [
        // The release-policy visibility gap: the server's own
        // console-config.json reports when its configured
        // release policy can never allow a release decision (every
        // -release-* value/session-config key still at a deny-all
        // default). Without this strip, an operator watching this board
        // only ever learns that after a run is accepted and no PR
        // appears -- this surfaces the same reason `factoryd doctor`
        // warns about, plus the fix, before that ever happens.
        if (widget.api.releasePolicyWarning != null)
          MaterialBanner(
            key: const ValueKey('release-policy-warning-banner'),
            content: Text(
              'Release policy denies every PR: '
              '${widget.api.releasePolicyWarning}',
            ),
            actions: const [SizedBox.shrink()],
          ),
        // worker is what actually advances a request/run forward
        // between polls -- without this, a request stuck because it's
        // down reads on this board exactly like one making ordinary
        // progress. See _queueRunWarning's own doc comment for when this
        // shows (and, just as deliberately, when it stays hidden).
        if (_queueRunWarning != null)
          MaterialBanner(
            key: const ValueKey('worker-down-banner'),
            content: Text(_queueRunWarning!),
            actions: [
              TextButton(
                onPressed: _loadQueueRunStatus,
                child: const Text('Retry'),
              ),
            ],
          ),
        Padding(
          key: const ValueKey('board-freshness'),
          padding: const EdgeInsets.fromLTRB(16, 8, 16, 0),
          child: Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              Icon(
                switch (freshness) {
                  RequestBoardFreshness.live => Icons.circle,
                  RequestBoardFreshness.recent => Icons.sync,
                  RequestBoardFreshness.disconnected =>
                    Icons.cloud_off_outlined,
                },
                size: 10,
                color: switch (freshness) {
                  RequestBoardFreshness.live => Colors.green,
                  RequestBoardFreshness.recent => Colors.grey,
                  RequestBoardFreshness.disconnected => Colors.grey,
                },
              ),
              const SizedBox(width: 6),
              Text(
                freshnessLabel(freshness, _lastUpdateAt, DateTime.now()),
                style: Theme.of(context).textTheme.bodySmall,
              ),
            ],
          ),
        ),
        // "Needs you" strip (Phase 4, follow-along console): a single
        // banner naming how many requests are waiting on a review, that
        // taps into the board's own existing needsYou filter chip rather
        // than inventing a second way to reach the same view. Hidden once
        // that filter is already selected -- a banner telling you you're
        // already looking at what it points to is just noise.
        if (needsHumanCount(requests) > 0 &&
            _filters.section != RequestBoardSection.needsYou)
          MaterialBanner(
            key: const ValueKey('needs-you-banner'),
            content: Text(
              '${needsHumanCount(requests)} request(s) waiting for your review',
            ),
            actions: [
              TextButton(
                onPressed: () => _updateFilters(
                  _filters.copyWith(section: RequestBoardSection.needsYou),
                ),
                child: const Text('View'),
              ),
            ],
          ),
        _FilterBar(
          allProjects: distinctProjects(requests),
          filters: _filters,
          searchController: _searchController,
          onChanged: _updateFilters,
        ),
        if (_error != null)
          MaterialBanner(
            key: const ValueKey('request-list-stale-banner'),
            content: Text(
              'Showing the last successfully loaded data -- '
              'refresh failed: ${describeError(_error!).headline}',
            ),
            actions: [
              TextButton(
                onPressed: _loading ? null : _load,
                child: const Text('Retry'),
              ),
            ],
          ),
        Expanded(
          child: sorted.isEmpty
              ? const Center(child: Text('No requests found.'))
              : CustomScrollView(
                  slivers: [
                    for (final section in RequestBoardSection.values)
                      if (sections[section]!.isNotEmpty) ...[
                        SliverPersistentHeader(
                          pinned: true,
                          delegate: _SectionHeaderDelegate(
                            title:
                                '${sectionLabels[section]} '
                                '(${sections[section]!.length})',
                          ),
                        ),
                        SliverList.separated(
                          itemCount: sections[section]!.length,
                          separatorBuilder: (_, _) => const Divider(height: 1),
                          itemBuilder: (context, index) {
                            final request = sections[section]![index];
                            return _RequestRow(
                              request: request,
                              onTap: () {
                                Navigator.of(context).push(
                                  MaterialPageRoute<void>(
                                    builder: (_) => RequestDetailScreen(
                                      api: widget.api,
                                      requestId: request.id,
                                      initialRequest: request,
                                    ),
                                  ),
                                );
                              },
                            );
                          },
                        ),
                      ],
                  ],
                ),
        ),
      ],
    );
  }
}

/// The delegate behind each section's sticky header: three sticky
/// section headers with counts. A fixed, small
/// height is enough for a single line of title text -- this console has
/// no need for a taller collapsing header.
class _SectionHeaderDelegate extends SliverPersistentHeaderDelegate {
  const _SectionHeaderDelegate({required this.title});

  final String title;

  static const _height = 40.0;

  @override
  double get minExtent => _height;
  @override
  double get maxExtent => _height;

  @override
  Widget build(
    BuildContext context,
    double shrinkOffset,
    bool overlapsContent,
  ) {
    return Container(
      height: _height,
      alignment: Alignment.centerLeft,
      padding: const EdgeInsets.symmetric(horizontal: 16),
      color: Theme.of(context).colorScheme.surfaceContainerHighest,
      child: Text(title, style: Theme.of(context).textTheme.titleSmall),
    );
  }

  @override
  bool shouldRebuild(covariant _SectionHeaderDelegate oldDelegate) =>
      oldDelegate.title != title;
}

/// The filter bar: a project multi-select, a
/// single-select state-group filter, and free-text search. Pure
/// client-side filtering over already-fetched data -- see
/// request_board_filters.dart's own doc comments for the matching/URL
/// logic this only renders controls for.
class _FilterBar extends StatelessWidget {
  const _FilterBar({
    required this.allProjects,
    required this.filters,
    required this.searchController,
    required this.onChanged,
  });

  final List<String> allProjects;
  final RequestBoardFilters filters;
  final TextEditingController searchController;
  final ValueChanged<RequestBoardFilters> onChanged;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(16, 12, 16, 8),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          TextField(
            key: const ValueKey('request-search-field'),
            controller: searchController,
            decoration: const InputDecoration(
              isDense: true,
              prefixIcon: Icon(Icons.search),
              hintText: 'Search id, title, workspace',
              border: OutlineInputBorder(),
            ),
            onChanged: (value) => onChanged(filters.copyWith(search: value)),
          ),
          const SizedBox(height: 8),
          Wrap(
            spacing: 8,
            runSpacing: 4,
            crossAxisAlignment: WrapCrossAlignment.center,
            children: [
              for (final section in RequestBoardSection.values)
                ChoiceChip(
                  key: ValueKey('request-filter-group-${section.name}'),
                  label: Text(sectionLabels[section]!),
                  selected: filters.section == section,
                  onSelected: (selected) => onChanged(
                    filters.copyWith(section: selected ? section : null),
                  ),
                ),
              if (allProjects.isNotEmpty) const VerticalDivider(width: 16),
              for (final project in allProjects)
                FilterChip(
                  key: ValueKey('request-filter-project-$project'),
                  label: Text(project),
                  selected: filters.projects.contains(project),
                  onSelected: (selected) {
                    final projects = Set<String>.of(filters.projects);
                    if (selected) {
                      projects.add(project);
                    } else {
                      projects.remove(project);
                    }
                    onChanged(filters.copyWith(projects: projects));
                  },
                ),
            ],
          ),
        ],
      ),
    );
  }
}

class _RequestRow extends StatelessWidget {
  const _RequestRow({required this.request, required this.onTap});

  final RequestSummary request;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    final badge = waitingBadgeLabel(request, DateTime.now());
    // A derived, plain-text short title, not request.md's first
    // line verbatim (which could carry backticks/markdown straight onto
    // the board row) -- see RequestSummary.shortTitle's own doc comment.
    // Free-text search still matches the full raw title/id/workspace
    // (request_board_filters.dart's own filter, unaffected by how this
    // row displays it); the full, untruncated title is still one tap/
    // hover away via the tooltip below.
    final title = request.shortTitle;
    // The ticket fan-out roll-up is only shown for
    // requests actively building/in PR review -- earlier states have no
    // tickets yet, and computeTicketRollup already renders nothing for an
    // empty ticket list, so this condition is a display choice (don't
    // show a strip that would be empty for e.g. a "done" request whose
    // tickets are all merged) rather than one computeTicketRollup itself
    // needs.
    final showRollup =
        request.state == 'building' || request.state == 'pr_review';
    final tokenTotal = requestTokenTotal(request);
    final cost = request.costSummary;
    return ListTile(
      key: ValueKey('request-${request.id}'),
      onTap: onTap,
      isThreeLine: true,
      title: Tooltip(
        message: request.title.isNotEmpty ? request.title : request.id,
        child: Text(title),
      ),
      subtitle: Padding(
        padding: const EdgeInsets.only(top: 4),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Wrap(
              spacing: 8,
              runSpacing: 4,
              crossAxisAlignment: WrapCrossAlignment.center,
              children: [
                Text(request.project),
                if (request.ticketCount > 0)
                  Text(
                    'Ticket ${request.ticketIndex} / ${request.ticketCount}',
                  ),
                // Inline usage figure: the server-computed cost_summary
                // (model id + tokens spent -- no dollar figure, the
                // operator explicitly does not want one here) when the
                // list route provided one, falling back to an older
                // token-total proxy otherwise (an older server build, or
                // -- see request_cost.dart's doc comment -- any response
                // this field is absent from).
                Text(
                  key: cost != null
                      ? const ValueKey('request-cost-total')
                      : const ValueKey('request-token-total'),
                  cost != null
                      ? formatUsageSummary(cost)
                      : formatRequestTokenTotal(tokenTotal),
                  style: Theme.of(context).textTheme.bodySmall,
                ),
                // Rejection count marker: a request
                // on its Nth rejected draft is a signal in itself, worth
                // seeing without opening the detail screen.
                if (request.rejections.isNotEmpty)
                  Text(
                    key: const ValueKey('rejection-marker'),
                    '↻${request.rejections.length}',
                    style: Theme.of(context).textTheme.bodySmall,
                  ),
                // A PR state with no PR URL is a record from before the
                // server stopped writing "draft" for an unopened PR
                // (2026-09-24): no chip for a PR that does not exist.
                for (final ticket in request.tickets)
                  if (ticket.prState.isNotEmpty && ticket.prUrl.isNotEmpty)
                    PrStateChip(prState: ticket.prState),
              ],
            ),
            if (showRollup)
              Padding(
                padding: const EdgeInsets.only(top: 4),
                child: TicketRollupStrip(request: request),
              ),
          ],
        ),
      ),
      trailing: Wrap(
        spacing: 8,
        crossAxisAlignment: WrapCrossAlignment.center,
        children: [
          if (badge != null)
            Chip(
              key: const ValueKey('waiting-badge'),
              label: Text(badge, style: const TextStyle(color: Colors.white)),
              backgroundColor: Colors.deepOrange,
            ),
          RequestStageChip(
            state: request.state,
            awaitingPullRequest: request.awaitingPullRequest,
            waitingOn: request.waitingOn,
            needsYou: requestStageGroupOf(request) == RequestStageGroup.review,
          ),
        ],
      ),
    );
  }
}

class RequestStageChip extends StatelessWidget {
  const RequestStageChip({
    required this.state,
    this.awaitingPullRequest = false,
    this.waitingOn,
    this.needsYou = false,
    super.key,
  });

  final String state;

  /// A halted request that is really accepted work awaiting its pull request
  /// (request.awaitingPullRequest): a calm label and colour, not "halted".
  final bool awaitingPullRequest;

  /// The id of the request/run running ahead of this one -- set only while
  /// this request is queued behind it (RequestSummary.waitingOn). Shown in
  /// place of the plain "Building" label: found in the operator demo (C5),
  /// a request stalled behind another worker build showed a "Building"
  /// chip indistinguishable from one actually making progress.
  final String? waitingOn;

  /// Whether the request waits on a human (requestStageGroupOf ==
  /// review): a pr_review request whose PRs all wait on their reviewer
  /// gets the needs-you colour, not the working spinner its state alone
  /// would give, so the chip agrees with the board section it sits in.
  final bool needsYou;

  @override
  Widget build(BuildContext context) {
    // Colors/icon come from status.dart's shared vocabulary rather than
    // this screen's own switch over RequestStageGroup --
    // requestStageGroup itself stays (sortedRequests/_stageRank still need
    // the board's own review/working/done/failed/other grouping), but the
    // chip's own color must agree with every other chip in the console, not
    // just this screen's grouping.
    if (awaitingPullRequest) {
      return const StatusChip(
        key: ValueKey('request-state-awaiting-pr'),
        status: Status.done,
        label: 'accepted · awaiting PR',
      );
    }
    final behind = waitingOn;
    if (behind != null && behind.isNotEmpty) {
      return StatusChip(
        key: const ValueKey('request-state-waiting-on'),
        status: statusForToken(state),
        label: 'Queued behind ${_shortWaitingOnId(behind)}',
      );
    }
    return StatusChip(
      key: ValueKey('request-state-$state'),
      status: needsYou ? Status.needsHuman : statusForToken(state),
      label: state,
    );
  }
}

/// A short display form of a request/run id for the "Queued behind ..."
/// chip label -- ids can run to the full `GenerateID` slug-plus-timestamp
/// length, too long for a chip.
String _shortWaitingOnId(String id) =>
    id.length > 12 ? '${id.substring(0, 12)}…' : id;

/// A small chip for one ticket's own PR review state (`internal/request.
/// Ticket.PRState`, populated once the PR-review poll loop starts reading
/// it).
/// Distinct from RequestStageChip -- a request's overall stage chip and
/// one ticket's PR status are different pieces of information that can
/// both be true at once (e.g. a request in pr_review with one ticket's PR
/// already approved).
class PrStateChip extends StatelessWidget {
  const PrStateChip({required this.prState, super.key});

  final String prState;

  @override
  Widget build(BuildContext context) {
    // Label text is unchanged from before status.dart existed; only the
    // color now comes from the shared vocabulary so
    // "red" means the same thing here as it does on every other chip.
    final label = switch (prState) {
      'approved' => 'PR approved',
      'ready' => 'PR ready for review',
      'stacked' => 'PR stacked · merge its base first',
      'changes_requested' => 'Changes requested',
      'merged' => 'PR merged',
      'closed' => 'PR closed',
      'open' => 'PR open',
      'draft' => 'PR draft',
      _ => 'PR $prState',
    };
    final status = statusForPRState(prState);
    // Delegate to the shared StatusChip rather than building a Chip
    // directly, so an unrecognized PR state gets the same visually
    // distinct outlined/transparent Status.unknown treatment every other
    // status surface uses -- building this chip by hand previously always
    // painted a plain filled grey chip for an unmapped value, defeating
    // the point of a shared status vocabulary for this one surface.
    return KeyedSubtree(
      key: ValueKey('pr-state-$prState'),
      child: StatusChip(status: status, label: label),
    );
  }
}
