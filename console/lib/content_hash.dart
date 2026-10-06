import 'dart:convert';

import 'package:crypto/crypto.dart';

import 'models.dart';

/// The hex SHA-256 of content's UTF-8 bytes -- matches
/// internal/request.HashFile's own `sha256.Sum256` over a file's raw
/// bytes exactly, as long as content is what that file's bytes decode to
/// (true for spec.md/ticket spec files, always plain UTF-8 markdown in
/// practice). Used to bind an approval to the exact artifact the console
/// last fetched -- see approve_reject.dart's own use of this.
String sha256Hex(String content) => sha256HexBytes(utf8.encode(content));

/// The hex SHA-256 of raw [bytes] -- Go's `sha256Hex` in
/// internal/request. Used for oracle files, which are hashed as received
/// (never re-encoded from decoded text) so the digest is the server's own.
String sha256HexBytes(List<int> bytes) => sha256.convert(bytes).toString();

/// The `expected_sha256` map for an oracle_review approval: each file the
/// operator was shown ([shown]: file name -> hash of the bytes displayed),
/// keyed the way internal/request.requestOracleRelPaths keys them:
/// `oracle/NAME`.
Map<String, String> oracleExpectedSha256(Map<String, String> shown) => {
  for (final entry in shown.entries) 'oracle/${entry.key}': entry.value,
};

/// The `expected_sha256` map to send with an approve call for `request`,
/// binding approval to the artifact shown -- keyed exactly the way
/// internal/request.Approve's own relPaths are:
/// "spec.md" for a spec_review approval, "tickets/NNN.spec.md" per
/// ticket for a plan_review one. Empty for any other state (nothing to
/// bind -- Approve itself refuses those regardless).
///
/// A ticket's own [RequestTicket.specPath] is an absolute filesystem path
/// in production (see request_detail_screen.dart's own note on this),
/// not the bare relative key the server hashes by -- extracted here by
/// finding the "tickets/" segment, the same suffix-matching approach
/// request_detail_screen.dart's revision-compare view already uses for
/// the identical mismatch.
Map<String, String> expectedSha256For(RequestSummary request) {
  switch (request.state) {
    case 'spec_review':
      return {'spec.md': sha256Hex(request.spec)};
    case 'plan_review':
      final hashes = <String, String>{};
      for (final ticket in request.tickets) {
        if (ticket.content.isEmpty) continue;
        final relPath = _ticketRelPath(ticket.specPath);
        if (relPath == null) continue;
        hashes[relPath] = sha256Hex(ticket.content);
      }
      return hashes;
    default:
      return const {};
  }
}

String? _ticketRelPath(String specPath) {
  final normalized = specPath.replaceAll('\\', '/');
  // The *last* "/tickets/" segment, not the first -- found in review: an
  // absolute -data-dir that itself contains a directory literally named
  // "tickets" (e.g. /srv/tickets/factory-data/.../tickets/001.spec.md,
  // an entirely plausible deployment path) matched the outer occurrence
  // first, producing a key the server never recognizes. That silently
  // dropped this ticket from expected_sha256 -- the exact staleness gap
  // this map exists to close.
  final index = normalized.lastIndexOf('/tickets/');
  if (index == -1) {
    // No leading '/' at all -- e.g. the whole path is just
    // "tickets/001.spec.md" already. Only valid at the very start.
    return normalized.startsWith('tickets/') ? normalized : null;
  }
  return normalized.substring(index + 1);
}
