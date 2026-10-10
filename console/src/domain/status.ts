// The console's one shared semantic-status vocabulary: every chip across
// every screen renders one of these five statuses, so the same colour and
// icon always means the same thing to an operator regardless of which screen
// they are looking at.
//
// Each status is a semantic name the UI layer maps to a real colour and icon:
//
//   StatusTone      used for
//   "warning"       needsHuman; the waiting chip
//   "info"          working
//   "success"       done
//   "danger"        failed; stalled; kill switch engaged
//   "neutral"       unknown on a light surface
//   "neutralOnDark" unknown on a dark surface
//   "muted"         kill switch clear
//
// StatusIcon names are lucide's own, in snake case; ui/statusIcons maps each
// to its component.

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

/**
 * One shape per state family; the name is the lucide icon's, in snake case.
 *   circle_alert      waits on the operator
 *   circle_dot        in progress: the factory is working
 *   circle_check      passed or done
 *   circle_x          failed
 *   circle_help       unknown
 *   triangle_alert    stalled, or a warning
 *   circle_check_big  confirmed clear
 *   octagon_pause     stuck: the factory cannot move it
 *   circle_dashed     not started, queued or connecting
 *   circle_minus      skipped
 *   hourglass         waiting its turn
 */
export type StatusIcon =
  | "circle_alert"
  | "circle_dot"
  | "circle_check"
  | "circle_x"
  | "circle_help"
  | "triangle_alert"
  | "circle_check_big"
  | "octagon_pause"
  | "circle_dashed"
  | "circle_minus"
  | "hourglass";

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
      return "circle_alert";
    case "working":
      return "circle_dot";
    case "done":
      return "circle_check";
    case "failed":
      return "circle_x";
    case "unknown":
      return "circle_help";
  }
}

const STUCK_TOKENS: ReadonlySet<string> = new Set(["halted", "resume_review", "quarantined"]);

/**
 * The icon for a state token: the stuck states (the factory cannot move them
 * on its own) get the octagon, every other token the icon of its Status.
 */
export function statusIconForToken(token: string): StatusIcon {
  return STUCK_TOKENS.has(token) ? "octagon_pause" : statusIcon(statusForToken(token));
}

/** What an operator can do to a request, each action named once for every screen. */
export const REQUEST_VERBS = {
  approve: "Approve",
  requestChanges: "Request changes",
  retry: "Retry request",
  sendBack: "Send back",
  resume: "Resume",
  rebuild: "Rebuild from scratch",
  rerun: "Rerun step",
  cancel: "Cancel request",
} as const;

export type RequestVerb = keyof typeof REQUEST_VERBS;

interface RequestStateRow {
  /** The operator's word for the state, in Title case. */
  readonly label: string;
  /**
   * The actions a screen may offer in the state. The server still decides
   * each one (a send-back needs `can_send_back`, a resume its own options):
   * a verb listed here is never offered where the server would refuse it.
   */
  readonly verbs: readonly RequestVerb[];
}

const REVIEW: readonly RequestVerb[] = ["approve", "requestChanges"];
const RECOVERY: readonly RequestVerb[] = ["retry", "sendBack", "cancel"];

/** One row per request state: its label and the operator's verbs there. */
const REQUEST_STATES: Readonly<Record<string, RequestStateRow>> = {
  submitted: { label: "Submitted", verbs: [] },
  spec_drafting: { label: "Spec drafting", verbs: [] },
  spec_review: { label: "Spec review", verbs: REVIEW },
  oracle_drafting: { label: "Oracle drafting", verbs: [] },
  oracle_review: { label: "Oracle review", verbs: REVIEW },
  planning: { label: "Planning", verbs: [] },
  plan_review: { label: "Plan review", verbs: REVIEW },
  building: { label: "Building", verbs: [] },
  pr_review: { label: "PR review", verbs: [] },
  done: { label: "Done", verbs: [] },
  halted: { label: "Halted", verbs: RECOVERY },
  resume_review: { label: "Needs resume", verbs: ["resume", "rebuild", "rerun", "cancel"] },
  quarantined: { label: "Quarantined", verbs: RECOVERY },
  cancelled: { label: "Cancelled", verbs: [] },
};

/** The actions a screen may offer on a request in `state`; none for a state not listed. */
export function requestVerbs(state: string): readonly RequestVerb[] {
  return Object.hasOwn(REQUEST_STATES, state) ? (REQUEST_STATES[state]?.verbs ?? []) : [];
}

/** A request that is accepted with only its pull request missing: a composed label, not a state. */
export const AWAITING_PR_LABEL = "Accepted · awaiting PR";

/**
 * Operator-facing words for the run state tokens the request table does not
 * name. With REQUEST_STATES this is purely a display mapping: callers keep
 * passing the raw token, and the raw token stays one hover away. Anything not
 * listed (a composed label, a token this console has never seen) renders
 * unchanged.
 */
const RUN_STATE_LABELS: Readonly<Record<string, string>> = {
  ready: "Ready",
  slice_running: "Building",
  verifying: "Verifying",
  accepted: "Accepted",
  changes_requested: "Changes requested",
};

/** The display word for `token`; `token` itself when unmapped. */
export function stateLabel(token: string): string {
  if (Object.hasOwn(REQUEST_STATES, token)) return REQUEST_STATES[token]?.label ?? token;
  return Object.hasOwn(RUN_STATE_LABELS, token) ? (RUN_STATE_LABELS[token] ?? token) : token;
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
    return {
      label: "Kill switch engaged",
      tone: "danger",
      icon: "triangle_alert",
      outlined: false,
    };
  }
  if (engaged === false) {
    return {
      label: "Kill switch clear",
      tone: "muted",
      icon: "circle_check_big",
      outlined: false,
    };
  }
  return {
    label: "Kill switch unknown",
    tone: brightness === "dark" ? "neutralOnDark" : "neutral",
    icon: "circle_help",
    outlined: true,
  };
}
