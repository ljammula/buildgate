import 'package:console/models.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('ProgressEvent.fromJson', () {
    test('parses a full factory-stage line (progress-contract.md)', () {
      final event = ProgressEvent.fromJson({
        'ts': '2026-09-17T10:00:00.123Z',
        'source': 'factory',
        'stage': 'build',
        'event': 'start',
        'round': 0,
        'max_rounds': 0,
        'outcome': '',
        'detail': '',
      });

      expect(event.ts, DateTime.parse('2026-09-17T10:00:00.123Z'));
      expect(event.source, 'factory');
      expect(event.stage, 'build');
      expect(event.event, 'start');
      expect(event.round, 0);
      expect(event.maxRounds, 0);
      expect(event.outcome, '');
      expect(event.detail, '');
    });

    test('parses a worker round line', () {
      final event = ProgressEvent.fromJson({
        'ts': '2026-09-17T10:01:00.000Z',
        'source': 'worker',
        'stage': 'round',
        'event': 'end',
        'round': 2,
        'max_rounds': 6,
        'outcome': 'fail',
        'detail': 'verify failed: go test ./...',
      });

      expect(event.source, 'worker');
      expect(event.round, 2);
      expect(event.maxRounds, 6);
      expect(event.outcome, 'fail');
      expect(event.detail, 'verify failed: go test ./...');
    });

    // Every int/string field must tolerate being missing entirely --
    // progress-contract.md's own "omitempty is fine in Go, readers must
    // treat missing as 0/''" -- rather than throwing a type-cast error on
    // a genuinely minimal line.
    test('tolerates missing round/max_rounds/outcome/detail', () {
      final event = ProgressEvent.fromJson({
        'ts': '2026-09-17T10:00:00.000Z',
        'source': 'factory',
        'stage': 'preflight',
        'event': 'start',
      });

      expect(event.round, 0);
      expect(event.maxRounds, 0);
      expect(event.outcome, '');
      expect(event.detail, '');
    });

    // An unparseable/missing ts must not throw -- a malformed line from a
    // future server build must never crash this screen.
    test('tolerates a missing or malformed ts', () {
      final event = ProgressEvent.fromJson({
        'source': 'factory',
        'stage': 'preflight',
        'event': 'start',
      });

      expect(event.ts, DateTime.fromMillisecondsSinceEpoch(0));
    });

    test('tolerates missing source/stage/event', () {
      final event = ProgressEvent.fromJson({'ts': '2026-09-17T10:00:00.000Z'});

      expect(event.source, '');
      expect(event.stage, '');
      expect(event.event, '');
    });
  });
}
