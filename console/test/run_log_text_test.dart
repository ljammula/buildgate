import 'package:console/models.dart';
import 'package:console/run_detail_screen.dart';
import 'package:flutter_test/flutter_test.dart';

Attempt _attempt(String kind, int exitCode) => Attempt(
  command: const [],
  startedAt: '2026-09-29T14:00:00Z',
  finishedAt: '2026-09-29T14:01:00Z',
  exitCode: exitCode,
  logPath: '',
  kind: kind,
);

void main() {
  group('readableBuildLog', () {
    test('renders FACTORY_PROGRESS lines as steps, like factoryd watch', () {
      const raw =
          'plain output\n'
          'FACTORY_PROGRESS {"stage": "round", "event": "start", "round": 1, "max_rounds": 3}\n'
          'FACTORY_PROGRESS {"stage": "agent", "event": "note", "round": 1, "detail": "bash: go test\\n ./..."}\n'
          'FACTORY_PROGRESS {"stage": "round", "event": "end", "round": 1, "outcome": "fail", "detail": "verify failed: go test ./..."}';
      expect(readableBuildLog(raw).split('\n'), [
        'plain output',
        '▸ round 1/3    started',
        '▸ agent        bash: go test ./...',
        '▸ round 1      verify failed: go test ./...',
      ]);
    });

    test(
      'keeps a line cut mid-stream, or one that is not a step, verbatim',
      () {
        const cut = 'FACTORY_PROGRESS {"stage": "agent", "ev';
        expect(readableBuildLog(cut), cut);
        const notAnObject = 'FACTORY_PROGRESS [1, 2]';
        expect(readableBuildLog(notAnObject), notAnObject);
      },
    );
  });

  group('attemptExitText', () {
    test('decodes a combined review launch exit code', () {
      expect(
        attemptExitText(_attempt('review', 40)),
        'Exit code: 40 (spec conformity passed, code review passed)',
      );
      expect(
        attemptExitText(_attempt('review', 41)),
        'Exit code: 41 (spec conformity failed, code review passed)',
      );
      expect(
        attemptExitText(_attempt('review', 43)),
        'Exit code: 43 (spec conformity failed, code review failed)',
      );
    });

    test('leaves other exit codes alone', () {
      expect(attemptExitText(_attempt('review', 1)), 'Exit code: 1');
      expect(attemptExitText(_attempt('build', 40)), 'Exit code: 40');
    });
  });
}
