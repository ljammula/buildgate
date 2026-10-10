import { useProjectStats } from "@/api/runQueries";
import { formatMedianAcceptedTokens } from "@/domain/cost";
import type { ProjectStats } from "@/domain/project";
import { Card } from "@/ui/Card";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Section } from "@/ui/PageLayout";
import { StaleWarning } from "@/ui/StaleWarning";

const NO_DATA = "No accepted runs yet";

function StatsBody({ stats }: { readonly stats: ProjectStats }) {
  const rate = stats.overrideRatePercent;
  const causes = Object.entries(stats.quarantinedByCause);
  return (
    <>
      <Card className="p-4">
        <DescriptionList labelWidth="lg">
          <DescriptionItem label="Project">
            <span className="font-mono">{stats.project}</span>
          </DescriptionItem>
          <DescriptionItem label="Total runs">{stats.totalRuns}</DescriptionItem>
          <DescriptionItem label="Accepted">{stats.accepted}</DescriptionItem>
          {/* Nothing halted is the usual answer, and a row that says so is noise. */}
          {stats.halted === 0 ? null : (
            <DescriptionItem label="Halted">{stats.halted}</DescriptionItem>
          )}
          <DescriptionItem label="Override rate (accepted)">
            {rate === null
              ? NO_DATA
              : `${rate}% (${stats.acceptedViaOverride} of ${stats.accepted})`}
          </DescriptionItem>
          <DescriptionItem label="Median accepted">
            {stats.medianAcceptedTokens === null
              ? NO_DATA
              : formatMedianAcceptedTokens(stats.medianAcceptedTokens)}
          </DescriptionItem>
        </DescriptionList>
      </Card>
      {/* Absent, not "none recorded", when nothing was quarantined. */}
      {causes.length === 0 ? null : (
        <Section title="Quarantined by cause">
          <DescriptionList labelWidth="lg">
            {causes.map(([cause, count]) => (
              <DescriptionItem key={cause} label={cause}>
                {count}
              </DescriptionItem>
            ))}
          </DescriptionList>
        </Section>
      )}
    </>
  );
}

/**
 * One project's figures. A failed refresh keeps the last data under a
 * warning; Retry is disabled while a refresh is in flight. Keyed by the
 * project, so another project starts from nothing.
 */
export function ProjectStatsPanel({ project }: { readonly project: string }) {
  const query = useProjectStats(project);
  return (
    <>
      {query.isFetching ? <Spinner label="Loading stats" /> : null}
      {query.data === undefined ? (
        query.error === null ? null : (
          // GET /projects/{project}/stats is start-token-gated.
          <ErrorCallout error={query.error} startClass />
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
          <StatsBody stats={query.data} />
        </>
      )}
    </>
  );
}
