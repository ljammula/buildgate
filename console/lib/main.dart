import 'package:flutter/material.dart';
import 'package:flutter_web_plugins/url_strategy.dart';

import 'api_client.dart';
import 'request_board_route.dart';
import 'start_token.dart';
import 'theme_mode_store.dart';

const _apiBaseUrl = String.fromEnvironment(
  'API_BASE_URL',
  defaultValue: 'http://localhost:8090',
);
// Kept as a fallback for a deployment that genuinely uses one shared token
// for both endpoints; API_START_TOKEN/API_OVERRIDE_TOKEN below take
// precedence when set, matching the server's own independently
// configurable FACTORYD_API_START_TOKEN/FACTORYD_API_OVERRIDE_TOKEN.
const _apiAuthToken = String.fromEnvironment('API_AUTH_TOKEN');
const _apiStartToken = String.fromEnvironment('API_START_TOKEN');
const _apiOverrideToken = String.fromEnvironment('API_OVERRIDE_TOKEN');
// FACTORYD_API_READ_TOKEN's client-side counterpart -- unset (the default)
// matches the server's own open-unless-configured default for its read
// routes (see api.WithReadToken's doc comment).
const _apiReadToken = String.fromEnvironment('API_READ_TOKEN');

Future<void> main() async {
  // Plain paths (/requests/{id}, not /#/requests/{id}) are the whole
  // point of the console's deep links -- RequestBoardRouteInformationParser/
  // RequestBoardRouteConfig.toUri() writes them that way. Flutter Web's
  // default hash strategy
  // silently ignores the path portion of a fresh navigation, so without
  // this a request board reload lost its own filters/route on every
  // refresh, not just an externally shared link. A no-op on non-web
  // platforms.
  usePathUrlStrategy();
  // Capture `factoryd serve`'s own per-process start token (F: serve-
  // start-token) from the `#t=<token>` fragment its printed console link
  // carries, before anything else reads the URL -- must run ahead of
  // usePathUrlStrategy's own first navigation and RunApi's construction
  // below, which needs the result. A no-op when the hash carries no `t=`
  // component (already captured on an earlier load, or a build-time token
  // is in play instead) -- see start_token.dart's own doc comment.
  captureStartTokenFromLocation();
  // Build-time API_START_TOKEN/API_AUTH_TOKEN (below) always wins when
  // set -- an operator who deliberately baked in a shared token via
  // make console-build gets exactly that behavior, unchanged. Only when
  // neither is set does a token captured just above (or on an earlier
  // load of this same browser) become the start token, so a default
  // install's New run/release/stats/ops screens work without any
  // dart-define at all.
  final storedStartToken = getStoredStartToken();
  // GET /console-config.json, fetched once here before the app renders:
  // whether the server accepts an unauthenticated write from this
  // console's own origin, and the Temporal UI base URL if configured. A
  // server predating this route (or unreachable) reads as "writes not
  // enabled" -- RunApi.canWrite then falls back to whether a build-time
  // override token exists, exactly as before this route existed.
  final config = await RunApi.fetchConsoleConfig(_apiBaseUrl);
  runApp(
    FactoryConsole(
      api: RunApi(
        baseUrl: _apiBaseUrl,
        authToken: _apiAuthToken,
        startToken: _apiStartToken.isNotEmpty
            ? _apiStartToken
            : (_apiAuthToken.isNotEmpty ? null : storedStartToken),
        overrideToken: _apiOverrideToken.isNotEmpty ? _apiOverrideToken : null,
        readToken: _apiReadToken.isNotEmpty ? _apiReadToken : null,
        writesEnabled: config.writesEnabled,
        temporalUiUrl: config.temporalUiUrl,
        releasePolicyWarning: config.releasePolicyWarning,
      ),
    ),
  );
}

class FactoryConsole extends StatefulWidget {
  const FactoryConsole({required this.api, super.key});

  final RunApi api;

  @override
  State<FactoryConsole> createState() => _FactoryConsoleState();
}

class _FactoryConsoleState extends State<FactoryConsole> {
  // Dark mode: the router delegate and its route information parser are
  // created once, here, rather than fresh on every
  // build -- MaterialApp.router expects both to stay identity-stable
  // across rebuilds so the Navigator underneath keeps its own state
  // (open screens, scroll positions, ...) instead of being torn down and
  // recreated every time this widget rebuilds, which a manual theme
  // toggle now does.
  late final RequestBoardRouterDelegate _routerDelegate;
  final _routeParser = RequestBoardRouteInformationParser();
  late ThemeMode _themeMode;

  @override
  void initState() {
    super.initState();
    _themeMode = getStoredThemeMode() ?? ThemeMode.system;
    _routerDelegate = RequestBoardRouterDelegate(
      api: widget.api,
      themeMode: _themeMode,
      onThemeModeChanged: _setThemeMode,
    );
  }

  void _setThemeMode(ThemeMode mode) {
    setState(() => _themeMode = mode);
    _routerDelegate.updateThemeMode(mode);
    setStoredThemeMode(mode);
  }

  @override
  Widget build(BuildContext context) {
    // The request board is the landing screen: every operator-
    // submitted request's state, with a link to the run list (below) in
    // its own app bar for the pre-existing per-run view. Every route this
    // console had before the request board became the landing screen is
    // unchanged -- reached the same way, from RunListScreen onward.
    //
    // MaterialApp.router replaces the plain MaterialApp/`home:` this app
    // used before, so the request
    // board's own filter state can round-trip through the URL's query
    // string and several other screens are directly addressable by URL --
    // see request_board_route.dart's own doc comment for exactly how much
    // of the app that does, and does not, cover.
    return MaterialApp.router(
      title: 'Factory Console',
      theme: ThemeData(brightness: Brightness.light, useMaterial3: true),
      darkTheme: ThemeData(brightness: Brightness.dark, useMaterial3: true),
      themeMode: _themeMode,
      routerDelegate: _routerDelegate,
      routeInformationParser: _routeParser,
    );
  }
}
