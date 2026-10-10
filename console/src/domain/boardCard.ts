// What one card of the Mission Control board says, per column. Pure: every
// fact is read from the request list's own record, never fetched per card.
import type { BoardColumn } from "@/domain/boardColumns";
import { stageLabels } from "@/domain/runDetail";
import { digestText } from "@/domain/digest";
import { formatAgeCompact } from "@/domain/elapsed";
import {
  type RequestSummary,
  activeJobLabel,
  requestAwaitingPullRequest,
  requestRunningJob,
  requestWaitingSinceOrEnteredAt,
} from "@/domain/request";
import { safeHttpUrl } from "@/domain/safeUrl";
import { stateLabel } from "@/domain/status";
import { escapeInvisible } from "@/domain/textEscape";

/** One ticket's pull request on a card. */
export interface CardPullRequest {
  readonly ticket: number;
  readonly prState: string;
  /** The PR's address when it is a plain http(s) URL; null otherwise (never rendered as a link then). */
  readonly url: string | null;
  /** PR-review corrective rounds that have ended on this ticket. */
  readonly reviewRounds: number;
}

/** A failure the card marks in red: a halted or quarantined request. */
export interface CardAlert {
  /** "Halted" or "Quarantined". */
  readonly label: string;
  /** The first line of why; "" when the record gives none. */
  readonly reason: string;
}

export interface CardFacts {
  /** Short text lines, most important first. */
  readonly lines: readonly string[];
  /** "Queued · position 2 · behind req-a" while the request waits for a worker slot; null otherwise. */
  readonly queue: string | null;
  readonly alert: CardAlert | null;
  /** The server's stalled verdict on the build under way. */
  readonly stalled: boolean;
  /** Opened pull requests, by ticket; empty outside PR review and Done. */
  readonly pullRequests: readonly CardPullRequest[];
}

/**
 * "Queued · position 2 · behind req-a" from the server's `queue_position` and
 * `waiting_on`; null for a request that is not waiting. The position is the
 * server's: nothing here infers an order from timestamps.
 */
export function queueLine(request: RequestSummary): string | null {
  const behind =
    request.waitingOn !== null && request.waitingOn !== "" ? `behind ${request.waitingOn}` : "";
  if (request.queuePosition !== null) {
    return ["Queued", `position ${request.queuePosition}`, behind]
      .filter((part) => part !== "")
      .join(" · ");
  }
  return behind === "" ? null : `Queued ${behind}`;
}

const ASKS: Readonly<Record<string, string>> = {
  spec_review: "Review the drafted spec.",
  oracle_review: "Review the drafted acceptance tests.",
  plan_review: "Review the drafted tickets.",
  resume_review: "Decide how to resume the step a lost worker left.",
  pr_review: "Review and merge its pull requests.",
  halted: "Retry, send back or cancel.",
  quarantined: "Retry, send back or cancel.",
};

/**
 * What a Needs-you card asks of the operator, in one line: a fixed sentence
 * per state (the server's `next_action` for a review state names CLI commands
 * for buttons the console has), else the first line of `next_action`.
 */
export function needsYouAsk(request: RequestSummary): string {
  if (requestAwaitingPullRequest(request)) return "Open its pull request: retry, or merge by hand.";
  const fixed = Object.hasOwn(ASKS, request.state) ? ASKS[request.state] : undefined;
  if (fixed !== undefined) return fixed;
  // The server's text can quote agent-written text: shown as text, with
  // nothing invisible in it, like the alert's reason.
  return request.nextAction !== ""
    ? escapeInvisible(digestText(request.nextAction, 120).head)
    : "This request waits on you.";
}

/**
 * The red marker of a halted or quarantined request and the first line of its
 * reason. Null for every other state, and for a halt that is only a missing
 * pull request: that work is accepted, and reads calmly everywhere else too.
 */
export function cardAlert(request: RequestSummary): CardAlert | null {
  if (request.state !== "halted" && request.state !== "quarantined") return null;
  if (requestAwaitingPullRequest(request)) return null;
  const reason =
    request.error !== ""
      ? digestText(request.error, 140).head
      : request.quarantineCheck !== null && request.quarantineCheck !== ""
        ? `check: ${request.quarantineCheck}`
        : "";
  return { label: stateLabel(request.state), reason };
}

function pullRequests(request: RequestSummary): CardPullRequest[] {
  // A PR state with no URL is a record of a PR that was never opened.
  return request.tickets
    .filter((ticket) => ticket.prUrl !== "" && ticket.prState !== "")
    .map((ticket) => ({
      ticket: ticket.index,
      prState: ticket.prState,
      url: safeHttpUrl(ticket.prUrl),
      reviewRounds: ticket.reviewRounds.length,
    }));
}

function buildingLines(request: RequestSummary): string[] {
  const build = request.build;
  if (build === null) {
    return request.ticketCount > 0
      ? [`Ticket ${request.ticketIndex} of ${request.ticketCount}`]
      : [];
  }
  const round =
    build.round <= 0
      ? ""
      : build.maxRounds > 0
        ? `Round ${build.round} of ${build.maxRounds}`
        : `Round ${build.round}`;
  return [
    `Ticket ${build.ticket} of ${build.tickets}`,
    round,
    (Object.hasOwn(stageLabels, build.stage) ? stageLabels[build.stage] : undefined) ?? build.stage,
  ].filter((line) => line !== "");
}

function reviewRoundsLine(prs: readonly CardPullRequest[]): string[] {
  const rounds = prs.reduce((sum, pr) => sum + pr.reviewRounds, 0);
  return rounds === 0 ? [] : [rounds === 1 ? "1 review round" : `${rounds} review rounds`];
}

/** The facts a card shows for `request` in `column`. */
export function cardFacts(request: RequestSummary, column: BoardColumn): CardFacts {
  const none: CardFacts = { lines: [], queue: null, alert: null, stalled: false, pullRequests: [] };
  switch (column) {
    case "drafting": {
      const job = requestRunningJob(request);
      return {
        ...none,
        lines: [stateLabel(request.state), job === null ? "" : activeJobLabel(job)].filter(
          (line) => line !== "",
        ),
        queue: queueLine(request),
      };
    }
    case "needsYou":
      return {
        ...none,
        lines: [needsYouAsk(request)],
        alert: cardAlert(request),
        pullRequests: request.state === "pr_review" ? pullRequests(request) : [],
      };
    case "building":
      return {
        ...none,
        lines: buildingLines(request),
        queue: queueLine(request),
        stalled: request.build?.stalled ?? false,
      };
    case "prReview": {
      const prs = pullRequests(request);
      return { ...none, lines: reviewRoundsLine(prs), pullRequests: prs };
    }
    case "done": {
      const prs = pullRequests(request);
      return {
        ...none,
        lines: [
          ...(request.state === "done" ? [] : [stateLabel(request.state)]),
          ...reviewRoundsLine(prs),
        ],
        pullRequests: prs,
      };
    }
  }
}

/**
 * When the request came to rest where the card shows it: since it started
 * waiting on the operator in Needs you, else since it entered its state.
 */
export function cardSince(request: RequestSummary, column: BoardColumn): string {
  return column === "needsYou" ? requestWaitingSinceOrEnteredAt(request) : request.enteredAt;
}

/** "for 5m", "for 2h 05m", "just now"; null when the record has no usable time. */
export function cardAge(request: RequestSummary, column: BoardColumn, now: Date): string | null {
  const since = Date.parse(cardSince(request, column));
  if (Number.isNaN(since)) return null;
  const ageMs = now.getTime() - since;
  return ageMs < 60_000 ? "just now" : `for ${formatAgeCompact(ageMs)}`;
}
