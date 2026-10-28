// The Mission Control board: five columns in pipeline order, a lane per
// project. A card only moves right as its request moves on, except where the
// request itself goes back (a send-back, or the next ticket building under
// `advance_on: pr_approved`). Who acts is a mark on the card, not a column.
// Pure grouping over the request list; nothing here reads the clock.
import {
  type RequestBoardSection,
  requestStageGroupOf,
  sectionForRequest,
} from "@/domain/boardFilters";
import { type RequestSummary, requestWaitingSinceOrEnteredAt } from "@/domain/request";
import { FINISHED_STATES, STUCK_STATES, pipelineStateOf } from "@/domain/requestStage";

/** The board's columns, left to right. */
export type BoardColumn = "spec" | "plan" | "building" | "prReview" | "finished";

export const boardColumns: readonly BoardColumn[] = [
  "spec",
  "plan",
  "building",
  "prReview",
  "finished",
];

export const boardColumnLabels: Readonly<Record<BoardColumn, string>> = {
  spec: "Spec",
  plan: "Plan",
  building: "Building",
  prReview: "PR review",
  finished: "Finished",
};

const COLUMN_OF_STATE: Readonly<Record<string, BoardColumn>> = {
  submitted: "spec",
  spec_drafting: "spec",
  spec_review: "spec",
  planning: "plan",
  plan_review: "plan",
  building: "building",
  pr_review: "prReview",
};

/**
 * Whether the request waits on the operator: a review, a pull request that
 * waits only on a human, or a request stuck until they act. The one decision
 * of it is `requestStageGroupOf`, which the tab title, notifications and
 * Triage read too.
 */
export function waitsOnYou(request: RequestSummary): boolean {
  return requestStageGroupOf(request) === "review";
}

/**
 * The column a request is in: Finished for done, closed and cancelled, else
 * the column of its place on the pipeline (`pipelineStateOf`), so a stuck
 * request stays in the column of the step it stopped at.
 *
 * States are a server-owned set that can grow. One this console does not know
 * lands under Other, in the column of the step its history says it came
 * from, or in Spec with no such history: in view, without claiming the work
 * is over. The card shows the raw state.
 */
export function columnForRequest(request: RequestSummary): BoardColumn {
  if (FINISHED_STATES.has(request.state)) return "finished";
  const state = pipelineStateOf(request);
  return Object.hasOwn(COLUMN_OF_STATE, state) ? (COLUMN_OF_STATE[state] ?? "spec") : "spec";
}

/** A cancelled request: in the Finished column, and only when the operator asks to see it. */
export function isCancelledRequest(request: RequestSummary): boolean {
  return requestStageGroupOf(request) === "failed";
}

const SECTION_COLUMNS: Readonly<Record<RequestBoardSection, readonly BoardColumn[]>> = {
  needsYou: ["spec", "plan", "building", "prReview"],
  working: ["spec", "plan", "building", "prReview"],
  finished: ["finished"],
};

/**
 * The columns the board draws under the URL's section filter (`?group=`):
 * every column with none, else the columns of that section. A column outside
 * the section that still holds a card (a request in an unknown state, which
 * the filter counts as finished) is drawn too, so no card that passed the
 * filter is ever without a column.
 */
export function visibleColumns(
  section: RequestBoardSection | null,
  counts: Readonly<Record<BoardColumn, number>>,
): BoardColumn[] {
  if (section === null) return [...boardColumns];
  const own = SECTION_COLUMNS[section];
  return boardColumns.filter((column) => own.includes(column) || counts[column] > 0);
}

/** One column of one lane. */
export interface BoardCell {
  /** The cards drawn, in order. */
  readonly requests: readonly RequestSummary[];
  /** Every card of the cell, those behind the Finished cap included: what a group chip counts. */
  readonly all: readonly RequestSummary[];
  /** How many more the Finished cap left out; 0 in every other column. */
  readonly more: number;
}

/** One project's row of the board. */
export interface BoardLane {
  readonly project: string;
  /** Cards in the lane, those behind the Finished cap included. */
  readonly count: number;
  /** Cards in the lane that wait on the operator: what a folded lane still says. */
  readonly waiting: number;
  readonly cells: Readonly<Record<BoardColumn, BoardCell>>;
}

export interface Board {
  /** One lane per project with a card, by project name. */
  readonly lanes: readonly BoardLane[];
  /** Cards per column over every lane, those behind the Finished cap included. */
  readonly counts: Readonly<Record<BoardColumn, number>>;
  /** Cancelled requests among those given, shown or not. */
  readonly cancelled: number;
}

export interface BoardOptions {
  /** Cancelled requests join the Finished column. */
  readonly showCancelled: boolean;
  /** The newest this many of a lane's Finished column are drawn. */
  readonly doneCap?: number;
}

/** The Finished column of a lane shows its newest 20; the list view has the rest. */
export const defaultDoneCap = 20;

function timeKey(value: string): number {
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? 0 : ms;
}

function newestFirst(a: RequestSummary, b: RequestSummary): number {
  return timeKey(b.updatedAt) - timeKey(a.updatedAt);
}

// The states a job moves on: their cards are in the worker's order.
const JOB_STATES: ReadonlySet<string> = new Set([
  "submitted",
  "spec_drafting",
  "planning",
  "building",
]);

// What orders a card among the others of its kind: 0 waits on the operator
// (the oldest wait first, as every list of what waits on the operator
// sorts), 1 a job (the running ones, with no position, then the queue by
// position), 2 everything else (the newest first). Kinds never share a
// group, so the rank only keeps the sort one total order.
function kindOf(request: RequestSummary): 0 | 1 | 2 {
  if (waitsOnYou(request)) return 0;
  return JOB_STATES.has(request.state) ? 1 : 2;
}

function cardOrder(a: RequestSummary, b: RequestSummary): number {
  const kind = kindOf(a) - kindOf(b);
  if (kind !== 0) return kind;
  switch (kindOf(a)) {
    case 0:
      return (
        timeKey(requestWaitingSinceOrEnteredAt(a)) - timeKey(requestWaitingSinceOrEnteredAt(b))
      );
    case 1: {
      const left = a.wait?.position ?? 0;
      const right = b.wait?.position ?? 0;
      return left !== right ? left - right : newestFirst(a, b);
    }
    case 2:
      return newestFirst(a, b);
  }
}

function emptyColumns<T>(make: () => T): Record<BoardColumn, T> {
  return { spec: make(), plan: make(), building: make(), prReview: make(), finished: make() };
}

/**
 * The board over `requests` (already filtered by the caller): each request in
 * exactly one cell, a lane per project. Cancelled requests are counted but
 * left out unless asked for.
 */
export function buildBoard(requests: readonly RequestSummary[], options: BoardOptions): Board {
  const doneCap = options.doneCap ?? defaultDoneCap;
  const counts = emptyColumns(() => 0);
  const byProject = new Map<string, Record<BoardColumn, RequestSummary[]>>();
  let cancelled = 0;
  for (const request of requests) {
    if (isCancelledRequest(request)) {
      cancelled += 1;
      if (!options.showCancelled) continue;
    }
    const column = columnForRequest(request);
    let lane = byProject.get(request.project);
    if (lane === undefined) {
      lane = emptyColumns<RequestSummary[]>(() => []);
      byProject.set(request.project, lane);
    }
    lane[column].push(request);
    counts[column] += 1;
  }
  const lanes = [...byProject.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([project, columns]): BoardLane => {
      const cells = emptyColumns<BoardCell>(() => ({ requests: [], all: [], more: 0 }));
      let count = 0;
      let waiting = 0;
      for (const column of boardColumns) {
        // Newest first in Finished, so its cap keeps the latest.
        const all = columns[column].sort(column === "finished" ? newestFirst : cardOrder);
        count += all.length;
        waiting += all.filter(waitsOnYou).length;
        const shown = column === "finished" ? all.slice(0, doneCap) : all;
        cells[column] = { requests: shown, all, more: all.length - shown.length };
      }
      return { project, count, waiting, cells };
    });
  return { lanes, counts, cancelled };
}

/** Cards of one column that belong together, under one sub-heading. */
export interface BoardGroup {
  /** The sub-heading. */
  readonly label: string;
  /** The word on the group's summary chip ("Spec" for "Spec review"). */
  readonly short: string;
  /** The group holds exactly one state and its heading says it: a card in it draws no stage chip. */
  readonly oneState: boolean;
  readonly requests: readonly RequestSummary[];
}

interface GroupRule {
  readonly label: string;
  readonly short: string;
  /** Where the group is drawn in its column, 0 first. Independent of the rule's place in the match list. */
  readonly order: number;
  /** The heading names the one state the rule holds. */
  readonly oneState?: boolean;
  readonly holds: (request: RequestSummary) => boolean;
}

const inState =
  (...states: string[]) =>
  (request: RequestSummary): boolean =>
    states.includes(request.state);

// A request stopped until the operator acts, in the column of the step it
// stopped at.
const stuck: GroupRule = {
  label: "Stuck",
  short: "Stuck",
  order: 0,
  holds: (request) => STUCK_STATES.has(request.state),
};

// A state this console does not know: in view, under no stage it may not be
// in. Last in every column, so every card has a group.
const other: GroupRule = { label: "Other", short: "Other", order: 9, holds: () => true };

// In the order matched; the order drawn is each rule's `order`. Within a
// column what waits on the operator is drawn above the factory's work.
const GROUP_RULES: Readonly<Record<BoardColumn, readonly GroupRule[]>> = {
  spec: [
    stuck,
    {
      label: "Spec review",
      short: "Spec",
      order: 1,
      oneState: true,
      holds: inState("spec_review"),
    },
    {
      label: "Drafting",
      short: "Drafting",
      order: 2,
      holds: inState("submitted", "spec_drafting"),
    },
    other,
  ],
  plan: [
    stuck,
    {
      label: "Plan review",
      short: "Plan",
      order: 1,
      oneState: true,
      holds: inState("plan_review"),
    },
    { label: "Planning", short: "Planning", order: 2, oneState: true, holds: inState("planning") },
    other,
  ],
  building: [
    stuck,
    { label: "Building", short: "Building", order: 1, oneState: true, holds: inState("building") },
    other,
  ],
  prReview: [
    stuck,
    // Every pull request waits on a human: to review, or to merge.
    {
      label: "PR ready",
      short: "Ready",
      order: 1,
      holds: (request) => request.state === "pr_review" && waitsOnYou(request),
    },
    // The factory still has work on a pull request: checks, a corrective round.
    { label: "Factory working", short: "Working", order: 2, holds: inState("pr_review") },
    other,
  ],
  finished: [
    { label: "Merged", short: "Merged", order: 0, oneState: true, holds: inState("done") },
    { label: "Closed", short: "Closed", order: 1, oneState: true, holds: inState("closed") },
    {
      label: "Cancelled",
      short: "Cancelled",
      order: 2,
      oneState: true,
      holds: inState("cancelled"),
    },
    other,
  ],
};

/**
 * The groups of one column's cards, in the order drawn: what waits on the
 * operator (Stuck, then the reviews or PR ready) above the factory's work,
 * and Finished by how the request ended. Each request is in exactly one
 * group (every column ends in a group that holds anything), a group with no
 * request is left out, and the cards keep the order they were given (a cell
 * of `buildBoard`). It never moves a request between columns and changes no
 * count.
 */
export function columnGroups(
  column: BoardColumn,
  requests: readonly RequestSummary[],
): BoardGroup[] {
  const rules = GROUP_RULES[column];
  const groups = rules.map((rule) => ({ rule, requests: [] as RequestSummary[] }));
  for (const request of requests) {
    const index = rules.findIndex((rule) => rule.holds(request));
    groups[index]?.requests.push(request);
  }
  return groups
    .filter((group) => group.requests.length > 0)
    .sort((a, b) => a.rule.order - b.rule.order)
    .map(({ rule, requests: held }) => ({
      label: rule.label,
      short: rule.short,
      oneState: rule.oneState === true,
      requests: held,
    }));
}

/**
 * A column whose cards are drawn compact (the title, the id, the stage and
 * the age; the rest on hover or focus): Spec and Plan, where cards are many
 * and short-lived. The others draw cards in full: a build's progress and a
 * pull request's links are what their cards are for.
 */
export function isCompactColumn(column: BoardColumn): boolean {
  return column === "spec" || column === "plan";
}

/** One summary chip at the top of a grouped column: "Spec 5". */
export interface GroupChip {
  /** The group it stands for: the group's label. */
  readonly label: string;
  readonly short: string;
  readonly count: number;
}

/**
 * The chips of a column: one per group that holds a card, in the groups'
 * order, each with the group's real size. `requests` is every card of the
 * column, over every lane.
 */
export function groupChips(column: BoardColumn, requests: readonly RequestSummary[]): GroupChip[] {
  return columnGroups(column, requests).map((group) => ({
    label: group.label,
    short: group.short,
    count: group.requests.length,
  }));
}

/**
 * The chip in force. A chip narrows the column to one group; once that group
 * has no card left anywhere (its last request moved on) the choice lapses and
 * every group is shown again, so a pressed chip can never leave the column
 * blank over cards that wait.
 */
export function activeGroup(only: string | null, chips: readonly GroupChip[]): string | null {
  return only !== null && chips.some((chip) => chip.label === only) ? only : null;
}

/** How many cards a group shows before "+N more". */
export const groupPreviewSize = 3;

export interface GroupCards {
  /** The cards drawn: the first of the group, which are its oldest waits where it waits on the operator. */
  readonly shown: readonly RequestSummary[];
  /** How many more "+N more" opens; 0 when the whole group is drawn. */
  readonly more: number;
}

/**
 * The cards of one group in view. A group shows its first three, the rest one
 * press away ("+N more"); expanded, or alone in its column because a chip
 * chose it, it shows them all. Nothing is ever dropped: `shown` and `more`
 * always add up to the group.
 */
export function groupCards(
  group: BoardGroup,
  view: { readonly expanded: boolean; readonly alone: boolean },
): GroupCards {
  if (view.expanded || view.alone) {
    return { shown: group.requests, more: 0 };
  }
  const shown = group.requests.slice(0, groupPreviewSize);
  return { shown, more: group.requests.length - shown.length };
}

/**
 * A column with no card in view, in any lane: drawn as a narrow strip with
 * its heading and "(0)", so the columns that hold cards share its width.
 */
export function isNarrowColumn(
  column: BoardColumn,
  counts: Readonly<Record<BoardColumn, number>>,
): boolean {
  return counts[column] === 0;
}

/** Whether the board in view shows a request that waits on the operator. */
export function showsNeedsYou(requests: readonly RequestSummary[]): boolean {
  return requests.some((request) => sectionForRequest(request) === "needsYou");
}
