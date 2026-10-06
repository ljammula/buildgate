import 'package:console/start_token.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('parseStartTokenFromHash', () {
    test('parses a bare #t=<token> fragment', () {
      expect(parseStartTokenFromHash('#t=abc123'), 'abc123');
    });

    test('accepts the fragment with or without a leading #', () {
      expect(parseStartTokenFromHash('t=abc123'), 'abc123');
    });

    test('decodes a percent-encoded token', () {
      expect(parseStartTokenFromHash('#t=a%2Fb%3D'), 'a/b=');
    });

    test('finds t= among other &-joined components', () {
      expect(parseStartTokenFromHash('#other=1&t=abc123'), 'abc123');
      expect(parseStartTokenFromHash('#t=abc123&other=1'), 'abc123');
    });

    test('returns null for an empty hash', () {
      expect(parseStartTokenFromHash(''), isNull);
      expect(parseStartTokenFromHash('#'), isNull);
    });

    test('returns null when there is no t= component', () {
      expect(parseStartTokenFromHash('#/requests/req-1'), isNull);
      expect(parseStartTokenFromHash('#other=1'), isNull);
    });

    test('returns null for an empty token value', () {
      expect(parseStartTokenFromHash('#t='), isNull);
    });

    test('does not match a key merely ending in t, e.g. "at="', () {
      expect(parseStartTokenFromHash('#at=abc123'), isNull);
    });
  });

  group('stored start token (stub-backed)', () {
    tearDown(clearStoredStartTokenForTest);

    test('starts out unset', () {
      expect(getStoredStartToken(), isNull);
    });

    test('set/get round-trips', () {
      setStoredStartToken('tok-1');
      expect(getStoredStartToken(), 'tok-1');
    });

    test('captureStartTokenFromLocation is a no-op off the web', () {
      // The stub (selected under `flutter test`'s VM target) never has a
      // window.location to read from -- this just documents that calling
      // it does not throw and leaves any already-stored token alone.
      setStoredStartToken('tok-1');
      captureStartTokenFromLocation();
      expect(getStoredStartToken(), 'tok-1');
    });
  });
}
