import 'package:console/tab_title.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('needsHumanTabTitle', () {
    test('a failed poll always shows (?), never a stale count or zero', () {
      expect(
        needsHumanTabTitle(needsHumanCount: 5, pollFailed: true),
        '(?) Factory Console',
      );
      expect(
        needsHumanTabTitle(needsHumanCount: null, pollFailed: true),
        '(?) Factory Console',
      );
      expect(
        needsHumanTabTitle(needsHumanCount: 0, pollFailed: true),
        '(?) Factory Console',
      );
    });

    test('a positive count shows the count', () {
      expect(
        needsHumanTabTitle(needsHumanCount: 3, pollFailed: false),
        '(3) Factory Console',
      );
    });

    test(
      'a zero or unknown count with a successful poll shows the plain title',
      () {
        expect(
          needsHumanTabTitle(needsHumanCount: 0, pollFailed: false),
          'Factory Console',
        );
        expect(
          needsHumanTabTitle(needsHumanCount: null, pollFailed: false),
          'Factory Console',
        );
      },
    );
  });
}
