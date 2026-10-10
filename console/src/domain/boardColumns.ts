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

/** Whether the board in view shows a request that waits on the operator. */
export function showsNeedsYou(requests: readonly RequestSummary[]): boolean {
  return requests.some((request) => sectionForRequest(request) === "needsYou");
}
