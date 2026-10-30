import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

import type { ReadBack, RequestSummary } from "@/domain/request";

// SHA-256 comes from a pure-JS implementation, not Web Crypto: crypto.subtle
// exists only in secure contexts, and a console served over plain HTTP on a
// non-loopback address would otherwise lose approvals.

/**
 * The hex SHA-256 of content's UTF-8 bytes -- matches
 * internal/request.HashFile's own `sha256.Sum256` over a file's raw bytes
 * exactly, as long as content is what that file's bytes decode to (true for
 * spec.md/ticket spec files, always plain UTF-8 markdown in practice). Used
 * to bind an approval to the exact artifact the console last fetched.
 */
export function sha256Hex(content: string): string {
  return sha256HexBytes(new TextEncoder().encode(content));
}

/**
 * The hex SHA-256 of raw bytes -- Go's `sha256Hex` in internal/request. The digest of bytes
 * hashed as received, never re-encoded from decoded text.
 */
export function sha256HexBytes(bytes: Uint8Array): string {
  return bytesToHex(sha256(bytes));
}

/**
 * The pseudo-path a spec approval sends the read-back answers' digest under,
 * beside the file hashes (internal/request.ReadBackAnswersKey).
 */
export const readBackAnswersKey = "read-back/answers";

/**
 * The digest of the read-back answers the console shows
 * (internal/request.ReadBackShownSHA256): per answered question, in order,
 * its id, the UTF-8 byte length of its text, the text, the byte length of
 * its answer and the answer, each followed by a newline. No read-back, or no
 * answer, is the digest of no bytes.
 */
export function readBackAnswersSha256(readBack: ReadBack | null): string {
  const encoder = new TextEncoder();
  let text = "";
  for (const question of readBack?.questions ?? []) {
    if (question.answer === "") continue;
    const textBytes = encoder.encode(question.text).length;
    const answerBytes = encoder.encode(question.answer).length;
    text += `${question.id}\n${textBytes}\n${question.text}\n${answerBytes}\n${question.answer}\n`;
  }
  return sha256Hex(text);
}

/**
 * The `expected_sha256` map to send with an approve call for `request`,
 * binding approval to the artifact shown -- keyed exactly the way
 * internal/request.Approve's own relPaths are: "spec.md" for a spec_review
 * approval (with the read-back answers shown, under readBackAnswersKey),
 * "tickets/NNN.spec.md" per ticket for a plan_review one. Empty
 * for any other state (nothing to bind -- Approve itself refuses those
 * regardless).
 *
 * A ticket's own `specPath` is an absolute filesystem path in production,
 * not the bare relative key the server hashes by -- extracted here by
 * finding the "tickets/" segment, the same suffix-matching approach the
 * request detail's revision-compare view uses for the identical mismatch.
 */
export function expectedSha256For(request: RequestSummary): Record<string, string> {
  switch (request.state) {
    case "spec_review":
      return {
        "spec.md": sha256Hex(request.spec),
        [readBackAnswersKey]: readBackAnswersSha256(request.readBack),
      };
    case "plan_review": {
      const hashes: Record<string, string> = {};
      for (const ticket of request.tickets) {
        if (ticket.content === "") continue;
        const relPath = ticketRelPath(ticket.specPath);
        if (relPath === null) continue;
        hashes[relPath] = sha256Hex(ticket.content);
      }
      return hashes;
    }
    default:
      return {};
  }
}

/** A ticket file's request-relative path ("tickets/001.spec.md") from its `spec_path`; null when it has none. */
export function ticketRelPath(specPath: string): string | null {
  const normalized = specPath.replaceAll("\\", "/");
  // The *last* "/tickets/" segment, not the first -- found in review: an
  // absolute -data-dir that itself contains a directory literally named
  // "tickets" (e.g. /srv/tickets/factory-data/.../tickets/001.spec.md, an
  // entirely plausible deployment path) matched the outer occurrence first,
  // producing a key the server never recognizes. That silently dropped this
  // ticket from expected_sha256 -- the exact staleness gap this map exists
  // to close.
  const index = normalized.lastIndexOf("/tickets/");
  if (index === -1) {
    // No leading '/' at all -- e.g. the whole path is just
    // "tickets/001.spec.md" already. Only valid at the very start.
    return normalized.startsWith("tickets/") ? normalized : null;
  }
  return normalized.substring(index + 1);
}
