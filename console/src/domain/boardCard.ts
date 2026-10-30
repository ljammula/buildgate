// What one card of the Mission Control board says. Pure: every fact is read
// from the request list's own record, never fetched per card, and none from
// the column the card is in: a request stuck in Building says what a stuck
// request says.
import { waitsOnYou } from "@/domain/boardColumns";
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
import { FINISHED_STATES } from "@/domain/requestStage";
import { waitLine } from "@/domain/requestWait";
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
  /** One sentence naming why the request waits ("Queued · position 2 · behind req-a"); null while it does not. */
  readonly queue: string | null;
  readonly alert: CardAlert | null;
  /** The server's stalled verdict on the build under way. */
  readonly stalled: boolean;
  /** Opened pull requests, by ticket; empty before any is opened. */
  readonly pullRequests: readonly CardPullRequest[];
}

const ASKS: Readonly<Record<string, string>> = {
  spec_review: "Review the drafted spec.",
  plan_review: "Review the drafted tickets.",
  resume_review: "Decide how to resume the step a lost worker left.",
  pr_review: "Review and merge its pull requests.",
  halted: "Retry, send back or cancel.",
  quarantined: "Retry, send back or cancel.",
};

/**
 * What a card that waits on the operator asks of them, in one line: a fixed sentence
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

/** The facts a card shows for `request`. */
export function cardFacts(request: RequestSummary): CardFacts {
  const none: CardFacts = { lines: [], queue: null, alert: null, stalled: false, pullRequests: [] };
  const prs = pullRequests(request);
  if (waitsOnYou(request)) {
    return {
      ...none,
      lines: [needsYouAsk(request), ...reviewRoundsLine(prs)],
      alert: cardAlert(request),
      pullRequests: prs,
    };
  }
  if (FINISHED_STATES.has(request.state)) {
    return {
      ...none,
      lines: [
        ...(request.state === "done" ? [] : [stateLabel(request.state)]),
        ...reviewRoundsLine(prs),
      ],
      pullRequests: prs,
    };
  }
  switch (request.state) {
    case "building":
      return {
        ...none,
        lines: buildingLines(request),
        queue: waitLine(request.wait),
        stalled: request.build?.stalled ?? false,
      };
    case "pr_review":
      return { ...none, lines: reviewRoundsLine(prs), pullRequests: prs };
    default: {
      // Drafting, planning, and a state this console does not know: its
      // stage first, then the job running for it.
      const job = requestRunningJob(request);
      return {
        ...none,
        lines: [stateLabel(request.state), job === null ? "" : activeJobLabel(job)].filter(
          (line) => line !== "",
        ),
        queue: waitLine(request.wait),
      };
    }
  }
}

/**
 * When the request came to rest where the card shows it: since it started
 * waiting on the operator when it does, else since it entered its state.
 */
export function cardSince(request: RequestSummary): string {
  return waitsOnYou(request) ? requestWaitingSinceOrEnteredAt(request) : request.enteredAt;
}

/** "for 5m", "for 2h 05m", "just now"; null when the record has no usable time. */
export function cardAge(request: RequestSummary, now: Date): string | null {
  const since = Date.parse(cardSince(request));
  if (Number.isNaN(since)) return null;
  const ageMs = now.getTime() - since;
  return ageMs < 60_000 ? "just now" : `for ${formatAgeCompact(ageMs)}`;
}
