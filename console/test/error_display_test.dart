import 'package:console/api_client.dart';
import 'package:console/error_display.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('describeError classifies known failure modes', () {
    test('a 401 RunApiException is a generic authorization failure', () {
      // Not "read token mismatch": a 401/403 can come from the read,
      // start, or override token depending on which action failed (found
      // via Codex review of PR #172, P2) -- see error_display.dart's own
      // comment on this branch.
      final summary = describeError(
        const RunApiException(401, '{"error":"unauthorized"}'),
      );
      expect(summary.headline, 'Not authorized');
      expect(summary.raw, 'unauthorized');
    });

    test('a 403 RunApiException is also a generic authorization failure', () {
      final summary = describeError(
        const RunApiException(403, '{"error":"forbidden"}'),
      );
      expect(summary.headline, 'Not authorized');
    });

    test('a 404 RunApiException is reported as not found', () {
      final summary = describeError(
        const RunApiException(404, '{"error":"no such run"}'),
      );
      expect(summary.headline, 'Not found');
      expect(summary.nextStep, contains('pruned'));
    });

    test('another RunApiException status falls back to a generic message', () {
      final summary = describeError(
        const RunApiException(500, '{"error":"kill switch is unreadable"}'),
      );
      expect(summary.headline, 'Request failed (500)');
      expect(summary.raw, 'kill switch is unreadable');
    });

    test('a connection-refused failure names factoryd unreachable', () {
      final summary = describeError(
        Exception('SocketException: Connection refused'),
      );
      expect(summary.headline, "Can't reach factoryd");
      expect(summary.nextStep, contains('factoryd serve'));
    });

    test('a DNS lookup failure is also reported as unreachable', () {
      final summary = describeError(
        Exception('Failed host lookup: model-host'),
      );
      expect(summary.headline, "Can't reach factoryd");
    });

    test('a web fetch failure is also reported as unreachable', () {
      final summary = describeError(
        Exception('ClientException: Failed to fetch'),
      );
      expect(summary.headline, "Can't reach factoryd");
    });

    test('an unrecognized error falls back to a generic headline', () {
      final summary = describeError('Workspace path is required');
      expect(summary.headline, 'Something went wrong');
      expect(summary.raw, 'Workspace path is required');
    });
  });

  group('describeError(startClass: true) (F: serve-start-token)', () {
    test('a 401/403 points at the console link factoryd serve prints', () {
      for (final status in [401, 403]) {
        final summary = describeError(
          RunApiException(status, '{"error":"forbidden"}'),
          startClass: true,
        );
        expect(summary.headline, 'Not authorized');
        expect(summary.nextStep, contains('factoryd serve'));
        expect(summary.nextStep, contains('#t=...'));
        // Not the generic three-token guidance -- that would point an
        // operator at manually configuring an env var the console has no
        // way to use anyway.
        expect(summary.nextStep, isNot(contains('FACTORYD_API_READ_TOKEN')));
      }
    });

    test('startClass leaves a non-auth status code unaffected', () {
      final summary = describeError(
        const RunApiException(500, '{"error":"boom"}'),
        startClass: true,
      );
      expect(summary.headline, 'Request failed (500)');
    });

    test('startClass leaves a non-RunApiException error unaffected', () {
      final summary = describeError(
        Exception('SocketException: Connection refused'),
        startClass: true,
      );
      expect(summary.headline, "Can't reach factoryd");
    });
  });
}
