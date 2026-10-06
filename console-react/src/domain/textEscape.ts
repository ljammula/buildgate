// Making hidden characters visible. An oracle file is code the operator is
// about to approve, so nothing in it (or in a name or message derived from
// it) may hide behind an invisible or direction-changing character, and two
// different byte sequences must never render identically. Display only: the
// approval hash is always over the raw bytes.

/**
 * One run of display text; `escaped` runs are synthetic `\u{XXXX}` / `\xNN`
 * escapes, not characters of the source.
 */
export interface EscapeSegment {
  readonly text: string;
  readonly escaped: boolean;
}

// U+DC80..U+DCFF stand for one invalid UTF-8 byte 0x80..0xFF each (the
// "surrogateescape" convention): decodeUtf8Escaping emits them and
// segmentEscapes turns them back into `\xNN`.
const BYTE_ESCAPE_BASE = 0xdc00;

function isHidden(r: number): boolean {
  return (
    (r < 0x20 && r !== 0x0a && r !== 0x09) ||
    (r >= 0x7f && r <= 0x9f) ||
    r === 0x00ad ||
    (r >= 0x0600 && r <= 0x0605) ||
    r === 0x061c ||
    r === 0x06dd ||
    r === 0x070f ||
    r === 0x08e2 ||
    r === 0x115f ||
    r === 0x1160 ||
    r === 0x180e ||
    (r >= 0x200b && r <= 0x200f) ||
    (r >= 0x2028 && r <= 0x202e) ||
    (r >= 0x2060 && r <= 0x2064) ||
    (r >= 0x2066 && r <= 0x206f) ||
    r === 0x3164 ||
    r === 0xfeff ||
    r === 0xffa0 ||
    (r >= 0xfff9 && r <= 0xfffb) ||
    r === 0x110bd ||
    (r >= 0x1bca0 && r <= 0x1bca3) ||
    (r >= 0x1d173 && r <= 0x1d17a) ||
    r === 0xe0001 ||
    (r >= 0xe0020 && r <= 0xe007f)
  );
}

function hex(v: number, pad = 0): string {
  return v.toString(16).toUpperCase().padStart(pad, "0");
}

/**
 * Splits `text` into plain and escaped runs: C0/C1 controls (except newline
 * and tab), every Bidi_Control character, zero-width and other
 * layout-affecting format characters, line/paragraph separators, and the
 * surrogate-escaped invalid bytes from decodeUtf8Escaping.
 */
export function segmentEscapes(text: string): readonly EscapeSegment[] {
  const out: EscapeSegment[] = [];
  let plain = "";
  const flush = (): void => {
    if (plain === "") return;
    out.push({ text: plain, escaped: false });
    plain = "";
  };

  // Iterating a string yields code points; a lone surrogate comes through as
  // itself, like Dart's `runes`.
  for (const ch of text) {
    const rune = ch.codePointAt(0) ?? 0;
    if (rune >= BYTE_ESCAPE_BASE + 0x80 && rune <= BYTE_ESCAPE_BASE + 0xff) {
      flush();
      out.push({ text: `\\x${hex(rune - BYTE_ESCAPE_BASE, 2)}`, escaped: true });
    } else if (isHidden(rune) || (rune >= 0xd800 && rune <= 0xdfff)) {
      flush();
      out.push({ text: `\\u{${hex(rune)}}`, escaped: true });
    } else {
      plain += ch;
    }
  }
  flush();
  return out;
}

/** `text` with every escape from segmentEscapes written inline. */
export function escapeInvisible(text: string): string {
  return segmentEscapes(text)
    .map((s) => s.text)
    .join("");
}

/**
 * Strict UTF-8 decode that never substitutes U+FFFD: each byte that is not
 * part of a well-formed sequence (bad lead, truncated or overlong sequence,
 * surrogate, beyond U+10FFFF) becomes a surrogate-escape code unit that
 * segmentEscapes renders as `\xNN`, so distinct bytes never look alike.
 */
export function decodeUtf8Escaping(bytes: ArrayLike<number>): string {
  let out = "";
  let i = 0;
  while (i < bytes.length) {
    const b = bytes[i] ?? 0;
    let need = 0;
    let min = 0;
    let cp = b;
    if (b < 0x80) {
      out += String.fromCharCode(b);
      i++;
      continue;
    } else if (b >= 0xc2 && b <= 0xdf) {
      need = 1;
      min = 0x80;
      cp = b & 0x1f;
    } else if (b >= 0xe0 && b <= 0xef) {
      need = 2;
      min = 0x800;
      cp = b & 0x0f;
    } else if (b >= 0xf0 && b <= 0xf4) {
      need = 3;
      min = 0x10000;
      cp = b & 0x07;
    }
    let ok = need > 0 && i + need < bytes.length;
    if (ok) {
      for (let k = 1; k <= need; k++) {
        const c = bytes[i + k] ?? 0;
        if ((c & 0xc0) !== 0x80) {
          ok = false;
          break;
        }
        cp = (cp << 6) | (c & 0x3f);
      }
    }
    if (ok && (cp < min || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff))) {
      ok = false;
    }
    if (ok) {
      out += String.fromCodePoint(cp);
      i += need + 1;
    } else {
      out += String.fromCharCode(BYTE_ESCAPE_BASE + b);
      i++;
    }
  }
  return out;
}
