import { useProjectStats } from "@/api/runQueries";
import { formatMedianAcceptedTokens } from "@/domain/cost";
import type { ProjectStats } from "@/domain/project";
import { projectStatsPath } from "@/routes/paths";
import { Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Section } from "@/ui/PageLayout";

import { ProjectFieldList } from "./ProjectFieldList";
import { ProjectIdForm } from "./ProjectIdForm";
import { StaleWarning } from "./StaleWarning";
import { useLoadProject } from "./useLoadProject";

const NO_DATA = "No accepted runs yet";

function StatsBody({ stats }: { readonly stats: ProjectStats }) {
  const rate = stats.overrideRatePercent;
  const causes = Object.entries(stats.quarantinedByCause);
  return (
    <>
      <ProjectFieldList
        fields={[
          { label: "Project", value: <span className="font-mono">{stats.project}</span> },
          { label: "Total runs", value: stats.totalRuns },
          { label: "Accepted", value: stats.accepted },
          { label: "Halted", value: stats.halted },
          {
            label: "Override rate (accepted)",
            value:
              rate === null
                ? NO_DATA
                : `${rate}% (${stats.acceptedViaOverride} of ${stats.accepted})`,
          },
          {
            label: "Median accepted",
            value:
              stats.medianAcceptedTokens === null
                ? NO_DATA
                : formatMedianAcceptedTokens(stats.medianAcceptedTokens),
          },
        ]}
      />
      <Section title="Quarantined by cause">
        {causes.length === 0 ? (
          <p className="text-sm text-fg-muted">No quarantined runs recorded.</p>
        ) : (
          <ProjectFieldList
            fields={causes.map(([cause, count]) => ({
              label: cause,
              value: count,
            }))}
          />
        )}
      </Section>
    </>
  );
}

/**
 * One project's figures. A failed refresh keeps the last data under a
 * warning; Retry is disabled while a refresh is in flight. Keyed by the
 * project in the screen, so another project starts from nothing.
 */
export function ProjectStatsPanel({ project }: { readonly project: string }) {
  const query = useProjectStats(project);
  const load = useLoadProject(project, projectStatsPath, () => void query.refetch());
  return (
    <>
      <ProjectIdForm initial={project} loading={query.isFetching} onLoad={load} />
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
              fetching={query.isFetching}
              onRetry={() => void query.refetch()}
            />
          )}
          <StatsBody stats={query.data} />
        </>
      )}
    </>
  );
}
