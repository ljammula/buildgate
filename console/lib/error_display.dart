import 'package:flutter/material.dart';

import 'api_client.dart';

/// An operator-facing classification of a caught error: screens used to
/// render the raw exception as the *only*
/// text (`Text('Could not load run: $error')`), which forces every
/// operator to read and interpret Dart/HTTP exception internals with no
/// guidance on what to actually do about it. [describeError] turns that
/// into a short [headline] plus a concrete [nextStep], while keeping the
/// original text in [raw] for an expandable "details" section -- nothing
/// is hidden, just demoted below the actionable summary.
class ErrorSummary {
  const ErrorSummary({
    required this.headline,
    required this.nextStep,
    required this.raw,
  });

  final String headline;
  final String nextStep;
  final String raw;
}

/// Classifies [error] into an [ErrorSummary]. Order matters: a
/// [RunApiException] carries a real HTTP status code, so it's checked
/// first and takes priority over the connection-level text match below.
///
/// [startClass] marks a call to one of the start-token-gated routes (POST
/// /runs, the daemon lifecycle routes, GET /projects/{project}/release and
/// /stats -- see internal/api's authorizeStart) -- pass true from the
/// screens that make one (new_run_screen, project_release_screen,
/// project_stats_screen, release_screen, ops_screen, and
/// request_list_screen's own GET /daemons poll) so a 401/403 there can
/// name the one concrete fix (open serve's own printed console link)
/// instead of the generic three-token guidance below, which is still used
/// for a plain read/override 401/403 since the server's own error body
/// can't disambiguate those two either (see this function's older comment
/// on that, kept below).
ErrorSummary describeError(Object error, {bool startClass = false}) {
  if (error is RunApiException) {
    final raw = error.messageParts.join('\n');
    if (error.statusCode == 401 || error.statusCode == 403) {
      if (startClass) {
        // F: serve-start-token. `factoryd serve` now generates a start
        // token itself and embeds it in the console link it prints
        // (`http://<addr>/#t=<token>`); this console's own main.dart
        // captures that fragment into localStorage on first load, so the
        // token an operator needs is not a config value to go find -- it's
        // whichever link the currently-running `serve` process printed
        // most recently, since the token is per-process and changes every
        // restart. A stale stored token (from a previous `serve` run) is
        // the expected cause here, not a typo to go hunting for.
        return ErrorSummary(
          headline: 'Not authorized',
          nextStep:
              'This browser\'s start token is missing or is from a '
              'previous `factoryd serve` run -- the token changes every '
              'restart. Open the console link `factoryd serve` printed in '
              'its own log/terminal output just now (ends in `#t=...`), '
              'which reloads this page with the current token.',
          raw: raw,
        );
      }
      // Found via Codex review of PR #172 (P2): a 401/403 here isn't
      // always a read-token mismatch -- approve/reject use the override
      // token, starting a run uses the start token, and only plain
      // GET/watch requests use the read token (see api_client.dart's
      // _startToken/_overrideToken/read-token split). Naming a specific
      // env var here would point an operator at the wrong fix for a
      // write-action failure, so this stays generic across all three.
      // The server's own body can't disambiguate either: internal/api
      // reports "requests endpoint is not authorized" for both the
      // read-gated request list and the override-gated approve/reject.
      return ErrorSummary(
        headline: 'Not authorized',
        nextStep:
            "This request's token doesn't match what factoryd expects "
            '(FACTORYD_API_READ_TOKEN for run/request reads, '
            'FACTORYD_API_START_TOKEN for starting runs and the '
            'release/stats/daemon views, or FACTORYD_API_OVERRIDE_TOKEN '
            'for approve/reject/override) -- or none is configured on one '
            'side. Check the token for this action and reload.',
        raw: raw,
      );
    }
    if (error.statusCode == 404) {
      return ErrorSummary(
        headline: 'Not found',
        nextStep: 'It may have been pruned, or the id in the URL is wrong.',
        raw: raw,
      );
    }
    return ErrorSummary(
      headline: 'Request failed (${error.statusCode})',
      nextStep: 'See details below.',
      raw: raw,
    );
  }

  final raw = error.toString();
  final lower = raw.toLowerCase();
  // Covers both io (SocketException's "Connection refused"/"Failed host
  // lookup") and web (fetch's "Failed to fetch") transports -- this
  // console runs as Flutter Web, where a genuine connection failure never
  // reaches Dart as a SocketException at all, only as a generic
  // http.ClientException wrapping the browser's own fetch error text.
  if (lower.contains('connection refused') ||
      lower.contains('failed host lookup') ||
      lower.contains('socketexception') ||
      lower.contains('failed to fetch') ||
      lower.contains('clientexception')) {
    return ErrorSummary(
      headline: "Can't reach factoryd",
      nextStep: 'Is `factoryd serve` running, and is CORS enabled (see #171)?',
      raw: raw,
    );
  }

  return ErrorSummary(
    headline: 'Something went wrong',
    nextStep: 'See details below, or try again.',
    raw: raw,
  );
}

/// The shared rendering of an [ErrorSummary]: headline + next step as the
/// primary text, an optional Retry action, and the raw exception demoted
/// into a collapsed "Details" tile rather than deleted. Used at every
/// call site that used to render a bare `Text('...: $error')`.
class ErrorCallout extends StatelessWidget {
  const ErrorCallout({
    required this.error,
    this.onRetry,
    this.startClass = false,
    super.key,
  });

  final Object error;

  /// Shows a Retry button when given. Only pass this where the screen has
  /// no other visible way to retry (e.g. no refresh/load button already
  /// on screen) -- most screens already have one.
  final VoidCallback? onRetry;

  /// See [describeError]'s own doc comment for [startClass] -- pass true
  /// from a screen whose call is start-token-gated.
  final bool startClass;

  @override
  Widget build(BuildContext context) {
    final summary = describeError(error, startClass: startClass);
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          summary.headline,
          style: Theme.of(context).textTheme.titleMedium?.copyWith(
            color: Theme.of(context).colorScheme.error,
          ),
        ),
        const SizedBox(height: 4),
        Text(summary.nextStep),
        if (onRetry != null) ...[
          const SizedBox(height: 8),
          OutlinedButton(
            key: const ValueKey('error-callout-retry-button'),
            onPressed: onRetry,
            child: const Text('Retry'),
          ),
        ],
        Theme(
          data: Theme.of(context).copyWith(dividerColor: Colors.transparent),
          child: ExpansionTile(
            key: const ValueKey('error-callout-details'),
            title: const Text('Details'),
            tilePadding: EdgeInsets.zero,
            childrenPadding: EdgeInsets.zero,
            children: [
              Align(
                alignment: Alignment.centerLeft,
                child: SelectableText(summary.raw),
              ),
            ],
          ),
        ),
      ],
    );
  }
}
