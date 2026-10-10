import { ChevronRight } from "lucide-react";
import { useId } from "react";

import {
  type BoardColumn,
  type BoardLane,
  boardColumnLabels,
  cardsAcross,
  columnGroups,
  isGroupedColumn,
  isNarrowColumn,
} from "@/domain/boardColumns";
import type { RequestSummary } from "@/domain/request";
import { Button } from "@/ui/Button";
import { cn } from "@/ui/cn";

import { KanbanCard } from "./KanbanCard";
import { KanbanGroup } from "./KanbanGroup";

/** The width every column with cards keeps, header and cell alike, so lanes line up and a narrow screen scrolls sideways. */
export const columnClass = "w-0 min-w-56 flex-1";

/** Needs you takes two shares of the width to one for each other column: it is where the work is. */
export const needsYouColumnClass = "w-0 min-w-56 flex-[2]";

/** A column with no card in view: a strip, so the others share its width. */
export const narrowColumnClass = "w-36 shrink-0";

/** The width class of a column, for its header and for each lane's cell. */
export function columnWidthClass(column: BoardColumn, narrow: boolean): string {
  if (narrow) return narrowColumnClass;
  return column === "needsYou" ? needsYouColumnClass : columnClass;
}

export interface KanbanLaneProps {
  readonly lane: BoardLane;
  readonly columns: readonly BoardColumn[];
  /** Cards per column over every lane: a column is narrow only when empty in all of them. */
  readonly counts: Readonly<Record<BoardColumn, number>>;
  /** The Needs-you group a summary chip chose; null shows every group. */
  readonly onlyGroup: string | null;
  /**
   * Several lanes are drawn: this one gets its project heading and collapse
   * button, and the lanes scroll together. Alone, the lane fills the board
   * and each of its columns scrolls on its own.
   */
  readonly headed: boolean;
  readonly collapsed: boolean;
  readonly onToggle: () => void;
  readonly showProject: boolean;
  readonly canWrite: boolean;
  readonly rejectingId: string | null;
  readonly onReject: (id: string) => void;
  /** Requests whose card is highlighted for a moment: their state just changed. */
  readonly changedIds: ReadonlySet<string>;
  /** Whether a request's card shows the worker running it now. */
  readonly isRunning: (id: string) => boolean;
  readonly now: Date;
  /** Opens the list of everything finished: the Done cap's way out. */
  readonly onShowAllDone: () => void;
}

/**
 * One project's row of cards: a list per column, the cards of Needs you and
 * Drafting under their sub-headings (`columnGroups`). With several projects
 * it is a named region with a button that folds it; with one it is the bare
 * row.
 */
export function KanbanLane({
  lane,
  columns,
  counts,
  onlyGroup,
  headed,
  collapsed,
  onToggle,
  showProject,
  canWrite,
  rejectingId,
  onReject,
  changedIds,
  isRunning,
  now,
  onShowAllDone,
}: KanbanLaneProps) {
  const cellsId = useId();
  const waiting = lane.cells.needsYou.requests.length;
  const card = (request: RequestSummary, column: BoardColumn, stateInHeading = false) => (
    <KanbanCard
      key={request.id}
      request={request}
      column={column}
      compact={isGroupedColumn(column)}
      stateInHeading={stateInHeading}
      now={now}
      showProject={showProject}
      canWrite={canWrite}
      rejecting={rejectingId === request.id}
      onReject={onReject}
      changed={changedIds.has(request.id)}
      running={isRunning(request.id)}
    />
  );
  const cells = (
    <div
      id={cellsId}
      hidden={headed && collapsed}
      className={cn("flex gap-3", !headed && "min-h-0 flex-1")}
    >
      {columns.map((column) => {
        const cell = lane.cells[column];
        const narrow = isNarrowColumn(column, counts);
        const across = cardsAcross(column, columns, counts);
        return (
          <ul
            key={column}
            aria-label={boardColumnLabels[column]}
            data-narrow={narrow || undefined}
            // A lone lane's column is its own scroll box: focusable, so the
            // keyboard can scroll it.
            {...(headed ? {} : { tabIndex: 0 })}
            className={cn(
              columnWidthClass(column, narrow),
              // Every column is a panel, so an empty one is still a place; only
              // one that holds cards is a well.
              "border-border flex flex-col gap-2 border",
              // A lone lane's panel hangs from its heading's band.
              headed ? "rounded-md" : "rounded-b-md border-t-0",
              narrow ? "p-2" : "bg-surface-sunken min-h-14 px-2 py-1.5",
              // The list is the container its groups' two-across query measures.
              across === 2 && "@container",
              !headed && "overflow-y-auto focus-visible:outline-2 focus-visible:-outline-offset-2",
            )}
          >
            {columnGroups(column, cell.requests).map((group) => {
              const { label } = group;
              if (label === null) return group.requests.map((request) => card(request, column));
              // A chip chose one group of Needs you: the others are out of
              // view until it is pressed again, and that one is shown whole.
              const chosen = column === "needsYou" ? onlyGroup : null;
              if (chosen !== null && chosen !== label) return null;
              return (
                <KanbanGroup
                  key={label}
                  group={{ ...group, label }}
                  // One level below the column's heading, or the lane's when there is one.
                  headingLevel={headed ? "h4" : "h3"}
                  alone={chosen === label}
                  across={across}
                  renderCard={(request, stateInHeading) => card(request, column, stateInHeading)}
                />
              );
            })}
            {cell.more > 0 ? (
              <li className="text-fg-muted flex shrink-0 items-center justify-between gap-2 px-1 text-xs">
                <span>{`${cell.more} more not shown`}</span>
                <Button size="sm" variant="link" onClick={onShowAllDone}>
                  Show all
                </Button>
              </li>
            ) : null}
          </ul>
        );
      })}
    </div>
  );
  if (!headed) return cells;
  return (
    <section aria-label={`Project ${lane.project}`} className="flex shrink-0 flex-col gap-1">
      {/* Stays at the top of the lanes' scroll box while its cards pass under it. */}
      <h3 className="bg-bg sticky top-0 z-20 flex items-center gap-2 text-sm">
        <button
          type="button"
          aria-expanded={!collapsed}
          aria-controls={cellsId}
          onClick={onToggle}
          className="text-fg hover:bg-surface-hover flex items-center gap-1.5 rounded-md px-1.5 py-0.5 font-semibold transition-colors"
        >
          <ChevronRight
            aria-hidden
            className={cn(
              "text-fg-subtle size-4 transition-transform motion-reduce:transition-none",
              !collapsed && "rotate-90",
            )}
          />
          {lane.project} <span className="text-fg-muted font-normal">{`(${lane.count})`}</span>
        </button>
        {/* A folded lane must not hide that something in it waits on the operator. */}
        {collapsed && waiting > 0 ? (
          <span
            data-testid="lane-needs-you"
            className="text-tone-warning border-tone-warning-border bg-tone-warning-soft rounded-full border px-2 py-0.5 text-xs font-medium"
          >
            {waiting === 1 ? "1 needs you" : `${waiting} need you`}
          </span>
        ) : null}
      </h3>
      {cells}
    </section>
  );
}
