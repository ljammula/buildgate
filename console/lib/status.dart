import 'package:flutter/material.dart';

/// The console's one shared semantic-status vocabulary: every chip
/// across every screen renders one of these five statuses, so the same
/// color/icon always means the same thing to an
/// operator regardless of which screen they're looking at.
///
/// [needsHuman] must be reserved for states that are genuinely
/// operator-actionable right now; [unknown] is reserved for anything this
/// module does not recognize (an unmapped state string, or a fetch that
/// failed) -- an unrecognized value is always rendered as [unknown], never
/// silently hidden or guessed into one of the other four.
enum Status { needsHuman, working, done, failed, unknown }

/// Every state-machine/PR-review string literal this console renders,
/// mapped to its one semantic [Status]. Deliberately a single flat,
/// one-directional table (string -> semantic) shared by every domain
/// (request state, run state, PR review state) rather than one map per
/// domain: a per-domain table would let the same word (e.g. "closed") drift to a
/// different color depending only on which screen happened to render it
/// -- precisely the inconsistency this module exists to remove.
///
/// This never changes what any of these strings *mean* to the state
/// machines that produce them (request.go, run.go) -- it only assigns a
/// display color/icon to each. Adding a case here is purely additive to
/// callers already using [statusForToken]; the fallback is always
/// [Status.unknown], never a crash or a hidden row.
const Map<String, Status> _statusByToken = {
  // needsHuman (amber): operator-actionable right now.
  'spec_review': Status.needsHuman,
  'oracle_review': Status.needsHuman,
  'plan_review': Status.needsHuman,
  'changes_requested': Status.needsHuman,
  'halted': Status.needsHuman,
  'resume_review': Status.needsHuman,
  // working (blue): the factory is still doing something.
  'submitted': Status.working,
  'spec_drafting': Status.working,
  'oracle_drafting': Status.working,
  'planning': Status.working,
  'building': Status.working,
  'pr_review': Status.working,
  'slice_running': Status.working,
  'ready': Status.working,
  'verifying': Status.working,
  'open': Status.working,
  'draft': Status.working,
  // done (green): reached a positive terminal state.
  'done': Status.done,
  'accepted': Status.done,
  'approved': Status.done,
  'merged': Status.done,
  // failed (red): reached a negative terminal state.
  'quarantined': Status.failed,
  'cancelled': Status.failed,
  'closed': Status.failed,
};

/// Maps one state-machine/PR-review string to its semantic [Status].
/// Any string not in the table above -- including one this console has
/// simply never seen before -- renders as [Status.unknown] rather than
/// being hidden or mis-colored; see [Status.unknown]'s own doc comment.
Status statusForToken(String token) => _statusByToken[token] ?? Status.unknown;

/// A ticket PR's state as a [Status]: like [statusForToken], except a
/// `ready` PR (draft cleared, waiting on its reviewer) needs a human. The
/// shared table keeps `ready` as working because a run's own `ready`
/// state means the factory is about to act.
Status statusForPRState(String prState) => switch (prState) {
  // Waiting on the reviewer, or (stacked) on a human merging the ticket
  // PR it is stacked on.
  'ready' || 'stacked' => Status.needsHuman,
  _ => statusForToken(prState),
};

/// The factory's release decision (`ReleaseDecision.allowed`) is a bool,
/// not a state string, so it gets its own tiny mapping rather than a
/// synthetic token in [_statusByToken].
Status statusForReleaseDecision({required bool allowed}) =>
    allowed ? Status.done : Status.failed;

/// The fill color for [status]. [Status.unknown] gets its own distinct
/// grey so it can never be confused with a real failure ([Status.failed]
/// is also somewhat dark, but a different hue) -- see [StatusChip] for
/// why [Status.unknown] is additionally rendered outlined, not filled.
///
/// [brightness] only changes [Status.unknown]'s
/// grey: the other four render on their own filled, saturated
/// background (a [Chip]'s `backgroundColor`) with white text, which
/// stays equally legible regardless of the surrounding scaffold's
/// brightness -- there is nothing to retune. [Status.unknown] instead
/// renders outlined/transparent (see [StatusChip]), so its grey sits
/// directly against whatever the surrounding surface is; a single
/// mid-tone grey that reads fine on a light background loses contrast
/// against a near-black Material3 dark surface, so dark mode gets a
/// lighter shade and light mode a darker one.
Color statusColor(Status status, {Brightness brightness = Brightness.light}) =>
    switch (status) {
      Status.needsHuman => Colors.amber.shade800,
      Status.working => Colors.blue,
      Status.done => Colors.green,
      Status.failed => Colors.red,
      Status.unknown =>
        brightness == Brightness.dark
            ? Colors.grey.shade400
            : Colors.grey.shade700,
    };

/// The icon paired with [status] -- present alongside color so status
/// stays distinguishable by shape alone, not hue alone (kept in mind for
/// the dark-mode/colorblind pass in a later phase, but decided here since
/// every chip built on [Status] should carry an icon from day one rather
/// than needing a second retrofit later).
IconData statusIcon(Status status) => switch (status) {
  Status.needsHuman => Icons.priority_high,
  Status.working => Icons.autorenew,
  Status.done => Icons.check_circle,
  Status.failed => Icons.error,
  Status.unknown => Icons.help_outline,
};

/// A generic chip for one [Status], with an explicit [label] (the caller
/// chooses that -- e.g. the raw state string or a nicer display string;
/// this module never invents display copy for a domain it doesn't own).
///
/// [Status.unknown] renders outlined rather than filled -- a grey,
/// distinct outline so an unrecognized value never reads, at a glance,
/// as just another filled/settled status.
/// Operator-facing words for the request/run state tokens (chips
/// used to show `spec_review`/`slice_running` verbatim).
/// Purely a display mapping: callers keep passing the raw token, and the
/// raw token stays one hover away (see [StatusChip]). Anything not listed
/// -- a composed label such as "accepted · awaiting PR", or a token this
/// console has never seen -- renders unchanged.
const Map<String, String> _stateLabels = {
  'submitted': 'Submitted',
  'spec_drafting': 'Drafting spec',
  'spec_review': 'Spec review',
  'oracle_drafting': 'Drafting oracles',
  'oracle_review': 'Oracle review',
  'planning': 'Planning',
  'plan_review': 'Plan review',
  'building': 'Building',
  'pr_review': 'PR review',
  'done': 'Done',
  'halted': 'Halted',
  'resume_review': 'Resume?',
  'quarantined': 'Quarantined',
  'cancelled': 'Cancelled',
  'ready': 'Ready',
  'slice_running': 'Building',
  'verifying': 'Verifying',
  'accepted': 'Accepted',
  'changes_requested': 'Changes requested',
};

/// The display word for [token]; [token] itself when unmapped.
String stateLabel(String token) => _stateLabels[token] ?? token;

class StatusChip extends StatelessWidget {
  const StatusChip({required this.status, required this.label, super.key});

  final Status status;

  /// The raw state token (or an already-composed label); shown through
  /// [stateLabel], with the raw token as a tooltip when they differ.
  final String label;

  @override
  Widget build(BuildContext context) {
    final color = statusColor(status, brightness: Theme.of(context).brightness);
    final icon = statusIcon(status);
    final shown = stateLabel(label);
    final Widget chip;
    if (status == Status.unknown) {
      chip = Chip(
        avatar: Icon(icon, size: 16, color: color),
        label: Text(shown, style: TextStyle(color: color)),
        backgroundColor: Colors.transparent,
        side: BorderSide(color: color),
      );
    } else {
      chip = Chip(
        avatar: Icon(icon, size: 16, color: Colors.white),
        label: Text(shown, style: const TextStyle(color: Colors.white)),
        backgroundColor: color,
      );
    }
    return shown == label ? chip : Tooltip(message: label, child: chip);
  }
}

/// The kill switch's own three-state widget -- deliberately *not* folded
/// into [Status]/[StatusChip]: the kill switch is
/// engaged/clear/unknown, a distinct axis of meaning from "is this
/// request/run/PR/release in a state that needs, is getting, or has
/// finished getting operator attention" -- collapsing it into [Status]
/// would risk a future refactor giving "engaged" the same styling as some
/// unrelated "failed" state.
///
/// [engaged] is `null` exactly when the kill switch's own state could not
/// be determined (a failed fetch, or a screen that hasn't loaded one yet)
/// -- this must render as "unknown", never coerced to "clear". That
/// coercion is the exact PR #64 regression (`ops_screen.dart`,
/// `project_release_screen.dart` before this module existed): an operator
/// seeing "clear" must be able to trust the switch was actually confirmed
/// off, not that its state simply couldn't be read.
class KillSwitchChip extends StatelessWidget {
  const KillSwitchChip({required this.engaged, super.key});

  final bool? engaged;

  @override
  Widget build(BuildContext context) {
    return switch (engaged) {
      true => const Chip(
        avatar: Icon(Icons.warning_amber, size: 16, color: Colors.white),
        label: Text(
          'Kill switch engaged',
          style: TextStyle(color: Colors.white),
        ),
        backgroundColor: Colors.red,
      ),
      false => const Chip(
        avatar: Icon(Icons.check_circle_outline, size: 16, color: Colors.white),
        label: Text('Kill switch clear', style: TextStyle(color: Colors.white)),
        backgroundColor: Colors.blueGrey,
      ),
      null => _unknownKillSwitchChip(Theme.of(context).brightness),
    };
  }
}

// Split out from KillSwitchChip.build's switch above so the `unknown`
// branch (the only one whose color needs Theme.of(context)) doesn't have
// to give up the `const` chips the engaged/clear branches still are.
// Same brightness-aware grey as Status.unknown's own -- see statusColor's
// doc comment for why the outlined/transparent variants need one and the
// filled ones don't.
Widget _unknownKillSwitchChip(Brightness brightness) {
  final color = brightness == Brightness.dark
      ? Colors.grey.shade400
      : Colors.grey.shade700;
  return Chip(
    avatar: Icon(Icons.help_outline, size: 16, color: color),
    label: Text('Kill switch unknown', style: TextStyle(color: color)),
    backgroundColor: Colors.transparent,
    side: BorderSide(color: color),
  );
}
