// Pure rules of the request page: which stage sections and callouts a state
// shows, what a recovery callout says, and how the pipeline stepper reads a
// request's history. No React, no clock (callers pass `now`).
import { ApiError } from "@/domain/apiError";
import { formatLocalTimestamp, tryParseTimestamp } from "@/domain/elapsed";
import {
  type RequestSummary,
  type RequestTicket,
  type RequestTransition,
  type RevisionDetail,
  requestAwaitingPullRequest,
  requestAwaitingPullRequestLabel,
} from "@/domain/request";
import { stateLabel } from "@/domain/status";
import { unifiedLineDiff } from "@/domain/textDiff";

/** Approve / Request changes are legal from these states only (the server refuses any other). */
export function isReviewState(state: string): boolean {
  return state === "spec_review" || state === "oracle_review" || state === "plan_review";
}

/**
 * The server's own single next-step sentence, falling back to the
 * awaiting-pull-request label for a server predating next_action; never
 * both, and never a guess for any other state.
 */
export function nextActionText(request: RequestSummary): string | null {
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

/** The server's own text for a failed write; a 503 is marked as retryable. */
export function errorText(error: unknown): string {
  if (error instanceof ApiError) {
    return error.isRetryable
      ? `${error.serverMessage} (temporary: try again)`
      : error.serverMessage;
  }
  return error instanceof Error ? error.message : String(error);
}

/** The recovery callout of a halted or quarantined request. */
export interface RecoveryPlan {
  readonly quarantined: boolean;
  /** Accepted, only the pull request is missing: neutral, and Retry rebuilds. */
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
    retryLabel: awaitingPullRequest ? "Retry request (rebuilds)" : "Retry request",
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

export type StepStatus = "done" | "current" | "pending" | "failed" | "needsYou";

export interface PipelineStep {
  readonly step: string;
  readonly label: string;
  readonly status: StepStatus;
  /** The history entry that reached this step (the terminal move itself, for a failed one). */
  readonly entry: RequestTransition | null;
  /** The current building step shows ticket progress and elapsed time instead of an entry. */
  readonly showsBuildProgress: boolean;
}

/**
 * Where the request is in the pipeline: each step's glyph status and the
 * history entry behind it. A terminal request marks the step it was on when
 * it stopped (the one the last entry moved out of) as failed, or as
 * needs-you when only the pull request is missing. A record with no history
 * falls back to `state` for the current step and leaves the rest pending.
 */
export function pipelineSteps(request: RequestSummary): PipelineStep[] {
  const terminal =
    request.state === "quarantined" || request.state === "halted" || request.state === "cancelled";
  const resumeReview = request.state === "resume_review";
  const last = request.history[request.history.length - 1] ?? null;
  const activeState = resumeReview
    ? (request.resume?.fromState ?? "")
    : terminal
      ? (last?.from ?? "")
      : request.state;
  const visitedOracleStage =
    request.draftOracles ||
    OPTIONAL_ORACLE_STEPS.has(request.state) ||
    request.history.some((e) => OPTIONAL_ORACLE_STEPS.has(e.to));
  const steps = PIPELINE_STEPS.filter((s) => visitedOracleStage || !OPTIONAL_ORACLE_STEPS.has(s));
  const currentIndex = steps.findIndex((s) => s === activeState);
  const failureEntry = terminal ? last : null;

  const statusAt = (i: number): StepStatus => {
    if (i < currentIndex) return "done";
    if (i > currentIndex) return "pending";
    if (resumeReview) return "needsYou";
    if (!terminal) return "current";
    return requestAwaitingPullRequest(request) ? "needsYou" : "failed";
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
      entry: terminal && i === currentIndex ? failureEntry : lastEntryReaching(step),
      showsBuildProgress: step === "building" && status === "current",
    };
  });
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * An RFC 3339 timestamp as local "HH:mm" when it falls on `now`'s day, or
 * "MMM d HH:mm" otherwise; the raw value when empty or unparseable.
 */
export function formatWhen(at: string, now: Date): string {
  const parsed = tryParseTimestamp(at);
  if (parsed === null) return at;
  const two = (n: number): string => n.toString().padStart(2, "0");
  const hm = `${two(parsed.getHours())}:${two(parsed.getMinutes())}`;
  const sameDay =
    parsed.getFullYear() === now.getFullYear() &&
    parsed.getMonth() === now.getMonth() &&
    parsed.getDate() === now.getDate();
  return sameDay ? hm : `${MONTHS[parsed.getMonth()] ?? ""} ${parsed.getDate()} ${hm}`;
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
