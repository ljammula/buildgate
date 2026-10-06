/// Shared elapsed/duration formatting (Phase 4, follow-along console):
/// run_list_screen.dart's row subtitle and run_detail_screen.dart's
/// Timeline section both need the same "mm:ss" (or "hh:mm:ss" once past an
/// hour) rendering of a wall-clock duration, and the same tolerant parsing
/// of an RFC3339 timestamp string that might be empty or malformed.
///
/// stallStatus (below) is the "silence is a bug" addition
/// (progress-contract.md, 2026-09-18): the shared rule every screen that
/// shows a run uses to decide whether to flag it as stalled. This used to
/// be computed here, independently of cmd/factoryd's own copy of the same
/// rule, and the two drifted (see internal/progress.Stalled's own doc
/// comment). The rule now lives once, server-side, in
/// internal/progress.Stalled; every route this console reads a Run from is
/// this same factoryd HTTP API (see api_client.dart -- Run.fromJson has no
/// other caller), so [stallStatus] simply trusts the server's [Run.stalled]
/// verdict instead of re-deriving it from timestamps.
library;

import 'package:flutter/material.dart';

import 'models.dart';

/// Formats [duration] as "mm:ss", or "hh:mm:ss" once it reaches an hour.
/// Negative durations (a clock skew between this client and the server)
/// render as zero rather than a confusing negative time.
String formatDuration(Duration duration) {
  final clamped = duration.isNegative ? Duration.zero : duration;
  final hours = clamped.inHours;
  final minutes = clamped.inMinutes.remainder(60);
  final seconds = clamped.inSeconds.remainder(60);
  String two(int n) => n.toString().padLeft(2, '0');
  return hours > 0
      ? '${two(hours)}:${two(minutes)}:${two(seconds)}'
      : '${two(minutes)}:${two(seconds)}';
}

/// Parses an RFC3339 timestamp, returning null (rather than throwing) for
/// an empty or unparseable value.
DateTime? tryParseTimestamp(String value) =>
    value.isEmpty ? null : DateTime.tryParse(value);

/// The elapsed time between [start] and [end] (defaulting to now when
/// [end] is null, e.g. a still-running run) -- an unparseable/empty
/// [start] renders as zero rather than throwing, since a timeline/list row
/// must always show something.
Duration elapsedBetween(String start, String? end) {
  final startTime = tryParseTimestamp(start);
  if (startTime == null) return Duration.zero;
  final endTime = end == null ? null : tryParseTimestamp(end);
  return (endTime ?? DateTime.now()).difference(startTime);
}

/// Formats an RFC3339 [value] in local time as "YYYY-MM-DD HH:MM:SS" --
/// every screen's own raw-UTC timestamp (C4, operator demo, 2026-09-26:
/// found showing UTC verbatim across the request/run/release screens)
/// routes through this one formatter rather than each keeping its own
/// copy. Falls back to the raw value verbatim for an empty/unparseable
/// timestamp rather than throwing.
String formatLocalTimestamp(String value) {
  final parsed = tryParseTimestamp(value);
  if (parsed == null) return value;
  final local = parsed.toLocal();
  String two(int n) => n.toString().padLeft(2, '0');
  return '${local.year}-${two(local.month)}-${two(local.day)} '
      '${two(local.hour)}:${two(local.minute)}:${two(local.second)}';
}

/// The value half of a label+value timestamp row: [value] rendered in
/// local time via [formatLocalTimestamp], with the full UTC value on
/// hover/long-press via [Tooltip] rather than only available as raw text.
/// Falls back to plain [SelectableText] of the raw value for an
/// empty/unparseable timestamp. Callers needing a label alongside this
/// combine it with their own screen's `_Field` row, same as any other
/// `_TextField`-shaped value.
class LocalTimeText extends StatelessWidget {
  const LocalTimeText(this.value, {super.key});

  final String value;

  @override
  Widget build(BuildContext context) {
    final parsed = tryParseTimestamp(value);
    if (parsed == null) return SelectableText(value);
    return Tooltip(
      message: 'UTC: ${parsed.toUtc().toIso8601String()}',
      child: SelectableText(formatLocalTimestamp(value)),
    );
  }
}

/// Returns 'stalled' when the server's own [Run.stalled] verdict is true,
/// null otherwise -- see this file's own doc comment for why that verdict
/// is trusted directly rather than re-derived from timestamps here.
String? stallStatus(Run run) => run.stalled ? 'stalled' : null;

/// The "silence is a bug" chip: a red 'stalled' chip when the server
/// reports [Run.stalled] (see [stallStatus]), or an amber "waiting: `
/// reason`" chip when the server reports a [Run.waitingReason] (queued
/// behind another run, e.g.) -- shown instead, since a run the factory
/// itself has already explained the silence for is not the same as
/// genuine, unexplained silence. Renders nothing (a zero-size box) when
/// neither applies, so callers can lay it out unconditionally.
class StallChip extends StatelessWidget {
  const StallChip({required this.run, super.key});

  final Run run;

  @override
  Widget build(BuildContext context) {
    if (stallStatus(run) == 'stalled') {
      return const Chip(
        key: ValueKey('stalled-chip'),
        avatar: Icon(Icons.warning_amber, size: 16, color: Colors.white),
        label: Text('stalled', style: TextStyle(color: Colors.white)),
        backgroundColor: Colors.red,
      );
    }
    final waitingReason = run.waitingReason;
    if (waitingReason != null && waitingReason.isNotEmpty) {
      return Chip(
        key: const ValueKey('waiting-chip'),
        avatar: const Icon(Icons.hourglass_top, size: 16, color: Colors.white),
        label: Text(
          'waiting: $waitingReason',
          style: const TextStyle(color: Colors.white),
        ),
        backgroundColor: Colors.amber.shade800,
      );
    }
    return const SizedBox.shrink();
  }
}
