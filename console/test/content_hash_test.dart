import 'dart:convert';

import 'package:console/content_hash.dart';
import 'package:console/models.dart';
import 'package:crypto/crypto.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fixtures.dart';

void main() {
  test('sha256Hex matches a known SHA-256 digest', () {
    // echo -n "hello" | sha256sum
    expect(
      sha256Hex('hello'),
      '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824',
    );
  });

  test('sha256HexBytes hashes raw bytes, not decoded text (a non-UTF-8 '
      'oracle file must hash to the digest the server computed)', () {
    // python3 -c "import hashlib; print(hashlib.sha256(bytes([255,254,0])).hexdigest())"
    expect(
      sha256HexBytes([0xff, 0xfe, 0x00]),
      'ba778c0261008c8f71ae4061ad0162ffcbe63b52c91f89f236738131d1217ec7',
    );
    expect(sha256HexBytes(utf8.encode('hello')), sha256Hex('hello'));
  });

  test('oracleExpectedSha256 keys each shown file as oracle/NAME', () {
    expect(oracleExpectedSha256({'RUN_COMMAND.txt': 'aa', 'a_test.go': 'bb'}), {
      'oracle/RUN_COMMAND.txt': 'aa',
      'oracle/a_test.go': 'bb',
    });
    expect(oracleExpectedSha256({}), isEmpty);
  });

  group('expectedSha256For', () {
    test('spec_review hashes spec.md under that literal key', () {
      final request = RequestSummary.fromJson(
        jsonDecode(
              requestJson(id: 'req-1', state: 'spec_review', spec: '# Spec\n'),
            )
            as Map<String, dynamic>,
      );

      expect(expectedSha256For(request), {
        'spec.md': sha256.convert(utf8.encode('# Spec\n')).toString(),
      });
    });

    test('plan_review hashes each ticket under its own tickets/NNN.spec.md '
        'key, extracted from the (absolute, in production) specPath', () {
      final request = RequestSummary.fromJson(
        jsonDecode(
              requestJson(
                id: 'req-1',
                state: 'plan_review',
                tickets: [
                  requestTicketJson(
                    index: 1,
                    specPath: '/data/requests/req-1/tickets/001.spec.md',
                    content: 'Ticket 1',
                  ),
                  requestTicketJson(
                    index: 2,
                    specPath: '/data/requests/req-1/tickets/002.spec.md',
                    content: 'Ticket 2',
                  ),
                ],
              ),
            )
            as Map<String, dynamic>,
      );

      expect(expectedSha256For(request), {
        'tickets/001.spec.md': sha256
            .convert(utf8.encode('Ticket 1'))
            .toString(),
        'tickets/002.spec.md': sha256
            .convert(utf8.encode('Ticket 2'))
            .toString(),
      });
    });

    test('uses the final /tickets/ segment, not the first (regression: an '
        'absolute -data-dir that itself contains a directory literally '
        'named "tickets" -- a plausible real deployment path -- matched '
        'the outer occurrence first, producing a key the server never '
        'recognizes and silently dropping the ticket from the map)', () {
      final request = RequestSummary.fromJson(
        jsonDecode(
              requestJson(
                id: 'req-1',
                state: 'plan_review',
                tickets: [
                  requestTicketJson(
                    index: 1,
                    specPath:
                        '/srv/tickets/factory-data/requests/req-1/tickets/001.spec.md',
                    content: 'Ticket 1',
                  ),
                ],
              ),
            )
            as Map<String, dynamic>,
      );

      expect(expectedSha256For(request), {
        'tickets/001.spec.md': sha256
            .convert(utf8.encode('Ticket 1'))
            .toString(),
      });
    });

    test('any other state has nothing to bind an approval to', () {
      final request = RequestSummary.fromJson(
        jsonDecode(requestJson(id: 'req-1', state: 'building'))
            as Map<String, dynamic>,
      );

      expect(expectedSha256For(request), isEmpty);
    });
  });
}
