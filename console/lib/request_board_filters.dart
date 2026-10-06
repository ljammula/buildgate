import 'models.dart';
import 'request_list_screen.dart'
    show requestStageGroup, requestStageGroupOf, RequestStageGroup;

/// The three sections the request board groups into. Distinct from
/// [RequestStageGroup] (which [requestStageGroup] computes per-state, and
/// which [requestListAutoRefreshInterval]'s own sort still uses
/// unchanged): [RequestStageGroup.done], [.failed], and [.other] all
/// collapse into [RequestBoardSection.finished] here, matching a
/// three-header example ("Needs you (3)", "Working (7)", "Finished (41)")
/// rather than a fifth "other" bucket nobody asked to see.
enum RequestBoardSection { needsYou, working, finished }

/// section computes which board section [state] belongs in -- the display
/// grouping for the board's sticky headers, layered on top of the
/// pre-existing [requestStageGroup] rather than replacing it (that
/// function's own five-way rank is still what [sortedRequests] sorts by;
/// left unchanged).
RequestBoardSection sectionForState(String state) =>
    _sectionForGroup(requestStageGroup(state));

/// sectionForRequest is [sectionForState] for a whole request (see
/// [requestStageGroupOf]): a `pr_review` request waiting only on its
/// reviewer shows under Needs you. The board and its section filter both
/// use this, so a filter never disagrees with the header it matches.
RequestBoardSection sectionForRequest(RequestSummary request) =>
    _sectionForGroup(requestStageGroupOf(request));

RequestBoardSection _sectionForGroup(RequestStageGroup group) =>
    switch (group) {
      RequestStageGroup.review => RequestBoardSection.needsYou,
      RequestStageGroup.working => RequestBoardSection.working,
      RequestStageGroup.done ||
      RequestStageGroup.failed ||
      RequestStageGroup.other => RequestBoardSection.finished,
    };

const Map<RequestBoardSection, String> sectionLabels = {
  RequestBoardSection.needsYou: 'Needs you',
  RequestBoardSection.working: 'Working',
  RequestBoardSection.finished: 'Finished',
};

// The query-string spelling for each section, used by
// [RequestBoardFilters.toQueryParameters]/[RequestBoardFilters.fromUri] --
// kept distinct from the enum's own Dart identifier so the URL stays
// stable even if the enum is ever renamed.
const Map<RequestBoardSection, String> _sectionQueryValues = {
  RequestBoardSection.needsYou: 'needs-you',
  RequestBoardSection.working: 'working',
  RequestBoardSection.finished: 'finished',
};
final Map<String, RequestBoardSection> _sectionByQueryValue = {
  for (final entry in _sectionQueryValues.entries) entry.value: entry.key,
};

/// The request board's filter state: a project multi-select, an optional
/// single section filter, and free-text search. Immutable and
/// round-trips through a URL query string via [fromUri]/
/// [toQueryParameters] so the board's filters survive a reload and are
/// shareable -- no server-side filtering, no persistence beyond the URL
/// itself.
class RequestBoardFilters {
  const RequestBoardFilters({
    this.projects = const {},
    this.section,
    this.search = '',
  });

  /// Empty means "every project" -- not "no project", which would be an
  /// unreachable filter state for an operator to select their way into.
  final Set<String> projects;

  /// Null means "every section".
  final RequestBoardSection? section;
  final String search;

  bool get isEmpty => projects.isEmpty && section == null && search.isEmpty;

  RequestBoardFilters copyWith({
    Set<String>? projects,
    // A sentinel is unnecessary here (unlike section, clearing the search
    // is just passing ''), but section needs one to distinguish "leave
    // section alone" from "clear section back to null" -- copyWith(section:
    // null) would otherwise be ambiguous between the two.
    Object? section = _unset,
    String? search,
  }) => RequestBoardFilters(
    projects: projects ?? this.projects,
    section: identical(section, _unset)
        ? this.section
        : section as RequestBoardSection?,
    search: search ?? this.search,
  );

  factory RequestBoardFilters.fromUri(Uri uri) {
    // Repeated ?project=a&project=b params, not one comma-joined value --
    // see toQueryParameters' own doc comment for why.
    final projectParams = uri.queryParametersAll['project'] ?? const [];
    final groupParam = uri.queryParameters['group'];
    return RequestBoardFilters(
      projects: projectParams.where((p) => p.isNotEmpty).toSet(),
      section: groupParam == null ? null : _sectionByQueryValue[groupParam],
      search: uri.queryParameters['q'] ?? '',
    );
  }

  /// dynamic, not String: Uri's own `queryParameters` constructor
  /// parameter accepts an `Iterable<String>` value to mean "repeat this
  /// key once per element" -- used here for `project` so a project name
  /// that itself contains a comma round-trips correctly. Found in
  /// review: the earlier single comma-joined value made a project named
  /// e.g. "billing,legacy" indistinguishable from two projects "billing"
  /// and "legacy" once reloaded from that URL -- there is no way to
  /// escape a comma within one joined value and also use comma as the
  /// delimiter between values. Repeated keys have no such ambiguity: each
  /// occurrence is one full, separately-encoded value.
  Map<String, dynamic> toQueryParameters() => {
    if (projects.isNotEmpty) 'project': projects.toList()..sort(),
    if (section != null) 'group': _sectionQueryValues[section]!,
    if (search.isNotEmpty) 'q': search,
  };

  @override
  bool operator ==(Object other) =>
      other is RequestBoardFilters &&
      other.projects.length == projects.length &&
      other.projects.containsAll(projects) &&
      other.section == section &&
      other.search == search;

  @override
  int get hashCode =>
      Object.hash(Object.hashAllUnordered(projects), section, search);
}

const _unset = Object();

/// True when [request] matches every active filter in [filters] -- pure
/// client-side filtering over already-fetched data; does not add a
/// server-side filter.
bool matchesRequestBoardFilters(
  RequestSummary request,
  RequestBoardFilters filters,
) {
  if (filters.projects.isNotEmpty &&
      !filters.projects.contains(request.project)) {
    return false;
  }
  if (filters.section != null &&
      sectionForRequest(request) != filters.section) {
    return false;
  }
  if (filters.search.isNotEmpty) {
    final needle = filters.search.toLowerCase();
    final haystack = '${request.id} ${request.title} ${request.workspace}'
        .toLowerCase();
    if (!haystack.contains(needle)) return false;
  }
  return true;
}

/// Every distinct project value across [requests], sorted -- the
/// project multi-select's own option list, multi-select from distinct
/// project values in the response.
List<String> distinctProjects(List<RequestSummary> requests) =>
    (requests.map((r) => r.project).toSet().toList()..sort());
