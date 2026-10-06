import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

import type { RequestSummary } from "@/domain/request";

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
 * The hex SHA-256 of raw bytes -- Go's `sha256Hex` in internal/request. Used
 * for oracle files, which are hashed as received (never re-encoded from
 * decoded text) so the digest is the server's own.
 */
export function sha256HexBytes(bytes: Uint8Array): string {
  return bytesToHex(sha256(bytes));
}

/**
 * The `expected_sha256` map for an oracle_review approval: each file the
 * operator was shown (`shown`: file name -> hash of the bytes displayed),
 * keyed the way internal/request.requestOracleRelPaths keys them:
 * `oracle/NAME`.
 */
export function oracleExpectedSha256(shown: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [name, hash] of Object.entries(shown)) out[`oracle/${name}`] = hash;
  return out;
}

/**
 * The `expected_sha256` map to send with an approve call for `request`,
 * binding approval to the artifact shown -- keyed exactly the way
 * internal/request.Approve's own relPaths are: "spec.md" for a spec_review
 * approval, "tickets/NNN.spec.md" per ticket for a plan_review one. Empty
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
      return { "spec.md": sha256Hex(request.spec) };
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

function ticketRelPath(specPath: string): string | null {
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
