// Pure rules of the request page: which stage sections and callouts a state
// shows, what a recovery callout says, and how the pipeline stepper reads a
// request's history. No React, no clock (callers pass `now`).
import { formatLocalTimestamp } from "@/domain/elapsed";
import {
  type RequestSummary,
  type RequestTicket,
  type RequestTransition,
  type RevisionDetail,
  requestAwaitingPullRequest,
  requestAwaitingPullRequestLabel,
} from "@/domain/request";
import { requestVerbs, stateLabel } from "@/domain/status";
import { unifiedLineDiff } from "@/domain/textDiff";

/** Approve / Request changes are legal from the states whose row lists them (the server refuses any other). */
export function isReviewState(state: string): boolean {
  return requestVerbs(state).includes("approve");
}

/**
 * The server's own single next-step sentence, falling back to the
 * awaiting-pull-request label for a server predating next_action; never
 * both, and never a guess for any other state.
 */
export function nextActionText(request: RequestSummary): string | null {
  // A corrective round under way outranks the server's sentence, which is
  // written from the request record and does not know a round has started.
  const building = request.tickets.find((t) => t.activeRoundRunId !== "");
  if (building !== undefined) {
    return `A corrective round is building on ticket ${building.index}'s pull request. Nothing to do until it ends; its run is on the ticket below.`;
  }
  if (request.nextAction !== "") return request.nextAction;
  if (requestAwaitingPullRequest(request)) return requestAwaitingPullRequestLabel(request);
  return null;
}

/**
 * The "Next" banner is suppressed where its text would repeat or contradict
 * the screen: the recovery callouts already show the sentence above their
 * buttons, and in review states next_action names the CLI equivalents of
 * buttons this page has.
 */
export function showsNextBanner(request: RequestSummary): boolean {
  return (
    request.state !== "halted" &&
    request.state !== "quarantined" &&
    request.state !== "resume_review" &&
    !isReviewState(request.state)
  );
}

const RUN_PHASE_STATES: ReadonlySet<string> = new Set([
  "building",
  "pr_review",
  "halted",
  "quarantined",
]);

/** Once tickets are building (or stopped), their runs are what the operator follows. */
export function ticketsLeadContent(request: RequestSummary): boolean {
  return request.tickets.length > 0 && RUN_PHASE_STATES.has(request.state);
}

/**
 * The numbered items under a "## Acceptance criteria" heading of `spec`,
 * stopping at the next heading. Tolerant by design: no heading, or no
 * numbered items, is an empty list. A display aid, never a validator.
 */
export function parseAcceptanceCriteria(spec: string): string[] {
  const items: string[] = [];
  let inSection = false;
  for (const line of spec.split("\n")) {
    const heading = line.trimEnd();
    if (/^#{1,6}\s+/.test(heading)) {
      if (inSection) break;
      inSection = /^#{1,6}\s+acceptance criteria\s*$/i.test(heading);
      continue;
    }
    if (!inSection) continue;
    const match = /^\s*\d+[.)]\s+(.*)$/.exec(line);
    if (match?.[1] !== undefined) items.push(match[1].trim());
  }
  return items;
}

// Once the operator has approved, the spec and each ticket's plan are receipts,
// not a decision: they fold to one line. Written as the states that fold, never
// as "every state but the review ones": a state added later keeps its
// documents open until someone decides it should not, and spec_review,
// oracle_review and plan_review are never in this set (the approval is bound
// to the text on screen, so it is shown whole).
const FOLDED_CONTENT_STATES: ReadonlySet<string> = new Set([
  "planning",
  "building",
  "pr_review",
  "resume_review",
  "halted",
  "quarantined",
  "done",
  "cancelled",
]);

/** Whether the spec and ticket plans fold to a one-line disclosure in `state`. */
export function foldsContent(state: string): boolean {
  return FOLDED_CONTENT_STATES.has(state);
}

function plural(n: number, one: string): string {
  return `${n} ${one}${n === 1 ? "" : "s"}`;
}

/**
 * The one line a folded spec leaves: its title and how many acceptance
 * criteria it carries. Never empty: "spec.md" when it has neither.
 */
export function specSummary(spec: string): string {
  const title = /^#\s+(.+)$/m.exec(spec)?.[1]?.trim() ?? "";
  const criteria = parseAcceptanceCriteria(spec).length;
  const parts = [
    title,
    criteria > 0 ? `${criteria} acceptance ${criteria === 1 ? "criterion" : "criteria"}` : "",
  ];
  const line = parts.filter((p) => p !== "").join(" · ");
  return line === "" ? "spec.md" : line;
}

/**
 * The one line a folded ticket plan leaves: the files it may touch (the first
 * three, then a count) and how many steps it has. Never empty.
 */
export function planSummary(content: string): string {
  const allowed = /^Allowed-Files:\s*(.+)$/m.exec(content)?.[1] ?? "";
  const files = allowed
    .split(",")
    .map((f) => f.trim())
    .filter((f) => f !== "");
  const steps = stepCount(content);
  const parts: string[] = [];
  if (files.length > 0) {
    const shown = files.slice(0, 3).join(", ");
    parts.push(`files: ${shown}${files.length > 3 ? ` +${files.length - 3} more` : ""}`);
  }
  if (steps > 0) parts.push(plural(steps, "step"));
  return parts.length === 0 ? "plan" : parts.join(" · ");
}

// The numbered items under a "Steps" heading, up to the next heading.
function stepCount(content: string): number {
  let count = 0;
  let inSteps = false;
  for (const line of content.split("\n")) {
    const heading = line.trimEnd();
    if (/^#{1,6}\s+/.test(heading)) {
      if (inSteps) break;
      inSteps = /^#{1,6}\s+steps\s*$/i.test(heading);
      continue;
    }
    if (inSteps && /^\s*\d+[.)]\s+\S/.test(line)) count += 1;
  }
  return count;
}

/**
 * What a revision's snapshotted path is compared against: spec.md against
 * the request's own spec, a ticket spec path against that ticket's content.
 * The revision key is relative ("tickets/001.spec.md"), a ticket's specPath
 * absolute in production, so the match is by path suffix.
 */
export function currentContentFor(request: RequestSummary, path: string): string {
  if (path === "spec.md") return request.spec;
  for (const ticket of request.tickets) {
    const specPath = ticket.specPath.replaceAll("\\", "/");
    if (specPath === path || specPath.endsWith(`/${path}`)) return ticket.content;
  }
  return "";
}

/** The recovery callout of a halted or quarantined request. */
export interface RecoveryPlan {
  readonly quarantined: boolean;
  /** Accepted, only the pull request is missing: neutral. */
  readonly awaitingPullRequest: boolean;
  readonly headline: string;
  readonly explanation: string;
  readonly retryLabel: string;
  readonly showSendBack: boolean;
  readonly sendBackLabel: string;
  /** A spec_conformity quarantine's usual recovery: Send back leads, ahead of Retry. */
  readonly sendBackIsPrimary: boolean;
  /** Mirrors whichever action leads, so copying it never runs a different recovery. */
  readonly cliEquivalent: string;
  /** The quarantined ticket whose run override the callout links to; null otherwise. */
  readonly overrideTicket: RequestTicket | null;
}

export function quarantinedTicket(request: RequestSummary): RequestTicket | null {
  // The ticket at the request's own current index triggered the quarantine;
  // fall back to the first ticket with a run.
  for (const ticket of request.tickets) {
    if (ticket.index === request.ticketIndex && ticket.runId !== "") return ticket;
  }
  return request.tickets.find((t) => t.runId !== "") ?? null;
}

// The server says what a retry does: its own text for an accepted request
// with no pull request states "(no rebuild)", and the label must not say the
// opposite beside it (found dogfooding, 2026-10-05). A server that says
// nothing keeps the older rule: a retry from there is a fresh, paid run.
function retryLabel(request: RequestSummary, awaitingPullRequest: boolean): string {
  if (!awaitingPullRequest) return "Retry request";
  return /no rebuild/i.test(`${request.nextAction}\n${request.error}`)
    ? "Retry request (opens the PR only)"
    : "Retry request (rebuilds)";
}

export function recoveryPlan(request: RequestSummary): RecoveryPlan {
  const quarantined = request.state === "quarantined";
  const awaitingPullRequest = requestAwaitingPullRequest(request);
  const headline = quarantined
    ? "This request is quarantined."
    : awaitingPullRequest
      ? "Built and verified; no pull request was opened."
      : "This request is halted.";
  const explanation =
    request.nextAction !== ""
      ? request.nextAction
      : "Retry the request to move it forward again, or cancel it to abandon it.";
  const specConformity = request.quarantineCheck === "spec_conformity";
  const target = specConformity || !request.canSendBackToPlan ? "spec" : "plan";
  const sendBackIsPrimary = request.canSendBack && specConformity;
  return {
    quarantined,
    awaitingPullRequest,
    headline,
    explanation,
    retryLabel: retryLabel(request, awaitingPullRequest),
    showSendBack: request.canSendBack,
    sendBackLabel: target === "plan" ? "Send back to planning" : "Send back to spec",
    sendBackIsPrimary,
    cliEquivalent: sendBackIsPrimary
      ? `factoryd reject -to ${target} -reason "<what to change>" ${request.id}`
      : `factoryd retry ${request.id}`,
    overrideTicket: quarantined ? quarantinedTicket(request) : null,
  };
}

/** The resume_review callout: the lost step and which decisions are open. */
export interface ResumePlan {
  readonly headline: string;
  readonly prompt: string;
  readonly refused: readonly string[];
  /** A lost build offers Resume and Rebuild; any other step one "Rerun step". */
  readonly isBuild: boolean;
}

export function resumePlan(request: RequestSummary): ResumePlan {
  const lost = request.resume?.fromState ?? "";
  return {
    headline:
      lost === ""
        ? "A step was lost when the worker stopped."
        : `The ${stateLabel(lost)} step was lost when the worker stopped.`,
    prompt: request.error !== "" ? request.error : request.nextAction,
    refused: request.resume?.refused ?? [],
    isBuild: lost === "" || lost === "building",
  };
}

/** The canonical, linear pipeline; terminal states are not steps. */
const PIPELINE_STEPS = [
  "submitted",
  "spec_drafting",
  "spec_review",
  "oracle_drafting",
  "oracle_review",
  "planning",
  "plan_review",
  "building",
  "pr_review",
  "done",
] as const;

// The two opt-in oracle steps (`-draft-oracles`): hidden unless the request
// asked for them or has been in one.
const OPTIONAL_ORACLE_STEPS: ReadonlySet<string> = new Set(["oracle_drafting", "oracle_review"]);

const STEP_LABELS: Readonly<Record<string, string>> = {
  submitted: "Submitted",
  spec_drafting: "Spec drafting",
  spec_review: "Spec review",
  oracle_drafting: "Oracle drafting",
  oracle_review: "Oracle review",
  planning: "Planning",
  plan_review: "Plan review",
  building: "Building",
  pr_review: "PR review",
  done: "Done",
};

export type StepStatus = "done" | "current" | "pending" | "failed" | "needsYou" | "stopped";

export interface PipelineStep {
  readonly step: string;
  readonly label: string;
  readonly status: StepStatus;
  /** The history entry that reached this step (the move out of it, for the step a request stopped at). */
  readonly entry: RequestTransition | null;
  /** The current building step shows ticket progress and elapsed time instead of an entry. */
  readonly showsBuildProgress: boolean;
}

const TERMINAL_STATES: ReadonlySet<string> = new Set(["quarantined", "halted", "cancelled"]);

/**
 * The step a terminal request stopped at, and the move that took it out of
 * it. The last entry's `from` is not enough: a request cancelled out of
 * quarantine moved `quarantined -> cancelled`, and `quarantined` is not a
 * pipeline step, so every step read pending (a real cancelled request). The
 * most recent move out of a pipeline step is where it stopped.
 */
function stoppedAt(
  request: RequestSummary,
  steps: readonly string[],
): { readonly step: string; readonly entry: RequestTransition } | null {
  for (let i = request.history.length - 1; i >= 0; i--) {
    const entry = request.history[i];
    if (entry !== undefined && steps.includes(entry.from)) return { step: entry.from, entry };
  }
  return null;
}

/**
 * Where the request is in the pipeline: each step's glyph status and the
 * history entry behind it. A terminal request marks the step it stopped at
 * (see `stoppedAt`) as failed, as needs-you when only the pull request is
 * missing, or as stopped when the operator cancelled it; the steps before it
 * are done. A record with no history falls back to `state` for the current
 * step and leaves the rest pending.
 */
export function pipelineSteps(request: RequestSummary): PipelineStep[] {
  const terminal = TERMINAL_STATES.has(request.state);
  const resumeReview = request.state === "resume_review";
  const visitedOracleStage =
    request.draftOracles ||
    OPTIONAL_ORACLE_STEPS.has(request.state) ||
    request.history.some((e) => OPTIONAL_ORACLE_STEPS.has(e.to));
  const steps = PIPELINE_STEPS.filter((s) => visitedOracleStage || !OPTIONAL_ORACLE_STEPS.has(s));
  const stopped = terminal ? stoppedAt(request, steps) : null;
  const activeState = resumeReview
    ? (request.resume?.fromState ?? "")
    : terminal
      ? (stopped?.step ?? "")
      : request.state;
  const currentIndex = steps.findIndex((s) => s === activeState);

  const statusAt = (i: number): StepStatus => {
    if (i < currentIndex) return "done";
    if (i > currentIndex) return "pending";
    if (resumeReview) return "needsYou";
    if (!terminal) return "current";
    if (requestAwaitingPullRequest(request)) return "needsYou";
    return request.state === "cancelled" ? "stopped" : "failed";
  };
  const lastEntryReaching = (step: string): RequestTransition | null => {
    for (let i = request.history.length - 1; i >= 0; i--) {
      const entry = request.history[i];
      if (entry?.to === step) return entry;
    }
    return null;
  };

  return steps.map((step, i) => {
    const status = statusAt(i);
    return {
      step,
      label: STEP_LABELS[step] ?? step,
      status,
      entry: terminal && i === currentIndex ? (stopped?.entry ?? null) : lastEntryReaching(step),
      showsBuildProgress: step === "building" && status === "current",
    };
  });
}

/** How a terminal request ended, drawn under the steps. */
export interface PipelineOutcome {
  readonly state: string;
  readonly label: string;
  /** The move into the terminal state: who, when, why. */
  readonly entry: RequestTransition | null;
  /** The stopped step already prints this entry, so the row need not repeat its reason. */
  readonly reasonShownAtStep: boolean;
}

/**
 * The terminal state a request is in, visible as such: cancelled,
 * quarantined or halted. Null for a live request, and for a halt where only
 * the pull request is missing (nothing failed; its step reads needs-you).
 */
export function pipelineOutcome(request: RequestSummary): PipelineOutcome | null {
  if (!TERMINAL_STATES.has(request.state) || requestAwaitingPullRequest(request)) return null;
  let entry: RequestTransition | null = null;
  for (let i = request.history.length - 1; i >= 0; i--) {
    const candidate = request.history[i];
    if (candidate?.to === request.state) {
      entry = candidate;
      break;
    }
  }
  const stepEntry = pipelineSteps(request).find(
    (s) => s.status === "failed" || s.status === "stopped",
  )?.entry;
  return {
    state: request.state,
    label: stateLabel(request.state),
    entry,
    reasonShownAtStep: entry !== null && stepEntry === entry,
  };
}

/** Every history entry that moved the request into cancelled, quarantined or halted, oldest first. */
export function terminalEntries(request: RequestSummary): RequestTransition[] {
  return request.history.filter((e) => TERMINAL_STATES.has(e.to));
}

/** "Cancelled by jane at 2026-09-12 10:00:00". */
export function terminalEntryLine(entry: RequestTransition): string {
  return `${stateLabel(entry.to)} by ${entry.by === "" ? "the factory" : entry.by} at ${formatLocalTimestamp(entry.at)}`;
}

/**
 * Whether a history entry was made by the factory itself rather than by an
 * operator. History says "factory" in some entries and "factoryd" in others
 * for the same thing, so neither is worth printing beside a step: only a
 * person's name is a decision someone should be able to see.
 */
export function isAutomatedActor(by: string): boolean {
  return by === "" || by === "factory" || by === "factoryd";
}

/**
 * Whether the stepper draws a step's own lines (when, who, why) or leaves it
 * a bare line. A pending step has none. A completed step keeps them only when
 * a person decided it: the approval or rejection that moved the request on is
 * the audit trail. The current, failed and needs-you steps always keep them.
 */
export function stepShowsDetail(step: Pick<PipelineStep, "status" | "entry">): boolean {
  if (step.status === "pending") return false;
  if (step.status !== "done") return true;
  return step.entry !== null && !isAutomatedActor(step.entry.by);
}

/** "Approved by jane at 2026-09-12 10:00:00". */
export function approvedLine(by: string, at: string): string {
  return `Approved by ${by} at ${formatLocalTimestamp(at)}`;
}

/** The heading of one rejection-history entry. */
export function rejectionHeading(rejection: {
  readonly by: string;
  readonly at: string;
  readonly fromState: string;
  readonly forStage: string | null;
}): string {
  const when = formatLocalTimestamp(rejection.at);
  return rejection.forStage === null
    ? `Rejected by ${rejection.by} at ${when} (from ${rejection.fromState})`
    : `Sent back by ${rejection.by} at ${when} (from ${rejection.fromState}, for ${rejection.forStage})`;
}

/** One file of the request the page shows as text: spec.md, or a ticket's plan file. */
export interface ContentFile {
  /** "spec" or "ticket-N": names the file for the one-editor-at-a-time state. */
  readonly id: string;
  readonly ticket: RequestTicket | null;
  /**
   * Whether an Edit control is offered: only in the review state the file's
   * own PUT route accepts (spec_review for spec.md, plan_review for a
   * ticket) and only with write access. A control that would 403/409 on
   * every click is worse than none.
   */
  readonly editable: boolean;
}

/**
 * Whichever file the request has to show. Each ticket's plan file (once
 * planning produced tickets) takes precedence over spec.md, which stops
 * being the operative document; spec.md otherwise; nothing before a spec
 * exists.
 */
export function contentFiles(request: RequestSummary, canWrite: boolean): ContentFile[] {
  const withContent = request.tickets.filter((t) => t.content !== "");
  if (withContent.length > 0) {
    return withContent.map((ticket) => ({
      id: `ticket-${ticket.index}`,
      ticket,
      editable: request.state === "plan_review" && canWrite,
    }));
  }
  if (request.spec !== "") {
    return [{ id: "spec", ticket: null, editable: request.state === "spec_review" && canWrite }];
  }
  return [];
}

/**
 * The compare view's text: each snapshotted file of `detail` against the
 * request's current content for the same path, as unified diffs one after
 * another.
 */
export function revisionDiffText(request: RequestSummary, detail: RevisionDetail): string {
  let out = "";
  for (const [path, snapshot] of Object.entries(detail.files)) {
    out += `--- ${path} (revision ${detail.index})\n`;
    out += `+++ ${path} (current)\n`;
    out += unifiedLineDiff(snapshot, currentContentFor(request, path));
    out += "\n";
  }
  return out;
}

/**
 * What an open editor tells its operator when a newer record arrives under
 * it. States the request's new state, and what that means for the text: a
 * changed file (Save will show the difference, the server's 409 stays the
 * arbiter) or a file that can no longer be saved (the text stays to copy).
 */
export function editChangeNotice(options: {
  readonly state: string;
  readonly contentChanged: boolean;
  readonly editable: boolean;
}): string {
  const parts = [
    `This request changed while you were editing: it is now ${stateLabel(options.state)}.`,
  ];
  if (options.contentChanged) {
    parts.push(
      "The file on the server is not the one you started from; Save will show you the difference.",
    );
  }
  if (!options.editable) {
    parts.push(
      "This file can no longer be edited in this state, so Save is off; your text stays here to copy.",
    );
  }
  return parts.join(" ");
}

/** Why Save is off for an editor whose file stopped being editable; null while it is editable. */
export function editBlockedReason(state: string, editable: boolean): string | null {
  return editable
    ? null
    : `Save is off: this file can no longer be edited (the request is now ${stateLabel(state)}).`;
}
