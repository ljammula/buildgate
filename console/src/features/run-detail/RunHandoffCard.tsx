import { useRunHandoff } from "@/api/runQueries";
import { ApiError } from "@/domain/apiError";
import { HANDOFF_NOT_JUDGED, handoffBinLabel, handoffNextSentence } from "@/domain/handoff";
import type { Run } from "@/domain/run";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { CodeBlock } from "@/ui/CodeBlock";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { Section } from "@/ui/PageLayout";

export interface RunHandoffCardProps {
  readonly run: Run;
}

/**
 * What a stopped run left for a later attempt: each check that failed, the
 * factory's finding about it, and how the failure is sorted (whether a build
 * may be told about it). Nothing for a run that recorded no handoff.
 *
 * It restates the factory's record; it does not say another attempt has been
 * or will be started. A handoff the server will not vouch for (409), or one
 * for a state the run has left, shows nothing. A finding may quote a line of a log, so it is rendered
 * as text.
 */
export function RunHandoffCard({ run }: RunHandoffCardProps) {
  const query = useRunHandoff(run.id, run.handoffSha256);
  if (run.handoffSha256 === "") return null;
  // The server answers 409 for a handoff it will not vouch for: the run has
  // left the state it describes, or the file is gone or was changed. There
  // is then nothing to show, and it is not a fault of this page.
  if (query.error instanceof ApiError && query.error.status === 409) return null;
  // A record of another state than the run is in is not shown either.
  if (query.data !== undefined && query.data.state !== run.state) return null;
  return (
    <div data-testid="run-handoff">
      <Section title="What this attempt left" card>
        {query.isPending ? <Spinner label="Loading the handoff" /> : null}
        {query.error === null ? null : <ErrorCallout error={query.error} />}
        {query.data === undefined ? null : (
          <div className="flex flex-col gap-3">
            <p className="text-sm text-fg">{handoffNextSentence(query.data.next)}</p>
            {query.data.checks.length === 0 ? null : (
              <ul className="flex flex-col">
                {query.data.checks.map((check, i) => (
                  <li
                    key={i}
                    data-testid="run-handoff-check"
                    className="flex flex-col gap-0.5 border-b border-border py-2 last:border-b-0"
                  >
                    <p className="flex flex-wrap items-baseline gap-x-3 text-sm">
                      <span className="font-mono font-medium text-fg">
                        <EscapedText text={check.check} />
                      </span>
                      <span className="text-xs text-fg-muted">
                        {check.notJudged ? HANDOFF_NOT_JUDGED : handoffBinLabel(check.bin)}
                      </span>
                    </p>
                    <p className="text-xs break-words text-fg-muted">
                      {check.finding === "" ? (
                        `Failed${check.exitCode === null || check.exitCode === 0 ? "" : ` (exit ${check.exitCode})`}; the factory has no more to say about it.`
                      ) : (
                        <EscapedText text={check.finding} />
                      )}
                    </p>
                    {check.output.length === 0 ? null : (
                      <CodeBlock
                        label={`Output of ${check.check}`}
                        wrap
                        maxHeight="max-h-40"
                        className="text-[11px]"
                      >
                        {check.output.join("\n")}
                      </CodeBlock>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </Section>
    </div>
  );
}
