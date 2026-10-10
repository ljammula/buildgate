import { useProjectRelease } from "@/api/runQueries";
import type { ProjectReleaseView } from "@/domain/release";
import { Card } from "@/ui/Card";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Section } from "@/ui/PageLayout";
import { StaleWarning } from "@/ui/StaleWarning";
import { KillSwitchChip } from "@/ui/StatusChip";
import {
  Table,
  TableBody,
  TableCell,
  TableFrame,
  TableHead,
  TableHeaderCell,
  TableRow,
} from "@/ui/Table";
import { LocalTimeText } from "@/ui/Time";

function ReleaseBody({ release }: { readonly release: ProjectReleaseView }) {
  const { killSwitch } = release;
  return (
    <>
      <Card className="p-4">
        <DescriptionList labelWidth="lg">
          <DescriptionItem label="Project">
            <span className="font-mono">{release.project}</span>
          </DescriptionItem>
          <DescriptionItem label="State">
            <KillSwitchChip engaged={killSwitch.engaged} />
          </DescriptionItem>
          <DescriptionItem label="Control">
            Engage and disengage from the command line (factoryd kill-switch). It is deliberately
            not a console action, so hitting it never depends on a healthy factoryd serve.
          </DescriptionItem>
        </DescriptionList>
      </Card>
      <Section title="History">
        {killSwitch.history.length === 0 ? (
          <p className="text-sm text-fg-muted">
            Never engaged. This project has no recorded transitions.
          </p>
        ) : (
          <TableFrame>
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
                    <TableCell className="text-xs tabular-nums">
                      <LocalTimeText value={transition.at} />
                    </TableCell>
                    <TableCell>{transition.reason}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableFrame>
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
  return (
    <>
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
              retrying={query.isFetching}
              onRetry={() => void query.refetch()}
            />
          )}
          <ReleaseBody release={query.data} />
        </>
      )}
    </>
  );
}
