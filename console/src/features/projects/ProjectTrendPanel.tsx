import { useProjectTrend } from "@/api/runQueries";
import {
  type ProjectTrend,
  type TrendCount,
  bucketLabel,
  medianText,
  shareText,
} from "@/domain/trend";
import { escapeInvisible } from "@/domain/textEscape";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { Section } from "@/ui/PageLayout";
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

function Tile({
  label,
  value,
  detail,
}: {
  readonly label: string;
  readonly value: string;
  readonly detail: string;
}) {
  return (
    <div
      data-testid="trend-tile"
      className="flex flex-col gap-0.5 rounded-lg border border-border bg-surface px-4 py-3"
    >
      <span className="text-xs text-fg-muted">{label}</span>
      <span className="text-xl font-semibold text-fg">{value}</span>
      <span className="text-xs text-fg-muted">{detail}</span>
    </div>
  );
}

function percent(rate: number | null): string {
  return rate === null ? "-" : `${Math.round(rate * 100)}%`;
}

function Tiles({ trend }: { readonly trend: ProjectTrend }) {
  const m = trend.overall;
  const green = m.roundsToGreen;
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
      <Tile
        label="One-shot"
        value={percent(m.oneShotRate)}
        detail={`${shareText(m.oneShot, m.tickets)} tickets built right the first time`}
      />
      <Tile
        label="Accepted"
        value={percent(m.acceptedRate)}
        detail={`${shareText(m.accepted, m.tickets)} tickets ended accepted`}
      />
      <Tile
        label="Rounds to green"
        value={medianText(green)}
        detail={
          green.series === 0 ? "no accepted ticket yet" : `median; 90th percentile ${green.p90}`
        }
      />
      <Tile
        label="Same failure twice"
        value={m.comparablePairs === 0 ? "-" : percent(m.sameFailurePairs / m.comparablePairs)}
        detail={`${shareText(m.sameFailurePairs, m.comparablePairs)} failed-round pairs`}
      />
    </div>
  );
}

function BucketTable({ trend }: { readonly trend: ProjectTrend }) {
  return (
    <TableFrame>
      <Table>
        <caption className="sr-only">
          One row per {trend.bucketDays} days, by the day the ticket's first run began
        </caption>
        <TableHead>
          <TableRow>
            <TableHeaderCell scope="col">
              {trend.bucketDays === 7 ? "Week of" : `${trend.bucketDays} days from`}
            </TableHeaderCell>
            <TableHeaderCell scope="col">Tickets</TableHeaderCell>
            <TableHeaderCell scope="col">One-shot</TableHeaderCell>
            <TableHeaderCell scope="col">Accepted</TableHeaderCell>
            <TableHeaderCell scope="col">Median rounds</TableHeaderCell>
            <TableHeaderCell scope="col">Same failure</TableHeaderCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {trend.buckets.map((bucket) => {
            const m = bucket.metrics;
            return (
              <TableRow key={bucket.start} data-testid="trend-bucket">
                <TableCell className="font-mono">{bucketLabel(bucket)}</TableCell>
                <TableCell>{m.tickets}</TableCell>
                <TableCell>{shareText(m.oneShot, m.tickets)}</TableCell>
                <TableCell>{shareText(m.accepted, m.tickets)}</TableCell>
                <TableCell>{medianText(m.roundsToGreen)}</TableCell>
                <TableCell>{shareText(m.sameFailurePairs, m.comparablePairs)}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </TableFrame>
  );
}

/** A top-10 list. The names are check names and halt reasons a run recorded: text, never markup. */
function CountList({
  title,
  testId,
  counts,
}: {
  readonly title: string;
  readonly testId: string;
  readonly counts: readonly TrendCount[];
}) {
  return (
    <Section title={title}>
      {counts.length === 0 ? (
        <p className="text-sm text-fg-muted">None in this window.</p>
      ) : (
        <ul className="flex flex-col font-mono text-sm text-fg">
          {counts.map((c) => (
            <li
              key={c.name}
              data-testid={testId}
              className="flex justify-between gap-3 border-b border-border py-1 last:border-b-0"
            >
              <span>{escapeInvisible(c.name)}</span>
              <span>{c.runs}</span>
            </li>
          ))}
        </ul>
      )}
    </Section>
  );
}

function notes(trend: ProjectTrend): string[] {
  const out: string[] = [];
  if (trend.excludedRuns > 0) {
    out.push(`${trend.excludedRuns} live-smoke run(s) are not counted.`);
  }
  if (trend.unfinished > 0)
    out.push(`${trend.unfinished} run(s) still in progress are not counted.`);
  return out;
}

function TrendBody({ trend }: { readonly trend: ProjectTrend }) {
  if (trend.overall.tickets === 0) {
    return (
      <EmptyState title="No ticket yet">
        The numbers appear once a ticket in this project has a finished run.
      </EmptyState>
    );
  }
  return (
    <>
      <Tiles trend={trend} />
      <Section title="Over time">
        <BucketTable trend={trend} />
      </Section>
      <div className="grid gap-4 md:grid-cols-2">
        <CountList
          title="Quarantined by"
          testId="trend-quarantined"
          counts={trend.overall.quarantinedBy}
        />
        <CountList title="Halted by" testId="trend-halted" counts={trend.overall.haltedBy} />
      </div>
      {notes(trend).map((note) => (
        <p key={note} className="text-xs text-fg-muted">
          {note}
        </p>
      ))}
    </>
  );
}

/**
 * Whether the factory is getting better on one repository: four headline
 * numbers, one row per week and the checks and halt reasons that stopped
 * runs. Read only and computed from the run records on each read. A failed
 * refresh keeps the last data under a warning.
 */
export function ProjectTrendPanel({ project }: { readonly project: string }) {
  const query = useProjectTrend(project);
  return (
    <>
      {query.isFetching ? <Spinner label="Loading trend" /> : null}
      {query.data === undefined ? (
        query.error === null ? null : (
          <ErrorCallout error={query.error} />
        )
      ) : (
        <>
          {query.error === null ? null : (
            <StaleWarning
              error={query.error}
              retrying={query.isFetching}
              onRetry={() => void query.refetch()}
            />
          )}
          <TrendBody trend={query.data} />
        </>
      )}
    </>
  );
}
