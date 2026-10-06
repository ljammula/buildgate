import 'package:console/text_diff.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test('marks unchanged, removed, and added lines', () {
    final diff = unifiedLineDiff('a\nb\nc', 'a\nx\nc');
    expect(diff, '  a\n- b\n+ x\n  c\n');
  });

  test('an oversized comparison is refused, not computed', () {
    // The (m+1) x (n+1) LCS matrix this function
    // builds is one int per cell, allocated synchronously on the UI
    // thread. Without a bound, a large enough pair of texts could
    // allocate hundreds of megabytes and freeze the tab merely from
    // being selected for comparison -- this asserts the refusal message
    // appears instead of the function attempting the full computation.
    final big = List.generate(3000, (i) => 'line $i').join('\n');
    final diff = unifiedLineDiff(big, big);
    expect(diff, contains('diff not shown'));
    expect(diff, isNot(contains('  line 0')));
  });

  test('a same-sized-but-under-the-cap comparison still computes normally', () {
    final diff = unifiedLineDiff('a\nb', 'a\nc');
    expect(diff, '  a\n- b\n+ c\n');
  });
}
