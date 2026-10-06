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
import { Section } from "@/ui/PageLayout";

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
    <div className="flex flex-col gap-6">
      <Section title="Timeline">
        <Timeline run={run} events={progress.events} error={progress.error} />
      </Section>
      <RunSummarySection run={run} streamError={streamError} temporalUiUrl={temporalUiUrl} />
      <Section title="Build log">
        <div ref={logRef}>
          <BuildLogPane runId={run.id} enabled={logOn} onEnabledChange={setLogOn} />
        </div>
      </Section>
      {run.state === "quarantined" ? <OverrideSection runId={run.id} /> : null}
      <Section title="Commit and artifact evidence">
        <Fields>
          <Field label="Base SHA" mono>
            {run.baseSha}
          </Field>
          <Field label="Result SHA" mono>
            {run.resultSha ?? "Not available"}
          </Field>
          <Field label="Spec SHA-256" mono>
            {run.specSha256}
          </Field>
          <Field label="Committed by factoryd">{run.committedByFactoryd ? "Yes" : "No"}</Field>
        </Fields>
      </Section>
      <Section title="Attempts">
        {run.attempts.length === 0 ? (
          <Fields>
            <Field label="Attempts">None</Field>
          </Fields>
        ) : (
          run.attempts.map((attempt, i) => (
            <AttemptCard key={`${attempt.startedAt}-${i}`} attempt={attempt} onOpenLog={openLog} />
          ))
        )}
      </Section>
      <Section title="Gate results">
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
      {run.composePhases.length > 0 ? (
        <Section title="Compose services">
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
      <Section title="Changed files">
        <Fields>
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
      {run.notifications.length > 0 ? (
        <Section title="Notifications">
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
      {/* Always offered, not only for an accepted run: the release view reports the project kill switch's state and history too, and a run with no decision is itself the answer to "was this released?", stated there explicitly. */}
      <Section title="Release">
        <Fields>
          <Field label="Decision">
            The factory-owned release decision for this run, and the project kill switch it was
            evaluated against.
          </Field>
        </Fields>
      </Section>
      {run.overrides.length > 0 ? (
        <Section title="Overrides">
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
  );
}
