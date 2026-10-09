import { Link } from "react-router";

import { useProjectMemory } from "@/api/runQueries";
import { type MemoryCandidate, type ProjectMemory, memoryBudgetText } from "@/domain/memory";
import { escapeInvisible } from "@/domain/textEscape";
import { requestPath } from "@/routes/paths";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { Section } from "@/ui/PageLayout";
import { StaleWarning } from "@/ui/StaleWarning";

/**
 * One candidate line. The line is a build agent's or the operator's note, so
 * it is text, with invisible characters written out.
 */
function CandidateItem({ candidate }: { readonly candidate: MemoryCandidate }) {
  return (
    <li
      data-testid="memory-candidate"
      className="flex flex-col gap-1 border-b border-border py-2 last:border-b-0"
    >
      <p className="font-mono text-sm text-fg">{escapeInvisible(candidate.line)}</p>
      <p className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-fg-muted">
        <span className="font-mono">{candidate.id}</span>
        <span>{candidate.state}</span>
        <span>{candidate.source}</span>
        <span>
          seen in {candidate.seen} {candidate.seen === 1 ? "run" : "runs"}
        </span>
        {candidate.requestId === "" ? null : (
          <Link
            to={requestPath(candidate.requestId)}
            className="font-mono text-accent hover:underline"
          >
            {candidate.requestId}
          </Link>
        )}
      </p>
    </li>
  );
}

function MemoryBody({ memory }: { readonly memory: ProjectMemory }) {
  return (
    <>
      <DescriptionList labelWidth="lg">
        <DescriptionItem label="Repository memory">
          {memory.on ? "On" : `Off: ${escapeInvisible(memory.offReason)}`}
        </DescriptionItem>
        <DescriptionItem label="Section budget">{memoryBudgetText(memory)}</DescriptionItem>
      </DescriptionList>
      <Section title="In force: the memory section of AGENTS.md at HEAD">
        {memory.sectionError === "" ? null : (
          <p className="text-xs text-fg-muted">
            The section could not be read: {escapeInvisible(memory.sectionError)}
          </p>
        )}
        {memory.inForce.length === 0 ? (
          <EmptyState title="No line in force">
            A line reaches the section only through a memory request you approve and merge.
          </EmptyState>
        ) : (
          <ul className="flex flex-col font-mono text-sm text-fg">
            {memory.inForce.map((line, i) => (
              <li key={i} data-testid="memory-line">
                {escapeInvisible(line)}
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title="Candidates, most seen first">
        {memory.candidates.length === 0 ? (
          <EmptyState title="No candidate">
            Run factoryd memory list to collect what build agents noted, or factoryd memory add to
            write your own.
          </EmptyState>
        ) : (
          <ul className="flex flex-col">
            {memory.candidates.map((candidate) => (
              <CandidateItem key={candidate.id} candidate={candidate} />
            ))}
          </ul>
        )}
      </Section>
    </>
  );
}

/**
 * A project's repository memory, read only: whether it is on, the lines in
 * force and the candidate lines. Proposing, dropping and switching it are
 * the operator's `factoryd memory` commands; nothing on this panel changes
 * anything. A failed refresh keeps the last data under a warning.
 */
export function ProjectMemoryPanel({ project }: { readonly project: string }) {
  const query = useProjectMemory(project);
  return (
    <>
      {query.isFetching ? <Spinner label="Loading memory" /> : null}
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
          <MemoryBody memory={query.data} />
        </>
      )}
    </>
  );
}
