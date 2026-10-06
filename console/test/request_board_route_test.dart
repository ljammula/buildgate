import 'package:console/request_board_filters.dart';
import 'package:console/request_board_route.dart';
import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  final parser = RequestBoardRouteInformationParser();

  // /requests/{id}, /runs/{id}, and
  // /projects/{p}/release must each be addressable by URL -- this is the
  // parsing half; RequestBoardRouterDelegate.build (exercised end-to-end
  // via request_list_screen_test.dart/request_detail_screen_test.dart's
  // own widget tests) is the half that turns a parsed config into the
  // matching screen.
  group('deep links', () {
    test('/requests/{id} parses to a requestDetail deep link', () async {
      final config = await parser.parseRouteInformation(
        RouteInformation(uri: Uri.parse('/requests/req-42')),
      );
      expect(config.deepLink, RequestBoardDeepLink.requestDetail);
      expect(config.deepLinkId, 'req-42');
    });

    test('/runs/{id} parses to a runDetail deep link', () async {
      final config = await parser.parseRouteInformation(
        RouteInformation(uri: Uri.parse('/runs/run-7')),
      );
      expect(config.deepLink, RequestBoardDeepLink.runDetail);
      expect(config.deepLinkId, 'run-7');
    });

    test(
      '/projects/{p}/release parses to a projectRelease deep link',
      () async {
        final config = await parser.parseRouteInformation(
          RouteInformation(uri: Uri.parse('/projects/checkouts/release')),
        );
        expect(config.deepLink, RequestBoardDeepLink.projectRelease);
        expect(config.deepLinkId, 'checkouts');
      },
    );

    test('each deep link round-trips through toUri', () {
      expect(
        const RequestBoardRouteConfig.requestDetail('req-42').toUri().path,
        '/requests/req-42',
      );
      expect(
        const RequestBoardRouteConfig.runDetail('run-7').toUri().path,
        '/runs/run-7',
      );
      expect(
        const RequestBoardRouteConfig.projectRelease('checkouts').toUri().path,
        '/projects/checkouts/release',
      );
    });

    test(
      'a deep-link id with an escapable character round-trips without '
      'double-encoding (regression: Uri(path: ...) over an already-'
      'Uri.encodeComponent-ed segment re-escapes the % it just added)',
      () async {
        const rawId = 'my repo';
        final uri = const RequestBoardRouteConfig.projectRelease(rawId).toUri();
        expect(uri.path, '/projects/my%20repo/release');

        final config = await parser.parseRouteInformation(
          RouteInformation(uri: uri),
        );
        expect(config.deepLink, RequestBoardDeepLink.projectRelease);
        expect(config.deepLinkId, rawId);
      },
    );

    test(
      '/requests/new parses to a newRequest deep link, not requestDetail(id: '
      '"new")',
      () async {
        final config = await parser.parseRouteInformation(
          RouteInformation(uri: Uri.parse('/requests/new')),
        );
        expect(config.deepLink, RequestBoardDeepLink.newRequest);
        expect(config.deepLinkId, isNull);
      },
    );

    test('the newRequest deep link round-trips through toUri', () {
      expect(
        const RequestBoardRouteConfig.newRequest().toUri().path,
        '/requests/new',
      );
    });

    test('/runs parses to a runList deep link', () async {
      final config = await parser.parseRouteInformation(
        RouteInformation(uri: Uri.parse('/runs')),
      );
      expect(config.deepLink, RequestBoardDeepLink.runList);
      expect(config.deepLinkId, isNull);
    });

    test('/ops parses to an ops deep link', () async {
      final config = await parser.parseRouteInformation(
        RouteInformation(uri: Uri.parse('/ops')),
      );
      expect(config.deepLink, RequestBoardDeepLink.ops);
      expect(config.deepLinkId, isNull);
    });

    test('/projects/{p}/stats parses to a projectStats deep link', () async {
      final config = await parser.parseRouteInformation(
        RouteInformation(uri: Uri.parse('/projects/checkouts/stats')),
      );
      expect(config.deepLink, RequestBoardDeepLink.projectStats);
      expect(config.deepLinkId, 'checkouts');
    });

    test('these deep links round-trip through toUri', () {
      expect(const RequestBoardRouteConfig.runList().toUri().path, '/runs');
      expect(const RequestBoardRouteConfig.ops().toUri().path, '/ops');
      expect(
        const RequestBoardRouteConfig.projectStats('checkouts').toUri().path,
        '/projects/checkouts/stats',
      );
    });

    test(
      '/triage still parses to the triage config, unaffected by the deep-link changes',
      () async {
        final config = await parser.parseRouteInformation(
          RouteInformation(uri: Uri.parse('/triage')),
        );
        expect(config.triage, isTrue);
        expect(config.deepLink, RequestBoardDeepLink.none);
      },
    );

    test('a bare "/" with filters still parses to the board config, unaffected '
        'by the deep-link changes', () async {
      final config = await parser.parseRouteInformation(
        RouteInformation(uri: Uri.parse('/?project=checkouts')),
      );
      expect(config.deepLink, RequestBoardDeepLink.none);
      expect(config.triage, isFalse);
      expect(
        config.filters,
        const RequestBoardFilters(projects: {'checkouts'}),
      );
    });
  });
}
