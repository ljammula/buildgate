import { statsRefreshMs } from "@/api/polling";
import { useFactoryStats } from "@/api/runQueries";
import { ApiError } from "@/domain/apiError";
import type { BoardWindowDays } from "@/domain/boardFilters";
import { boardWindowLabel, scopeLabel, statsSince } from "@/domain/boardWindow";
import {
  type FactoryStats,
  noTicketsText,
  numbersHaveNoTickets,
  numbersRows,
  statsEmpty,
} from "@/domain/stats";
import { escapeInvisible } from "@/domain/textEscape";
import { cn } from "@/ui/cn";
import { StaleWarning } from "@/ui/StaleWarning";
import {
  Table,
  TableBody,
  TableCell,
  TableFrame,
  TableHead,
  TableHeaderCell,
  TableRow,
} from "@/ui/Table";

function NumbersTable({
  stats,
  projects,
  days,
}: {
  readonly stats: FactoryStats;
  readonly projects: ReadonlySet<string>;
  readonly days: BoardWindowDays;
}) {
  const rows = numbersRows(stats, projects);
  if (statsEmpty(stats) || rows.length === 0) {
    return (
      <p className="text-fg-muted text-sm">
        No ticket has a finished run yet: the numbers appear with the first one.
      </p>
    );
  }
  if (numbersHaveNoTickets(rows)) {
    // A table of dashes says nothing: one line does.
    return <p className="text-fg-muted text-sm">{noTicketsText(days)}</p>;
  }
  return (
    // Bounded, and focusable so the keyboard can scroll it.
    <TableFrame
      role="group"
      aria-label="Numbers table"
      tabIndex={0}
      className="min-h-0 flex-1 overflow-auto focus-visible:outline-2 focus-visible:-outline-offset-2"
    >
      <Table className="min-w-[52rem]">
        <caption className="sr-only">
          Tickets and what they cost, over every project and per project
        </caption>
        <TableHead>
          <TableRow className="hover:bg-transparent">
            <TableHeaderCell scope="col">Project</TableHeaderCell>
            <TableHeaderCell scope="col" numeric>
              Tickets
            </TableHeaderCell>
            <TableHeaderCell scope="col" numeric>
              One-shot
            </TableHeaderCell>
            <TableHeaderCell scope="col" numeric>
              Accepted
            </TableHeaderCell>
            <TableHeaderCell scope="col" numeric>
              Median rounds
            </TableHeaderCell>
            <TableHeaderCell scope="col">Top quarantine check</TableHeaderCell>
            <TableHeaderCell scope="col" numeric>
              Spend
            </TableHeaderCell>
            <TableHeaderCell scope="col" numeric>
              Cost / accepted ticket
            </TableHeaderCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((row, index) => (
            // The overall row and a project could share a name; the position cannot.
            <TableRow key={index} data-testid="numbers-row">
              <TableHeaderCell scope="row" className="text-fg border-b-0 text-sm">
                {row.label}
              </TableHeaderCell>
              <TableCell numeric>{row.tickets}</TableCell>
              <TableCell numeric>{row.oneShot}</TableCell>
              <TableCell numeric>{row.accepted}</TableCell>
              <TableCell numeric>{row.medianRounds}</TableCell>
              {/* A check name is text a run recorded: never markup. */}
              <TableCell className="font-mono text-xs">
                {escapeInvisible(row.topQuarantine)}
              </TableCell>
              <TableCell numeric className="font-mono text-xs">
                {row.spend}
              </TableCell>
              <TableCell numeric className="font-mono text-xs">
                {row.costPerAccepted}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableFrame>
  );
}

export interface NumbersPanelProps {
  /** The window the numbers are over. */
  readonly days: BoardWindowDays;
  /**
   * The toolbar's project filter: its projects' rows only; empty is every
   * project and the overall row. The search box does not apply: the numbers
   * are per project, not per request.
   */
  readonly projects: ReadonlySet<string>;
  readonly className?: string;
}

/**
 * The numbers `factoryd stats` prints, overall and per project, from
 * `GET /stats` over the board's time window, which the heading names. A
 * failed refresh keeps the last numbers under a warning. A server that has no
 * such route (a 404) shows no section at all: an older factoryd is not an
 * error on the home screen.
 */
export function NumbersPanel({ days, projects, className }: NumbersPanelProps) {
  const query = useFactoryStats(statsSince(days), statsRefreshMs);
  const missingRoute = query.error instanceof ApiError && query.error.status === 404;
  if (query.data === undefined && (query.error === null || missingRoute)) return null;
  return (
    <section aria-label="Numbers" className={cn("flex min-h-0 min-w-0 flex-col gap-2", className)}>
      <h2 className="text-fg flex shrink-0 items-baseline gap-2 text-base font-semibold">
        Numbers
        <span data-testid="numbers-window" className="text-fg-muted text-xs font-normal">
          {scopeLabel(boardWindowLabel(days), projects)}
        </span>
      </h2>
      {query.error === null ? null : (
        <StaleWarning
          error={query.error}
          detail="headline"
          retrying={query.isFetching}
          onRetry={() => void query.refetch()}
        >
          {query.data === undefined
            ? "The numbers could not be loaded:"
            : "Showing the last numbers loaded -- refresh failed:"}
        </StaleWarning>
      )}
      {query.data === undefined ? null : (
        <NumbersTable stats={query.data} projects={projects} days={days} />
      )}
    </section>
  );
}
