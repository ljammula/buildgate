// The console's one shared semantic-status vocabulary: every chip across
// every screen renders one of these five statuses, so the same colour and
// icon always means the same thing to an operator regardless of which screen
// they are looking at.
//
// The Flutter console named a Material colour and icon per status. Here each
// is a semantic name the UI layer maps to a real colour and icon:
//
//   Dart colour                     StatusTone      used for
//   Colors.amber.shade800           "warning"       needsHuman; the waiting chip
//   Colors.blue                     "info"          working
//   Colors.green                    "success"       done
//   Colors.red                      "danger"        failed; stalled; kill switch engaged
//   Colors.grey.shade700 (light)    "neutral"       unknown on a light surface
//   Colors.grey.shade400 (dark)     "neutralOnDark" unknown on a dark surface
//   Colors.blueGrey                 "muted"         kill switch clear
//
//   Dart icon                       StatusIcon
//   Icons.priority_high             "priority_high"
//   Icons.autorenew                 "autorenew"
//   Icons.check_circle              "check_circle"
//   Icons.error                     "error"
//   Icons.help_outline              "help_outline"
//   Icons.warning_amber             "warning_amber"
//   Icons.check_circle_outline      "check_circle_outline"

/**
 * needsHuman must be reserved for states that are genuinely operator-actionable
 * right now; unknown is reserved for anything this module does not recognize
 * (an unmapped state string, or a fetch that failed): an unrecognized value is
 * always rendered as unknown, never silently hidden or guessed into one of
 * the other four.
 */
export type Status = "needsHuman" | "working" | "done" | "failed" | "unknown";

export type StatusTone =
  "warning" | "info" | "success" | "danger" | "neutral" | "neutralOnDark" | "muted";

export type StatusIcon =
  | "priority_high"
  | "autorenew"
  | "check_circle"
  | "error"
  | "help_outline"
  | "warning_amber"
  | "check_circle_outline";

export type Brightness = "light" | "dark";

/**
 * Every state-machine/PR-review string literal this console renders, mapped
 * to its one semantic Status. Deliberately a single flat, one-directional
 * table (string -> semantic) shared by every domain (request state, run
 * state, PR review state) rather than one map per domain: a per-domain table
 * would let the same word (e.g. "closed") drift to a different colour
 * depending only on which screen happened to render it, precisely the
 * inconsistency this module exists to remove.
 *
 * This never changes what any of these strings mean to the state machines
 * that produce them (request.go, run.go); it only assigns a display colour
 * and icon to each. Adding a case here is purely additive to callers already
 * using statusForToken; the fallback is always "unknown", never a crash or a
 * hidden row.
 */
const STATUS_BY_TOKEN: Readonly<Record<string, Status>> = {
  // needsHuman (amber): operator-actionable right now.
  spec_review: "needsHuman",
  oracle_review: "needsHuman",
  plan_review: "needsHuman",
  changes_requested: "needsHuman",
  halted: "needsHuman",
  resume_review: "needsHuman",
  // working (blue): the factory is still doing something.
  submitted: "working",
  spec_drafting: "working",
  oracle_drafting: "working",
  planning: "working",
  building: "working",
  pr_review: "working",
  slice_running: "working",
  ready: "working",
  verifying: "working",
  open: "working",
  draft: "working",
  // done (green): reached a positive terminal state.
  done: "done",
  accepted: "done",
  approved: "done",
  merged: "done",
  // failed (red): reached a negative terminal state.
  quarantined: "failed",
  cancelled: "failed",
  closed: "failed",
};

/**
 * Maps one state-machine/PR-review string to its semantic Status. Any string
 * not in the table above, including one this console has simply never seen
 * before, renders as "unknown" rather than being hidden or mis-coloured.
 */
export function statusForToken(token: string): Status {
  return Object.hasOwn(STATUS_BY_TOKEN, token) ? (STATUS_BY_TOKEN[token] ?? "unknown") : "unknown";
}

/**
 * A ticket PR's state as a Status: like statusForToken, except a `ready` PR
 * (draft cleared, waiting on its reviewer) needs a human. The shared table
 * keeps `ready` as working because a run's own `ready` state means the
 * factory is about to act.
 */
export function statusForPRState(prState: string): Status {
  switch (prState) {
    // Waiting on the reviewer, or (stacked) on a human merging the ticket
    // PR it is stacked on.
    case "ready":
    case "stacked":
      return "needsHuman";
    default:
      return statusForToken(prState);
  }
}

/**
 * The factory's release decision (`ReleaseDecision.allowed`) is a bool, not a
 * state string, so it gets its own tiny mapping rather than a synthetic token
 * in STATUS_BY_TOKEN.
 */
export function statusForReleaseDecision(allowed: boolean): Status {
  return allowed ? "done" : "failed";
}

/**
 * The tone for `status`. "unknown" gets its own distinct grey so it can never
 * be confused with a real failure; it is additionally rendered outlined, not
 * filled.
 *
 * `brightness` only changes unknown's grey: the other four render on their
 * own filled, saturated background with white text, which stays equally
 * legible regardless of the surrounding surface, so there is nothing to
 * retune. Unknown instead renders outlined and transparent, so its grey sits
 * directly against whatever the surrounding surface is; a single mid-tone
 * grey that reads fine on a light background loses contrast against a
 * near-black dark surface, so dark mode gets a lighter shade and light mode a
 * darker one.
 */
export function statusTone(status: Status, brightness: Brightness = "light"): StatusTone {
  switch (status) {
    case "needsHuman":
      return "warning";
    case "working":
      return "info";
    case "done":
      return "success";
    case "failed":
      return "danger";
    case "unknown":
      return brightness === "dark" ? "neutralOnDark" : "neutral";
  }
}

/**
 * The icon paired with `status`: present alongside colour so status stays
 * distinguishable by shape alone, not hue alone. Every chip built on Status
 * carries an icon from day one rather than needing a second retrofit later.
 */
export function statusIcon(status: Status): StatusIcon {
  switch (status) {
    case "needsHuman":
      return "priority_high";
    case "working":
      return "autorenew";
    case "done":
      return "check_circle";
    case "failed":
      return "error";
    case "unknown":
      return "help_outline";
  }
}

/**
 * Operator-facing words for the request/run state tokens (chips used to show
 * `spec_review`/`slice_running` verbatim). Purely a display mapping: callers
 * keep passing the raw token, and the raw token stays one hover away. Anything
 * not listed, such as a composed label like "accepted · awaiting PR" or a
 * token this console has never seen, renders unchanged.
 */
const STATE_LABELS: Readonly<Record<string, string>> = {
  submitted: "Submitted",
  spec_drafting: "Drafting spec",
  spec_review: "Spec review",
  oracle_drafting: "Drafting oracles",
  oracle_review: "Oracle review",
  planning: "Planning",
  plan_review: "Plan review",
  building: "Building",
  pr_review: "PR review",
  done: "Done",
  halted: "Halted",
  resume_review: "Resume?",
  quarantined: "Quarantined",
  cancelled: "Cancelled",
  ready: "Ready",
  slice_running: "Building",
  verifying: "Verifying",
  accepted: "Accepted",
  changes_requested: "Changes requested",
};

/** The display word for `token`; `token` itself when unmapped. */
export function stateLabel(token: string): string {
  return Object.hasOwn(STATE_LABELS, token) ? (STATE_LABELS[token] ?? token) : token;
}

/** What the kill switch's own three-state chip shows. */
export interface KillSwitchDisplay {
  readonly label: string;
  readonly tone: StatusTone;
  readonly icon: StatusIcon;
  /** Unknown is outlined, the other two filled. */
  readonly outlined: boolean;
}

/**
 * The kill switch's own three-state display, deliberately not folded into
 * Status: the kill switch is engaged/clear/unknown, a distinct axis of meaning
 * from "is this request/run/PR/release in a state that needs, is getting, or
 * has finished getting operator attention". Collapsing it into Status would
 * risk a future refactor giving "engaged" the same styling as some unrelated
 * "failed" state.
 *
 * `engaged` is null exactly when the kill switch's own state could not be
 * determined (a failed fetch, or a screen that has not loaded one yet): this
 * must render as "unknown", never coerced to "clear". That coercion is the
 * exact PR #64 regression: an operator seeing "clear" must be able to trust
 * the switch was actually confirmed off, not that its state simply could not
 * be read. Unknown uses the same brightness-aware grey as Status unknown.
 */
export function killSwitchDisplay(
  engaged: boolean | null,
  brightness: Brightness = "light",
): KillSwitchDisplay {
  if (engaged === true) {
    return { label: "Kill switch engaged", tone: "danger", icon: "warning_amber", outlined: false };
  }
  if (engaged === false) {
    return {
      label: "Kill switch clear",
      tone: "muted",
      icon: "check_circle_outline",
      outlined: false,
    };
  }
  return {
    label: "Kill switch unknown",
    tone: brightness === "dark" ? "neutralOnDark" : "neutral",
    icon: "help_outline",
    outlined: true,
  };
}
