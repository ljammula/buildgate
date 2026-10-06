import { useRef, useState } from "react";

import { formatLocalTimestamp } from "@/domain/elapsed";
import type { Run } from "@/domain/run";
import { composeServiceAddress } from "@/domain/run";
import { AttemptCard } from "@/features/run-detail/AttemptCard";
import { BuildLogPane } from "@/features/run-detail/BuildLogPane";
import { EvidenceCard } from "@/features/run-detail/EvidenceCard";
import { Field, Fields } from "@/features/run-detail/Fields";
import { OverrideSection } from "@/features/run-detail/OverrideSection";
import { RunSummarySection } from "@/features/run-detail/RunSummarySection";
import { Timeline } from "@/features/run-detail/Timeline";
import type { RunProgress } from "@/features/run-detail/useRunProgress";
import { CompactId } from "@/ui/CompactId";
import { Section } from "@/ui/PageLayout";

// Label column of the side column's facts: the main column's 11rem would leave a 22rem card with no room for a value.
const sideFields = "grid-cols-[6.5rem_minmax(0,1fr)]";

export interface RunOverviewProps {
  readonly run: Run;
  readonly progress: RunProgress;
  readonly streamError: unknown;
  readonly temporalUiUrl: string | null;
}

/** Every section of the run's evidence, the Timeline first: "what is it doing right now" comes before anything that only exists once a stage has finished. */
export function RunOverview({ run, progress, streamError, temporalUiUrl }: RunOverviewProps) {
  const [logOn, setLogOn] = useState(false);
  const logRef = useRef<HTMLDivElement>(null);

  // An attempt's "Open log": turns the pane on the way its own switch would,
  // and brings it into view, reusing the one viewer instead of a second.
  function openLog() {
    setLogOn(true);
    const pane = logRef.current;
    if (pane !== null && typeof pane.scrollIntoView === "function") pane.scrollIntoView();
  }

  const diff = run.diffStat;
  const files = run.changedFiles;
  return (
    <div className="grid items-start gap-4 lg:grid-cols-[minmax(0,1fr)_22rem]">
      <div className="flex min-w-0 flex-col gap-4">
        <Section title="Timeline" card>
          <Timeline run={run} events={progress.events} error={progress.error} />
        </Section>
        <Section title="Build log" card>
          <div ref={logRef}>
            <BuildLogPane runId={run.id} enabled={logOn} onEnabledChange={setLogOn} />
          </div>
        </Section>
        <Section title="Attempts" card>
          {run.attempts.length === 0 ? (
            <Fields>
              <Field label="Attempts">None</Field>
            </Fields>
          ) : (
            run.attempts.map((attempt, i) => (
              <AttemptCard
                key={`${attempt.startedAt}-${i}`}
                attempt={attempt}
                onOpenLog={openLog}
              />
            ))
          )}
        </Section>
        <Section title="Gate results" card>
          {run.gateResults.length === 0 ? (
            <Fields>
              <Field label="Gate results">None</Field>
            </Fields>
          ) : (
            run.gateResults.map((gate, i) => (
              <EvidenceCard
                key={`${gate.check}-${i}`}
                title={gate.check}
                lines={[
                  `Command: ${gate.command.join(" ")}`,
                  `Passed: ${gate.passed ? "Yes" : "No"}`,
                  `Exit code: ${gate.exitCode}`,
                  `Duration: ${gate.durationMs} ms`,
                  `Log SHA-256: ${gate.logSha256}`,
                ]}
              />
            ))
          )}
        </Section>
        {run.notifications.length > 0 ? (
          <Section title="Notifications" card>
            {run.notifications.map((n, i) => (
              <EvidenceCard
                key={`${n.sentAt}-${i}`}
                title={n.state}
                lines={[
                  n.reason,
                  `Sent: ${formatLocalTimestamp(n.sentAt)}`,
                  `Ticket: ${n.ticket}`,
                  `Run ID: ${n.runId}`,
                ]}
              />
            ))}
          </Section>
        ) : null}
        {run.overrides.length > 0 ? (
          <Section title="Overrides" card>
            {run.overrides.map((o, i) => (
              <EvidenceCard
                key={`${o.at}-${i}`}
                title={`${o.priorState} → ${o.newState}`}
                lines={[`By: ${o.by}`, `At: ${formatLocalTimestamp(o.at)}`, `Reason: ${o.reason}`]}
              />
            ))}
          </Section>
        ) : null}
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        {run.state === "quarantined" ? <OverrideSection runId={run.id} /> : null}
        <RunSummarySection run={run} streamError={streamError} temporalUiUrl={temporalUiUrl} />
        <Section title="Commit and artifact evidence" card>
          <Fields className={sideFields}>
            <Field label="Base SHA" mono>
              <CompactId value={run.baseSha} max={20} label="base SHA" />
            </Field>
            <Field label="Result SHA" mono>
              {run.resultSha === null ? (
                "Not available"
              ) : (
                <CompactId value={run.resultSha} max={20} label="result SHA" />
              )}
            </Field>
            <Field label="Spec SHA-256" mono>
              <CompactId value={run.specSha256} max={20} label="spec SHA-256" />
            </Field>
            <Field label="Committed by factoryd">{run.committedByFactoryd ? "Yes" : "No"}</Field>
          </Fields>
        </Section>
        <Section title="Changed files" card>
          <Fields className={sideFields}>
            {files === null ? (
              <Field label="Files">Not collected</Field>
            ) : files.length === 0 ? (
              <Field label="Files">None</Field>
            ) : (
              files.map((file) => (
                <Field key={file} label="File" mono>
                  {file}
                </Field>
              ))
            )}
            <Field label="Diff stat">
              {diff === null
                ? "Not available"
                : `${diff.filesChanged} files, +${diff.insertions}, -${diff.deletions}`}
            </Field>
          </Fields>
        </Section>
        {run.composePhases.length > 0 ? (
          <Section title="Compose services" card>
            {run.composePhases.map((phase, i) => (
              <EvidenceCard
                key={`${phase.phase}-${i}`}
                title={phase.phase === "" ? "run" : phase.phase}
                lines={
                  phase.enabled
                    ? phase.services.map(
                        (svc) =>
                          `${svc.name}: ${svc.image} at ${composeServiceAddress(svc)}${
                            svc.digest === "" ? "" : ` (${svc.digest})`
                          }`,
                      )
                    : [`Not launched: ${phase.disabledReason}`]
                }
              />
            ))}
          </Section>
        ) : null}
        {/* Always offered, not only for an accepted run: the release view reports the project kill switch's state and history too, and a run with no decision is itself the answer to "was this released?", stated there explicitly. */}
        <Section title="Release" card>
          <Fields className={sideFields}>
            <Field label="Decision">
              The factory-owned release decision for this run, and the project kill switch it was
              evaluated against.
            </Field>
          </Fields>
        </Section>
      </div>
    </div>
  );
}
