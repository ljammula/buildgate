import { ChevronRight } from "lucide-react";
import { useId } from "react";

import { type BoardColumn, type BoardLane, boardColumnLabels } from "@/domain/boardColumns";
import { Button } from "@/ui/Button";
import { cn } from "@/ui/cn";

import { KanbanCard } from "./KanbanCard";

/** The width every column keeps, header and cell alike, so lanes line up and a narrow screen scrolls sideways. */
export const columnClass = "w-0 min-w-56 flex-1";

export interface KanbanLaneProps {
  readonly lane: BoardLane;
  readonly columns: readonly BoardColumn[];
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
  readonly now: Date;
  /** Opens the list of everything finished: the Done cap's way out. */
  readonly onShowAllDone: () => void;
}

/**
 * One project's row of cards: a list per column. With several projects it is
 * a named region with a button that folds it; with one it is the bare row.
 */
export function KanbanLane({
  lane,
  columns,
  headed,
  collapsed,
  onToggle,
  showProject,
  canWrite,
  now,
  onShowAllDone,
}: KanbanLaneProps) {
  const cellsId = useId();
  const cells = (
    <div
      id={cellsId}
      hidden={headed && collapsed}
      className={cn("flex gap-3", !headed && "min-h-0 flex-1")}
    >
      {columns.map((column) => {
        const cell = lane.cells[column];
        return (
          <ul
            key={column}
            aria-label={boardColumnLabels[column]}
            // A lone lane's column is its own scroll box: focusable, so the
            // keyboard can scroll it.
            {...(headed ? {} : { tabIndex: 0 })}
            className={cn(
              columnClass,
              "bg-surface-sunken flex min-h-14 flex-col gap-2 rounded-md p-2",
              !headed && "overflow-y-auto focus-visible:outline-2 focus-visible:-outline-offset-2",
            )}
          >
            {cell.requests.map((request) => (
              <KanbanCard
                key={request.id}
                request={request}
                column={column}
                now={now}
                showProject={showProject}
                canWrite={canWrite}
              />
            ))}
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
    <section aria-label={`Project ${lane.project}`} className="flex shrink-0 flex-col gap-1.5">
      {/* Stays at the top of the lanes' scroll box while its cards pass under it. */}
      <h3 className="bg-bg sticky top-0 z-20 text-sm">
        <button
          type="button"
          aria-expanded={!collapsed}
          aria-controls={cellsId}
          onClick={onToggle}
          className="text-fg hover:bg-surface-hover flex items-center gap-1.5 rounded-md px-1.5 py-1 font-semibold transition-colors"
        >
          <ChevronRight
            aria-hidden
            className={cn("text-fg-subtle size-4 transition-transform", !collapsed && "rotate-90")}
          />
          {lane.project} <span className="text-fg-muted font-normal">{`(${lane.count})`}</span>
        </button>
      </h3>
      {cells}
    </section>
  );
}
