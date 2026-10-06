import { useProjectRelease } from "@/api/runQueries";
import type { ProjectReleaseView } from "@/domain/release";
import { projectReleasePath } from "@/routes/paths";
import { Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Section } from "@/ui/PageLayout";
import { KillSwitchChip } from "@/ui/StatusChip";
import { Table, TableBody, TableCell, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";
import { LocalTimeText } from "@/ui/Time";

import { ProjectFieldList } from "./ProjectFieldList";
import { ProjectIdForm } from "./ProjectIdForm";
import { StaleWarning } from "./StaleWarning";
import { useLoadProject } from "./useLoadProject";

function ReleaseBody({ release }: { readonly release: ProjectReleaseView }) {
  const { killSwitch } = release;
  return (
    <>
      <ProjectFieldList
        fields={[
          { label: "Project", value: <span className="font-mono">{release.project}</span> },
          { label: "State", value: <KillSwitchChip engaged={killSwitch.engaged} /> },
          {
            label: "Control",
            value:
              "Engage and disengage from the command line (factoryd kill-switch). It is " +
              "deliberately not a console action, so hitting it never depends on a healthy " +
              "factoryd serve.",
          },
        ]}
      />
      <Section title="History">
        {killSwitch.history.length === 0 ? (
          <p className="text-sm text-fg-muted">
            Never engaged. This project has no recorded transitions.
          </p>
        ) : (
          <Table>
            <TableHead>
              <TableRow className="hover:bg-transparent">
                <TableHeaderCell>Transition</TableHeaderCell>
                <TableHeaderCell>By</TableHeaderCell>
                <TableHeaderCell>At</TableHeaderCell>
                <TableHeaderCell>Reason</TableHeaderCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {killSwitch.history.map((transition) => (
                <TableRow key={`${transition.at}-${transition.by}`}>
                  <TableCell className="font-medium">
                    {transition.engaged ? "Engaged" : "Disengaged"}
                  </TableCell>
                  <TableCell>{transition.by}</TableCell>
                  <TableCell className="text-xs">
                    <LocalTimeText value={transition.at} />
                  </TableCell>
                  <TableCell>{transition.reason}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>
    </>
  );
}

/**
 * One project's kill switch. A failed refresh keeps the last data under a
 * warning; another project (a new key in the screen) starts from nothing, so
 * project B's error can never sit next to project A's "Engaged" chip.
 */
export function ProjectReleasePanel({ project }: { readonly project: string }) {
  const query = useProjectRelease(project);
  const load = useLoadProject(project, projectReleasePath, () => void query.refetch());
  return (
    <>
      <ProjectIdForm initial={project} loading={query.isFetching} onLoad={load} />
      {query.isFetching ? <Spinner label="Loading release" /> : null}
      {query.data === undefined ? (
        query.error === null ? null : (
          // GET /projects/{project}/release is start-token-gated.
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
          <ReleaseBody release={query.data} />
        </>
      )}
    </>
  );
}
