import { useState } from "react";
import { Link } from "react-router";

import {
  type Board,
  type BoardColumn,
  activeGroup,
  boardColumnLabels,
  groupChips,
  isNarrowColumn,
} from "@/domain/boardColumns";
import { triagePath } from "@/routes/paths";
import { cn } from "@/ui/cn";

import { FilterChip } from "./FilterChip";
import { KanbanLane, columnWidthClass } from "./KanbanLane";
import { OlderHiddenNote } from "./OlderHiddenNote";
import type { CollapsedLanes } from "./useCollapsedLanes";

export interface KanbanBoardProps {
  readonly board: Board;
  /** The columns drawn, in order: all five, or those of the URL's section filter. */
  readonly columns: readonly BoardColumn[];
  /** Several projects are on the board but only one lane is drawn: a card names its project. */
  readonly showProject: boolean;
  readonly collapsedLanes: CollapsedLanes;
  readonly showCancelled: boolean;
  readonly onShowCancelled: (show: boolean) => void;
  /** Finished requests the time window left out of Done. */
  readonly olderHidden: number;
  readonly onShowAllTime: () => void;
  readonly canWrite: boolean;
  readonly now: Date;
  readonly onShowAllDone: () => void;
  readonly className?: string;
}

/**
 * The board, in the height its parent gives it: a heading with its count per
 * column, fixed, and the cards scrolling under them. With one project each
 * column scrolls on its own. With several, the lanes are the scroll box (one
 * vertical scroll for all columns, each lane's heading sticking to its top),
 * because a lane is a row across every column and cannot scroll per column.
 * The whole board scrolls sideways as one piece on a narrow screen, so no
 * column is ever hidden.
 */
export function KanbanBoard({
  board,
  columns,
  showProject,
  collapsedLanes,
  showCancelled,
  onShowCancelled,
  olderHidden,
  onShowAllTime,
  canWrite,
  now,
  onShowAllDone,
  className,
}: KanbanBoardProps) {
  const laned = board.lanes.length > 1;
  // The summary chips of Needs you, over every lane. The one pressed is view
  // state for this visit: not stored, not in the URL.
  const chips = groupChips(
    "needsYou",
    board.lanes.flatMap((lane) => lane.cells.needsYou.requests),
  );
  const [pressedGroup, setPressedGroup] = useState<string | null>(null);
  const onlyGroup = activeGroup(pressedGroup, chips);
  const lanes = board.lanes.map((lane) => (
    <KanbanLane
      key={lane.project}
      lane={lane}
      columns={columns}
      counts={board.counts}
      onlyGroup={onlyGroup}
      headed={laned}
      collapsed={collapsedLanes.isCollapsed(lane.project)}
      onToggle={() => {
        collapsedLanes.toggle(lane.project);
      }}
      showProject={showProject}
      canWrite={canWrite}
      now={now}
      onShowAllDone={onShowAllDone}
    />
  ));
  return (
    <section aria-label="Board" className={cn("flex min-h-0 flex-col gap-2", className)}>
      {board.cancelled > 0 ? (
        <div className="flex shrink-0 justify-end">
          <FilterChip
            pressed={showCancelled}
            onPressedChange={() => {
              onShowCancelled(!showCancelled);
            }}
          >
            {`Show cancelled (${board.cancelled})`}
          </FilterChip>
        </div>
      ) : null}
      {/* Focusable, so the keyboard can scroll it sideways too. */}
      <div
        role="group"
        aria-label="Board columns"
        tabIndex={0}
        className="min-h-0 flex-1 overflow-x-auto overflow-y-hidden focus-visible:outline-2 focus-visible:-outline-offset-2"
      >
        <div className="flex h-full min-w-fit flex-col gap-2">
          <div className="flex shrink-0 gap-3">
            {columns.map((column) => (
              <div
                key={column}
                data-testid={`column-${column}`}
                data-narrow={isNarrowColumn(column, board.counts) || undefined}
                className={cn(
                  columnWidthClass(isNarrowColumn(column, board.counts)),
                  "flex flex-col gap-1 px-2",
                )}
              >
                <div className="flex flex-wrap items-baseline justify-between gap-x-2">
                  <h2 className="text-fg text-sm font-semibold">
                    {boardColumnLabels[column]}{" "}
                    <span className="text-fg-muted font-normal">{`(${board.counts[column]})`}</span>
                  </h2>
                  {/* Triage is the screen for deciding one after another. */}
                  {column === "needsYou" && board.counts.needsYou > 0 ? (
                    <Link to={triagePath()} className="text-accent text-xs hover:underline">
                      Open in Triage
                    </Link>
                  ) : null}
                </div>
                {column === "needsYou" && chips.length > 1 ? (
                  <div
                    role="group"
                    aria-label="Needs you groups"
                    className="flex flex-wrap items-center gap-1"
                  >
                    {/* Pressing the chip in force, or All, shows every group again. */}
                    <FilterChip
                      pressed={onlyGroup === null}
                      onPressedChange={() => {
                        setPressedGroup(null);
                      }}
                    >
                      All
                    </FilterChip>
                    {chips.map((chip) => (
                      <FilterChip
                        key={chip.label}
                        pressed={onlyGroup === chip.label}
                        onPressedChange={() => {
                          setPressedGroup(onlyGroup === chip.label ? null : chip.label);
                        }}
                      >
                        {`${chip.short} ${chip.count}`}
                      </FilterChip>
                    ))}
                  </div>
                ) : null}
                {column === "done" ? (
                  <OlderHiddenNote count={olderHidden} onShowAllTime={onShowAllTime} />
                ) : null}
              </div>
            ))}
          </div>
          {laned ? (
            <div
              role="group"
              aria-label="Project lanes"
              tabIndex={0}
              className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto focus-visible:outline-2 focus-visible:-outline-offset-2"
            >
              {lanes}
            </div>
          ) : board.lanes.length === 0 ? (
            // Every card is behind the window or the cancelled toggle: the
            // columns stay, empty, under their headers.
            <div className="flex min-h-0 flex-1 gap-3">
              {columns.map((column) => (
                <ul
                  key={column}
                  aria-label={boardColumnLabels[column]}
                  className={cn(
                    columnWidthClass(isNarrowColumn(column, board.counts)),
                    "bg-surface-sunken min-h-14 rounded-md p-2",
                  )}
                />
              ))}
            </div>
          ) : (
            lanes
          )}
        </div>
      </div>
    </section>
  );
}
