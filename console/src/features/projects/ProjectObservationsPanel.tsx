import { Link } from "react-router";

import { useProjectObservations } from "@/api/runQueries";
import {
  type Observation,
  type ObservationReport,
  observationCounts,
  observationKindLabel,
} from "@/domain/observation";
import { escapeInvisible } from "@/domain/textEscape";
import { projectObservationsPath, requestPath, runPath } from "@/routes/paths";
import { CodeBlock } from "@/ui/CodeBlock";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { Section } from "@/ui/PageLayout";
import { StaleWarning } from "@/ui/StaleWarning";
import { LocalTimeText } from "@/ui/Time";

import { ProjectIdForm } from "./ProjectIdForm";
import { useLoadProject } from "./useLoadProject";

/**
 * One observation: the sentence, the run it is from, and what the round's
 * output said. The sentence's wording is the factory's; the names and the
 * excerpt in it are the build agent's report, so everything is text, with
 * invisible characters written out.
 */
function RunLink({ id }: { readonly id: string }) {
  return (
    <Link to={runPath(id)} className="font-mono text-accent hover:underline">
      {id}
    </Link>
  );
}

/**
 * The one line of identifiers under a sentence: the runs, the request, and
 * for the request kinds the thread ids or the gate and the places an
 * operator touched. Names only; no comment or edit text reaches the page.
 */
function ObservationRefs({ observation }: { readonly observation: Observation }) {
  return (
    <>
      {observation.runId === "" ? null : <RunLink id={observation.runId} />}
      {observation.acceptedRunId === "" ? null : (
        <>
          <span aria-hidden="true">{"\u2192"}</span>
          <RunLink id={observation.acceptedRunId} />
        </>
      )}
      {observation.requestId === "" ? null : (
        <Link
          to={requestPath(observation.requestId)}
          className="font-mono text-accent hover:underline"
        >
          {observation.requestId}
        </Link>
      )}
      {observation.threadIds.length === 0 ? null : (
        <span className="font-mono">{escapeInvisible(observation.threadIds.join(", "))}</span>
      )}
      {observation.stage === "" ? null : <span>{observation.stage}</span>}
      {observation.anchors.length === 0 ? null : (
        <span className="font-mono">{escapeInvisible(observation.anchors.join("; "))}</span>
      )}
    </>
  );
}

function ObservationItem({ observation }: { readonly observation: Observation }) {
  return (
    <li
      data-testid="observation"
      className="flex flex-col gap-1 border-b border-border py-2 last:border-b-0"
    >
      <p className="text-sm text-fg">{escapeInvisible(observation.what)}</p>
      <p className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-fg-muted">
        <span>{observationKindLabel(observation.kind)}</span>
        <ObservationRefs observation={observation} />
        {observation.at === "" ? null : <LocalTimeText value={observation.at} />}
      </p>
      {observation.checks.length === 0 ? null : (
        <ul className="text-xs text-fg-muted">
          {observation.checks.map((c) => (
            <li key={c.check}>
              <span className="font-mono">{escapeInvisible(c.check)}</span>
              {c.sentence === "" ? null : <> {escapeInvisible(c.sentence)}</>}
            </li>
          ))}
        </ul>
      )}
      {observation.excerpt === "" ? null : (
        <CodeBlock
          label={`Output of run ${observation.runId}: ${observation.log}`}
          wrap
          maxHeight="max-h-40"
          className="text-[11px]"
        >
          {observation.excerpt}
        </CodeBlock>
      )}
    </li>
  );
}

function ObservationsBody({ report }: { readonly report: ObservationReport }) {
  const counts = observationCounts(report);
  return (
    <>
      <DescriptionList labelWidth="lg">
        <DescriptionItem label="Project">
          <span className="font-mono">{report.project}</span>
        </DescriptionItem>
        <DescriptionItem label="Finished runs read">{report.runs}</DescriptionItem>
        <DescriptionItem label="Accepted in the first round">
          {report.acceptedFirstRound}
        </DescriptionItem>
        {counts.map(([kind, count]) => (
          <DescriptionItem key={kind} label={observationKindLabel(kind)}>
            {count}
          </DescriptionItem>
        ))}
      </DescriptionList>
      {report.observations.length === 0 ? (
        <EmptyState title="Nothing to report">
          {report.runs === 0
            ? "No finished run of this project is in this data directory."
            : "No finished run of this project recorded a failed round, a failed check or a halt."}
        </EmptyState>
      ) : (
        <Section title="What happened, newest run first">
          {report.truncated ? (
            <p className="text-xs text-fg-muted">
              Showing the {report.observations.length} newest; the counts above include the rest.
            </p>
          ) : null}
          <ul className="flex flex-col">
            {report.observations.map((observation, i) => (
              <ObservationItem key={i} observation={observation} />
            ))}
          </ul>
        </Section>
      )}
    </>
  );
}

/**
 * What a project's finished runs have shown: rounds that failed and were
 * fixed, failures that repeated, rounds that changed nothing, checks that
 * quarantined a run, halts. Read from the run records on each load; nothing
 * here is a setting, and nothing on it changes how a build runs. A failed
 * refresh keeps the last data under a warning.
 */
export function ProjectObservationsPanel({
  project,
  withForm = true,
}: {
  readonly project: string;
  /** False inside a project's own row, where there is no other project to load. */
  readonly withForm?: boolean;
}) {
  const query = useProjectObservations(project);
  const load = useLoadProject(project, projectObservationsPath, () => void query.refetch());
  return (
    <>
      {withForm ? (
        <ProjectIdForm initial={project} loading={query.isFetching} onLoad={load} />
      ) : null}
      {query.isFetching ? <Spinner label="Loading observations" /> : null}
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
          <ObservationsBody report={query.data} />
        </>
      )}
    </>
  );
}
