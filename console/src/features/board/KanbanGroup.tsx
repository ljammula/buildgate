import { type ReactNode, useState } from "react";

import { type BoardGroup, groupCards } from "@/domain/boardColumns";
import type { RequestSummary } from "@/domain/request";
import { Button } from "@/ui/Button";
import { cn } from "@/ui/cn";

export interface KanbanGroupProps {
  /** A group with a label: the unnamed group of an ungrouped column is drawn flat by the lane. */
  readonly group: BoardGroup & { readonly label: string };
  /** "h3" under a column heading, "h4" under a lane's. */
  readonly headingLevel: "h3" | "h4";
  /** A chip narrowed the column to this group: all of it is shown. */
  readonly alone: boolean;
  /** Cards side by side: 2 when the column is wide enough. */
  readonly across: 1 | 2;
  readonly renderCard: (request: RequestSummary, stateInHeading: boolean) => ReactNode;
}

/**
 * One labelled group inside a column's list: a counted sub-heading, the
 * group's first three cards and "+N more" for the rest ("Show fewer" folds
 * them back). The heading's count is always the group's real size, and no
 * card is ever out of reach: the rest is one press away.
 */
export function KanbanGroup({ group, headingLevel, alone, across, renderCard }: KanbanGroupProps) {
  const [expanded, setExpanded] = useState(false);
  const { shown, more } = groupCards(group, { expanded, alone });
  // "Show fewer" only where "+N more" would come back.
  const foldable = groupCards(group, { expanded: false, alone: false }).more > 0;
  const Heading = headingLevel;
  return (
    <li className="shrink-0">
      <div role="group" aria-label={group.label} className="flex flex-col gap-2">
        <Heading className="text-fg-muted px-1 text-xs font-medium">
          {group.label} <span className="font-normal">{`(${group.requests.length})`}</span>
        </Heading>
        <ul
          data-across={across}
          className={cn(
            "flex flex-col gap-2",
            // Two across needs the room: the column's own width (a container
            // query on its list), so each card stays at least about 250px.
            across === 2 && "@min-[510px]:grid @min-[510px]:grid-cols-2 @min-[510px]:items-start",
          )}
        >
          {shown.map((request) => renderCard(request, group.oneState))}
        </ul>
        {more > 0 ? (
          <Button
            size="sm"
            variant="link"
            className="self-start px-1"
            aria-expanded={false}
            onClick={() => {
              setExpanded(true);
            }}
          >
            {`+${more} more`}
          </Button>
        ) : expanded && !alone && foldable ? (
          <Button
            size="sm"
            variant="link"
            className="self-start px-1"
            aria-expanded
            onClick={() => {
              setExpanded(false);
            }}
          >
            Show fewer
          </Button>
        ) : null}
      </div>
    </li>
  );
}
