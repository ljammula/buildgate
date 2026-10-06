import 'package:console/text_escape.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('decodeUtf8Escaping', () {
    String show(List<int> bytes) => escapeInvisible(decodeUtf8Escaping(bytes));

    test('valid UTF-8 round-trips, including 2-, 3- and 4-byte sequences', () {
      expect(
        show([0x68, 0xC3, 0xA9, 0xE6, 0x97, 0xA5, 0xF0, 0x9F, 0x98, 0x80]),
        'hé日😀',
      );
    });

    test('each invalid byte becomes an explicit \\xNN, never U+FFFD', () {
      expect(show([0x80]), r'\x80');
      expect(show([0xFF, 0xFE]), r'\xFF\xFE');
      // Truncated 3-byte sequence, then valid text.
      expect(show([0xE2, 0x82, 0x41]), r'\xE2\x82A');
      // Overlong NUL, surrogate (0xED 0xA0 0x80) and > U+10FFFF
      // (0xF4 0x90 0x80 0x80).
      expect(show([0xC0, 0x80]), r'\xC0\x80');
      expect(show([0xED, 0xA0, 0x80]), r'\xED\xA0\x80');
      expect(show([0xF4, 0x90, 0x80, 0x80]), r'\xF4\x90\x80\x80');
      // Lead byte at the very end of input.
      expect(show([0x41, 0xE2]), r'A\xE2');
    });

    test('a real U+FFFD and an invalid byte render differently', () {
      expect(show([0xEF, 0xBF, 0xBD]), '�');
      expect(show([0xEF, 0xBF]), r'\xEF\xBF');
    });

    test('the escapes are flagged as synthetic segments', () {
      final segments = segmentEscapes(decodeUtf8Escaping([0x61, 0x80, 0x62]));
      expect(segments.map((s) => s.text), ['a', r'\x80', 'b']);
      expect(segments.map((s) => s.escaped), [false, true, false]);
    });
  });

  group('escapeInvisible', () {
    test('keeps ordinary text, newline and tab', () {
      expect(escapeInvisible('a\tb\nc é 日本'), 'a\tb\nc é 日本');
    });

    test('escapes every Bidi_Control character', () {
      final bidi = [
        0x061C,
        0x200E,
        0x200F,
        0x202A,
        0x202B,
        0x202C,
        0x202D,
        0x202E,
        0x2066,
        0x2067,
        0x2068,
        0x2069,
      ];
      for (final cp in bidi) {
        expect(
          escapeInvisible(String.fromCharCode(cp)),
          '\\u{${cp.toRadixString(16).toUpperCase()}}',
          reason: 'U+${cp.toRadixString(16)}',
        );
      }
    });

    test('escapes zero-width, format, separator and control characters', () {
      final hidden = [
        0x00,
        0x01,
        0x0D,
        0x1B,
        0x7F,
        0x85,
        0x9F,
        0x00AD,
        0x0600,
        0x180E,
        0x200B,
        0x200C,
        0x200D,
        0x2028,
        0x2029,
        0x2060,
        0x2064,
        0x206A,
        0x206F,
        0x3164,
        0xFEFF,
        0xFFA0,
        0xFFF9,
        0xE0001,
        0xE0041,
      ];
      for (final cp in hidden) {
        expect(
          segmentEscapes(String.fromCharCode(cp)).single.escaped,
          isTrue,
          reason: 'U+${cp.toRadixString(16)}',
        );
      }
    });

    test('a hidden character inside a word is visible', () {
      expect(escapeInvisible('go\u202Etest'), r'go\u{202E}test');
    });
  });
}
