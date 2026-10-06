// The minimal `Router` plumbing this app needs: MaterialApp.router with
// a minimal RouteInformationParser (no go_router dependency, added only
// if deep links need it) so the request board's own filter state
// (project/section/search) round-trips through the browser's URL query
// string, extended to make `/requests/{id}`, `/runs/{id}`, and
// `/projects/{p}/release` each their own addressable URL too, and
// further extended to add `/runs`, `/projects/{p}/stats`, and `/ops` so
// reloading or typing any server-served console URL lands on the right
// screen instead of the board or raw JSON.
//
// Still deliberately narrow in one respect: only these extra screens are
// reachable by typed/bookmarked/deep-linked URL, each as a single page
// stacked on top of the board/triage page. Navigating to one of them
// *from inside the app* (a board row tap, a "View run" button,
// ...) still uses the pre-existing imperative
// `Navigator.push(MaterialPageRoute(...))` calls, which does not update
// the address bar -- exactly as before this file's own deep-link extension.
// That asymmetry is accepted, not a gap: the plan's own goal ("so the
// HITL reminder emails ... can link straight to the waiting request") is
// about a URL landing on the right screen, not about every in-app click
// growing browser history.

import 'package:flutter/material.dart';

import 'api_client.dart';
import 'new_request_screen.dart';
import 'ops_screen.dart';
import 'project_release_screen.dart';
import 'project_stats_screen.dart';
import 'request_board_filters.dart';
import 'request_detail_screen.dart';
import 'request_list_screen.dart';
import 'run_detail_screen.dart';
import 'run_list_screen.dart';
import 'triage_screen.dart';

/// Which extra, URL-addressable screen (extended for `/runs`,
/// `/projects/{p}/stats`, and `/ops`) is currently stacked on top of the
/// board/triage page, if any. `runList` and `ops`
/// need no id (same shape as `triage` below, just folded into this enum
/// rather than a second boolean flag, since -- unlike `triage` -- they are
/// each a *stacked* page over the board, not one of the router's two base
/// pages); their `deepLinkId` is always null.
enum RequestBoardDeepLink {
  none,
  requestDetail,
  runDetail,
  projectRelease,
  runList,
  projectStats,
  ops,
  newRequest,
}

/// The router-owned state: the request board's own filters (serialized
/// to/from the URL query string), which of the router's two base pages
/// (the board, or `/triage`) is current, and optionally which deep-link
/// screen is stacked on top.
class RequestBoardRouteConfig {
  const RequestBoardRouteConfig(this.filters)
    : triage = false,
      deepLink = RequestBoardDeepLink.none,
      deepLinkId = null;

  const RequestBoardRouteConfig.triage()
    : filters = const RequestBoardFilters(),
      triage = true,
      deepLink = RequestBoardDeepLink.none,
      deepLinkId = null;

  const RequestBoardRouteConfig.requestDetail(String id)
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.requestDetail,
      deepLinkId = id;

  const RequestBoardRouteConfig.runDetail(String id)
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.runDetail,
      deepLinkId = id;

  const RequestBoardRouteConfig.projectRelease(String project)
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.projectRelease,
      deepLinkId = project;

  const RequestBoardRouteConfig.runList()
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.runList,
      deepLinkId = null;

  const RequestBoardRouteConfig.projectStats(String project)
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.projectStats,
      deepLinkId = project;

  const RequestBoardRouteConfig.ops()
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.ops,
      deepLinkId = null;

  const RequestBoardRouteConfig.newRequest()
    : filters = const RequestBoardFilters(),
      triage = false,
      deepLink = RequestBoardDeepLink.newRequest,
      deepLinkId = null;

  final RequestBoardFilters filters;
  final bool triage;
  final RequestBoardDeepLink deepLink;
  final String? deepLinkId;

  Uri toUri() {
    // Uri.parse, not Uri(path: ...), for a segment built with
    // Uri.encodeComponent. Found in review: Uri(path: ...)'s own `path`
    // parameter is raw, unencoded text that the constructor
    // percent-encodes itself, so pre-encoding an id with
    // Uri.encodeComponent and then passing the result through Uri(path:
    // ...) encoded it a second time -- "my%20repo" came out as
    // "my%2520repo". Uri.parse takes its input as already-encoded and
    // does not re-escape it, so encoding happens exactly once here.
    switch (deepLink) {
      case RequestBoardDeepLink.requestDetail:
        return Uri.parse('/requests/${Uri.encodeComponent(deepLinkId!)}');
      case RequestBoardDeepLink.runDetail:
        return Uri.parse('/runs/${Uri.encodeComponent(deepLinkId!)}');
      case RequestBoardDeepLink.projectRelease:
        return Uri.parse(
          '/projects/${Uri.encodeComponent(deepLinkId!)}/release',
        );
      case RequestBoardDeepLink.runList:
        return Uri(path: '/runs');
      case RequestBoardDeepLink.projectStats:
        return Uri.parse('/projects/${Uri.encodeComponent(deepLinkId!)}/stats');
      case RequestBoardDeepLink.ops:
        return Uri(path: '/ops');
      case RequestBoardDeepLink.newRequest:
        return Uri(path: '/requests/new');
      case RequestBoardDeepLink.none:
        return triage
            ? Uri(path: '/triage')
            : Uri(path: '/', queryParameters: _orNull(filters));
    }
  }

  static Map<String, dynamic>? _orNull(RequestBoardFilters filters) {
    final params = filters.toQueryParameters();
    return params.isEmpty ? null : params;
  }
}

class RequestBoardRouteInformationParser
    extends RouteInformationParser<RequestBoardRouteConfig> {
  @override
  Future<RequestBoardRouteConfig> parseRouteInformation(
    RouteInformation routeInformation,
  ) async {
    final uri = routeInformation.uri;
    if (uri.path == '/triage') return const RequestBoardRouteConfig.triage();
    if (uri.path == '/runs') return const RequestBoardRouteConfig.runList();
    if (uri.path == '/ops') return const RequestBoardRouteConfig.ops();
    // Checked before the generic 2-segment "requests"/{id} branch below:
    // `/requests/new` would otherwise parse as requestDetail(id: 'new')
    // and RequestDetailScreen would try (and fail) to GET /requests/new
    // as if "new" were a real request id. The server-side ambiguity this
    // mirrors -- `GET /requests/{id}` also matching this same path -- is
    // already resolved by consoleDeepLinkPatterns (internal/api/
    // server.go), which serves the console shell for any browser
    // navigation matching that pattern, "new" included; only this
    // client-side parse needs its own special case.
    if (uri.path == '/requests/new') {
      return const RequestBoardRouteConfig.newRequest();
    }
    final segments = uri.pathSegments;
    if (segments.length == 2 && segments[0] == 'requests') {
      return RequestBoardRouteConfig.requestDetail(segments[1]);
    }
    if (segments.length == 2 && segments[0] == 'runs') {
      return RequestBoardRouteConfig.runDetail(segments[1]);
    }
    if (segments.length == 3 &&
        segments[0] == 'projects' &&
        segments[2] == 'release') {
      return RequestBoardRouteConfig.projectRelease(segments[1]);
    }
    if (segments.length == 3 &&
        segments[0] == 'projects' &&
        segments[2] == 'stats') {
      return RequestBoardRouteConfig.projectStats(segments[1]);
    }
    return RequestBoardRouteConfig(RequestBoardFilters.fromUri(uri));
  }

  @override
  RouteInformation restoreRouteInformation(
    RequestBoardRouteConfig configuration,
  ) => RouteInformation(uri: configuration.toUri());
}

class RequestBoardRouterDelegate extends RouterDelegate<RequestBoardRouteConfig>
    with
        ChangeNotifier,
        PopNavigatorRouterDelegateMixin<RequestBoardRouteConfig> {
  RequestBoardRouterDelegate({
    required this.api,
    ThemeMode themeMode = ThemeMode.system,
    this.onThemeModeChanged,
  }) : navigatorKey = GlobalKey<NavigatorState>(),
       _themeMode = themeMode;

  final RunApi api;
  @override
  final GlobalKey<NavigatorState> navigatorKey;

  RequestBoardFilters _filters = const RequestBoardFilters();
  bool _triage = false;
  ThemeMode _themeMode;
  RequestBoardDeepLink _deepLink = RequestBoardDeepLink.none;
  String? _deepLinkId;

  // Reports the operator's choice back to FactoryConsole, which owns
  // persisting it and actually setting MaterialApp's
  // own `themeMode:` -- this delegate only needs to know the current
  // value so the board screen it builds below can render the toggle's
  // icon correctly.
  final ValueChanged<ThemeMode>? onThemeModeChanged;

  /// Called by [FactoryConsole] after it persists a new theme mode, so
  /// this delegate's own board rebuilds with the icon reflecting it.
  void updateThemeMode(ThemeMode mode) {
    if (mode == _themeMode) return;
    _themeMode = mode;
    notifyListeners();
  }

  @override
  RequestBoardRouteConfig get currentConfiguration {
    switch (_deepLink) {
      case RequestBoardDeepLink.requestDetail:
        return RequestBoardRouteConfig.requestDetail(_deepLinkId!);
      case RequestBoardDeepLink.runDetail:
        return RequestBoardRouteConfig.runDetail(_deepLinkId!);
      case RequestBoardDeepLink.projectRelease:
        return RequestBoardRouteConfig.projectRelease(_deepLinkId!);
      case RequestBoardDeepLink.runList:
        return const RequestBoardRouteConfig.runList();
      case RequestBoardDeepLink.projectStats:
        return RequestBoardRouteConfig.projectStats(_deepLinkId!);
      case RequestBoardDeepLink.ops:
        return const RequestBoardRouteConfig.ops();
      case RequestBoardDeepLink.newRequest:
        return const RequestBoardRouteConfig.newRequest();
      case RequestBoardDeepLink.none:
        return _triage
            ? const RequestBoardRouteConfig.triage()
            : RequestBoardRouteConfig(_filters);
    }
  }

  @override
  Future<void> setNewRoutePath(RequestBoardRouteConfig configuration) async {
    _deepLink = configuration.deepLink;
    _deepLinkId = configuration.deepLinkId;
    if (_deepLink == RequestBoardDeepLink.none) {
      _triage = configuration.triage;
      if (!configuration.triage) _filters = configuration.filters;
    }
  }

  /// Called by [RequestListScreen] whenever the operator changes a
  /// filter -- updates this delegate's own notion of the current route
  /// (which the framework then asks [currentConfiguration] for, and
  /// reflects into the URL via [RequestBoardRouteInformationParser]) so
  /// the URL and the on-screen filter state can never drift apart.
  void updateFilters(RequestBoardFilters filters) {
    if (filters == _filters) return;
    _filters = filters;
    notifyListeners();
  }

  /// Switches this router's one page to `/triage`.
  void openTriage() {
    if (_triage) return;
    _triage = true;
    notifyListeners();
  }

  /// Switches this router's one page back to the board.
  void openBoard() {
    if (!_triage) return;
    _triage = false;
    notifyListeners();
  }

  /// Opens `/requests/new` (the "New request" app-bar button on the
  /// board) as a stacked deep-link page, the same way a typed/bookmarked
  /// URL for it would -- unlike NewRunScreen (opened via a plain
  /// imperative Navigator.push from project_list_screen.dart), this one
  /// updates the address bar, since scripts/console-walk's own walk.mjs
  /// needs `/requests/new` to be a real, navigable route.
  void openNewRequest() {
    if (_deepLink == RequestBoardDeepLink.newRequest) return;
    _deepLink = RequestBoardDeepLink.newRequest;
    _deepLinkId = null;
    notifyListeners();
  }

  /// Switches this router's deep-link page to `/requests/<id>` -- passed
  /// to NewRequestScreen as `onCreated` so a request created from
  /// `/requests/new` lands on its own addressable detail URL instead of
  /// a same-URL imperative push (see NewRequestScreen.onCreated's own
  /// doc comment for why the address bar must actually change here).
  void openRequestDetail(String id) {
    _deepLink = RequestBoardDeepLink.requestDetail;
    _deepLinkId = id;
    notifyListeners();
  }

  // _closeDeepLink drops the top deep-link page -- called from
  // onDidRemovePage below when the framework (a browser back navigation,
  // or the deep-linked screen's own back arrow going through the
  // Navigator's normal pop) removes it, so this delegate's own state
  // stays in sync with what's actually on screen.
  void _closeDeepLink() {
    if (_deepLink == RequestBoardDeepLink.none) return;
    _deepLink = RequestBoardDeepLink.none;
    _deepLinkId = null;
    notifyListeners();
  }

  @override
  Widget build(BuildContext context) {
    return Navigator(
      key: navigatorKey,
      pages: [
        MaterialPage<void>(
          key: const ValueKey('board-page'),
          child: _triage
              ? TriageScreen(api: api, onExit: openBoard)
              : RequestListScreen(
                  api: api,
                  filters: _filters,
                  onFiltersChanged: updateFilters,
                  onOpenTriage: openTriage,
                  onOpenNewRequest: openNewRequest,
                  themeMode: _themeMode,
                  onThemeModeChanged: onThemeModeChanged,
                ),
        ),
        // The deep-link page, present only when the current URL
        // (or an in-app deep link, if one is ever added) named one of
        // these screens directly. Everything else this console reaches
        // from inside a screen (a board row tap, "View run", ...) still
        // uses the pre-existing imperative Navigator.push, pushed onto
        // this same Navigator on top of whichever pages are declared
        // here -- unaffected by this list growing to two entries.
        if (_deepLink != RequestBoardDeepLink.none)
          MaterialPage<void>(
            key: ValueKey('deep-link-$_deepLink-$_deepLinkId'),
            child: switch (_deepLink) {
              RequestBoardDeepLink.requestDetail => RequestDetailScreen(
                api: api,
                requestId: _deepLinkId!,
              ),
              RequestBoardDeepLink.runDetail => RunDetailScreen(
                api: api,
                runId: _deepLinkId!,
              ),
              RequestBoardDeepLink.projectRelease => ProjectReleaseScreen(
                api: api,
                initialProject: _deepLinkId,
              ),
              RequestBoardDeepLink.runList => RunListScreen(api: api),
              RequestBoardDeepLink.projectStats => ProjectStatsScreen(
                api: api,
                initialProject: _deepLinkId,
              ),
              RequestBoardDeepLink.ops => OpsScreen(api: api),
              RequestBoardDeepLink.newRequest => NewRequestScreen(
                api: api,
                onCreated: openRequestDetail,
              ),
              RequestBoardDeepLink.none => throw StateError(
                'unreachable: guarded by the if above',
              ),
            },
          ),
      ],
      onDidRemovePage: (page) => _closeDeepLink(),
    );
  }
}
