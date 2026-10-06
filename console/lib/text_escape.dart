// Making hidden characters visible. An oracle file is code the operator is
// about to approve, so nothing in it (or in a name or message derived from
// it) may hide behind an invisible or direction-changing character, and two
// different byte sequences must never render identically. Display only: the
// approval hash is always over the raw bytes.

/// One run of display text; [escaped] runs are synthetic `\u{XXXX}` /
/// `\xNN` escapes, not characters of the source.
class EscapeSegment {
  const EscapeSegment(this.text, {required this.escaped});

  final String text;
  final bool escaped;
}

// U+DC80..U+DCFF stand for one invalid UTF-8 byte 0x80..0xFF each (the
// "surrogateescape" convention): decodeUtf8Escaping emits them and
// segmentEscapes turns them back into `\xNN`.
const _byteEscapeBase = 0xDC00;

bool _isHidden(int r) =>
    (r < 0x20 && r != 0x0A && r != 0x09) ||
    (r >= 0x7F && r <= 0x9F) ||
    r == 0x00AD ||
    (r >= 0x0600 && r <= 0x0605) ||
    r == 0x061C ||
    r == 0x06DD ||
    r == 0x070F ||
    r == 0x08E2 ||
    r == 0x115F ||
    r == 0x1160 ||
    r == 0x180E ||
    (r >= 0x200B && r <= 0x200F) ||
    (r >= 0x2028 && r <= 0x202E) ||
    (r >= 0x2060 && r <= 0x2064) ||
    (r >= 0x2066 && r <= 0x206F) ||
    r == 0x3164 ||
    r == 0xFEFF ||
    r == 0xFFA0 ||
    (r >= 0xFFF9 && r <= 0xFFFB) ||
    r == 0x110BD ||
    (r >= 0x1BCA0 && r <= 0x1BCA3) ||
    (r >= 0x1D173 && r <= 0x1D17A) ||
    r == 0xE0001 ||
    (r >= 0xE0020 && r <= 0xE007F);

String _hex(int v, [int pad = 0]) =>
    v.toRadixString(16).toUpperCase().padLeft(pad, '0');

/// Splits [text] into plain and escaped runs: C0/C1 controls (except
/// newline and tab), every Bidi_Control character, zero-width and other
/// layout-affecting format characters, line/paragraph separators, and the
/// surrogate-escaped invalid bytes from [decodeUtf8Escaping].
List<EscapeSegment> segmentEscapes(String text) {
  final out = <EscapeSegment>[];
  final plain = StringBuffer();
  void flush() {
    if (plain.isEmpty) return;
    out.add(EscapeSegment(plain.toString(), escaped: false));
    plain.clear();
  }

  for (final rune in text.runes) {
    if (rune >= _byteEscapeBase + 0x80 && rune <= _byteEscapeBase + 0xFF) {
      flush();
      out.add(
        EscapeSegment('\\x${_hex(rune - _byteEscapeBase, 2)}', escaped: true),
      );
    } else if (_isHidden(rune) || (rune >= 0xD800 && rune <= 0xDFFF)) {
      flush();
      out.add(EscapeSegment('\\u{${_hex(rune)}}', escaped: true));
    } else {
      plain.writeCharCode(rune);
    }
  }
  flush();
  return out;
}

/// [text] with every escape from [segmentEscapes] written inline.
String escapeInvisible(String text) =>
    segmentEscapes(text).map((s) => s.text).join();

/// Strict UTF-8 decode that never substitutes U+FFFD: each byte that is not
/// part of a well-formed sequence (bad lead, truncated or overlong sequence,
/// surrogate, beyond U+10FFFF) becomes a surrogate-escape code unit that
/// [segmentEscapes] renders as `\xNN`, so distinct bytes never look alike.
String decodeUtf8Escaping(List<int> bytes) {
  final out = StringBuffer();
  var i = 0;
  while (i < bytes.length) {
    final b = bytes[i];
    var need = 0;
    var min = 0;
    var cp = b;
    if (b < 0x80) {
      out.writeCharCode(b);
      i++;
      continue;
    } else if (b >= 0xC2 && b <= 0xDF) {
      need = 1;
      min = 0x80;
      cp = b & 0x1F;
    } else if (b >= 0xE0 && b <= 0xEF) {
      need = 2;
      min = 0x800;
      cp = b & 0x0F;
    } else if (b >= 0xF0 && b <= 0xF4) {
      need = 3;
      min = 0x10000;
      cp = b & 0x07;
    }
    var ok = need > 0 && i + need < bytes.length;
    if (ok) {
      for (var k = 1; k <= need; k++) {
        final c = bytes[i + k];
        if (c & 0xC0 != 0x80) {
          ok = false;
          break;
        }
        cp = (cp << 6) | (c & 0x3F);
      }
    }
    if (ok && (cp < min || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF))) {
      ok = false;
    }
    if (ok) {
      out.writeCharCode(cp);
      i += need + 1;
    } else {
      out.writeCharCode(_byteEscapeBase + b);
      i++;
    }
  }
  return out.toString();
}
