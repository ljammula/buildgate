import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

void main() {
  // Regression: these 8
  // screens used to render a caught exception directly as the *only* text
  // (e.g. `Text('Could not load run: $error')`), with no headline, no next
  // step, and no way to see the raw error separately from the summary.
  // Every such site now goes through describeError/ErrorCallout
  // (error_display.dart) instead. This scans each screen's source for the
  // literal pattern -- a string interpolating a variable named exactly
  // `error` or `_error` -- so a future edit can't reintroduce it here
  // without failing this test.
  test('none of the 8 screens interpolate a bare error/_error into text', () {
    final screens = [
      'run_detail_screen.dart',
      'run_list_screen.dart',
      'request_detail_screen.dart',
      'request_list_screen.dart',
      'new_run_screen.dart',
      'project_release_screen.dart',
      'project_stats_screen.dart',
      'triage_screen.dart',
    ];
    final barePattern = RegExp(r'\$_?error\b');

    for (final screen in screens) {
      final file = File('lib/$screen');
      expect(file.existsSync(), isTrue, reason: '$screen should exist');
      final matches = barePattern
          .allMatches(file.readAsStringSync())
          .map((m) => m.group(0))
          .toList();
      expect(
        matches,
        isEmpty,
        reason:
            '$screen still interpolates a bare error value directly into '
            'text: $matches -- route it through describeError/ErrorCallout '
            '(error_display.dart) instead',
      );
    }
  });
}
