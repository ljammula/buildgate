// The Mission Control board: five columns by who has to act, a lane per
// project. Pure grouping over the request list; nothing here reads the clock.
import {
  type RequestBoardSection,
  requestStageGroupOf,
  sectionForRequest,
} from "@/domain/boardFilters";
import { type RequestSummary, requestWaitingSinceOrEnteredAt } from "@/domain/request";

/** The board's columns, left to right. */
export type BoardColumn = "drafting" | "needsYou" | "building" | "prReview" | "done";

export const boardColumns: readonly BoardColumn[] = [
  "drafting",
  "needsYou",
  "building",
  "prReview",
  "done",
];

export const boardColumnLabels: Readonly<Record<BoardColumn, string>> = {
  drafting: "Drafting",
  needsYou: "Needs you",
  building: "Building",
  prReview: "PR review",
  done: "Done",
};

/**
 * The column a request is in. `requestStageGroupOf` stays the one decision of
 * "needs you" (a `pr_review` whose pull requests all wait on a human
 * included); the columns only split its `working` group by state.
 *
 * States are a server-owned set that can grow. One this console does not know
 * lands in Drafting: the factory's side of the board, where it stays in view
 * without claiming the operator's attention (Needs you) or that the work is
 * over (Done). The card shows the raw state.
 */
export function columnForRequest(request: RequestSummary): BoardColumn {
  switch (requestStageGroupOf(request)) {
    case "review":
      return "needsYou";
    case "done":
    case "failed":
      return "done";
    case "working":
      if (request.state === "building") return "building";
      if (request.state === "pr_review") return "prReview";
      return "drafting";
    case "other":
      return "drafting";
  }
}

/** A cancelled request: in the Done column, and only when the operator asks to see it. */
export function isCancelledRequest(request: RequestSummary): boolean {
  return requestStageGroupOf(request) === "failed";
}

const SECTION_COLUMNS: Readonly<Record<RequestBoardSection, readonly BoardColumn[]>> = {
  needsYou: ["needsYou"],
  working: ["drafting", "building", "prReview"],
  finished: ["done"],
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
  /** How many more the Done cap left out; 0 in every other column. */
  readonly more: number;
}

/** One project's row of the board. */
export interface BoardLane {
  readonly project: string;
  /** Cards in the lane, those behind the Done cap included. */
  readonly count: number;
  readonly cells: Readonly<Record<BoardColumn, BoardCell>>;
}

export interface Board {
  /** One lane per project with a card, by project name. */
  readonly lanes: readonly BoardLane[];
  /** Cards per column over every lane, those behind the Done cap included. */
  readonly counts: Readonly<Record<BoardColumn, number>>;
  /** Cancelled requests among those given, shown or not. */
  readonly cancelled: number;
}

export interface BoardOptions {
  /** Cancelled requests join the Done column. */
  readonly showCancelled: boolean;
  /** The newest this many of a lane's Done column are drawn. */
  readonly doneCap?: number;
}

/** The Done column of a lane shows its newest 20; the list view has the rest. */
export const defaultDoneCap = 20;

function timeKey(value: string): number {
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? 0 : ms;
}

function newestFirst(a: RequestSummary, b: RequestSummary): number {
  return timeKey(b.updatedAt) - timeKey(a.updatedAt);
}

// A waiting request sorts after the running ones, in the order the worker
// will take them.
function queueOrder(a: RequestSummary, b: RequestSummary): number {
  const left = a.queuePosition ?? 0;
  const right = b.queuePosition ?? 0;
  return left !== right ? left - right : newestFirst(a, b);
}

function ordered(column: BoardColumn, requests: RequestSummary[]): RequestSummary[] {
  switch (column) {
    case "needsYou":
      // Oldest wait first, as every list of what waits on the operator sorts.
      return requests.sort(
        (a, b) =>
          timeKey(requestWaitingSinceOrEnteredAt(a)) - timeKey(requestWaitingSinceOrEnteredAt(b)),
      );
    case "drafting":
    case "building":
      // The running ones (no position), then the queue by position.
      return requests.sort(queueOrder);
    case "prReview":
    case "done":
      return requests.sort(newestFirst);
  }
}

function emptyColumns<T>(make: () => T): Record<BoardColumn, T> {
  return { drafting: make(), needsYou: make(), building: make(), prReview: make(), done: make() };
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
      const cells = emptyColumns<BoardCell>(() => ({ requests: [], more: 0 }));
      let count = 0;
      for (const column of boardColumns) {
        const all = ordered(column, columns[column]);
        count += all.length;
        const shown = column === "done" ? all.slice(0, doneCap) : all;
        cells[column] = { requests: shown, more: all.length - shown.length };
      }
      return { project, count, cells };
    });
  return { lanes, counts, cancelled };
}

/** Cards of one column that belong together, under one sub-heading. */
export interface BoardGroup {
  /** The sub-heading; null for the one unnamed group of a column that has no groups. */
  readonly label: string | null;
  /** The word on the group's summary chip ("Spec" for "Spec review"); null with a null label. */
  readonly short: string | null;
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

// In the order matched. The last rule of a column takes whatever the others
// did not, so a state this console does not know still has a group. The order
// drawn is each rule's `order`: Stuck is drawn first and matched last.
const GROUP_RULES: Readonly<Partial<Record<BoardColumn, readonly GroupRule[]>>> = {
  needsYou: [
    {
      label: "Spec review",
      short: "Spec",
      order: 1,
      oneState: true,
      holds: inState("spec_review"),
    },
    {
      label: "Oracle review",
      short: "Oracle",
      order: 2,
      oneState: true,
      holds: inState("oracle_review"),
    },
    {
      label: "Plan review",
      short: "Plan",
      order: 3,
      oneState: true,
      holds: inState("plan_review"),
    },
    // Only a pr_review whose pull requests all wait on a human is in this column.
    { label: "PR ready", short: "PR ready", order: 4, holds: inState("pr_review") },
    // halted, quarantined, resume_review, and anything else that waits on
    // the operator without being a review: it needs a look, not a reading.
    { label: "Stuck", short: "Stuck", order: 0, holds: () => true },
  ],
  drafting: [
    {
      label: "Spec and oracles",
      short: "Spec",
      order: 0,
      holds: inState("submitted", "spec_drafting", "oracle_drafting"),
    },
    { label: "Planning", short: "Planning", order: 1, oneState: true, holds: inState("planning") },
    // A state this console does not know: in view, under no stage it may not be in.
    { label: "Other", short: "Other", order: 2, holds: () => true },
  ],
};

/**
 * The groups of one column's cards, in the order drawn: Needs you by what is
 * asked (Stuck first, then the reviews and PR ready; Stuck is matched last), Drafting by stage; every other column
 * is one unnamed group. Each request is in exactly one group, a group with no
 * request is left out, and the cards keep the order they were given (a cell
 * of `buildBoard`: the longest wait first in Needs you, the running jobs and
 * then the queue by position in Drafting). It never moves a request between
 * columns and changes no count.
 */
export function columnGroups(
  column: BoardColumn,
  requests: readonly RequestSummary[],
): BoardGroup[] {
  const rules = GROUP_RULES[column];
  if (rules === undefined) {
    return requests.length === 0 ? [] : [{ label: null, short: null, oneState: false, requests }];
  }
  const groups = rules.map((rule) => ({
    label: rule.label,
    short: rule.short,
    oneState: rule.oneState === true,
    order: rule.order,
    requests: [] as RequestSummary[],
  }));
  for (const request of requests) {
    const index = rules.findIndex((rule) => rule.holds(request));
    groups[index]?.requests.push(request);
  }
  return groups
    .filter((group) => group.requests.length > 0)
    .sort((a, b) => a.order - b.order)
    .map((group) => ({
      label: group.label,
      short: group.short,
      oneState: group.oneState,
      requests: group.requests,
    }));
}

/** A column whose cards are drawn under sub-headings, in the compact density. */
export function isGroupedColumn(column: BoardColumn): boolean {
  return GROUP_RULES[column] !== undefined;
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
  return columnGroups(column, requests).flatMap((group) =>
    group.label === null || group.short === null
      ? []
      : [{ label: group.label, short: group.short, count: group.requests.length }],
  );
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
  /** The cards drawn: the first of the group, which are its oldest waits in Needs you. */
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
  if (view.expanded || view.alone || group.label === null) {
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

/**
 * How many cards across a column's groups may lay out: two in Needs you, one
 * everywhere else. This only rules out the boards where two could never fit
 * (three or more columns holding cards); whether they do fit is decided by the
 * column's own width, in the group's container query.
 */
export function cardsAcross(
  column: BoardColumn,
  columns: readonly BoardColumn[],
  counts: Readonly<Record<BoardColumn, number>>,
): 1 | 2 {
  if (column !== "needsYou" || isNarrowColumn(column, counts)) return 1;
  const sharing = columns.filter((other) => !isNarrowColumn(other, counts)).length;
  return sharing * 2 <= boardColumns.length ? 2 : 1;
}

/** Whether the board in view shows a request that waits on the operator. */
export function showsNeedsYou(requests: readonly RequestSummary[]): boolean {
  return requests.some((request) => sectionForRequest(request) === "needsYou");
}
