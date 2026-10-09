import { useRef, useState } from "react";

import { formatLocalTimestamp } from "@/domain/elapsed";
import type { Run } from "@/domain/run";
import { runIsTerminalForDisplay } from "@/domain/run";
import { attemptsSummary, gatesSummary } from "@/domain/runSummary";
import { AttemptsBlock } from "@/features/run-detail/AttemptsBlock";
import { BuildLogPane } from "@/features/run-detail/BuildLogPane";
import { ComposeBlock } from "@/features/run-detail/ComposeBlock";
import { EvidenceCard } from "@/features/run-detail/EvidenceCard";
import { GatesBlock } from "@/features/run-detail/GatesBlock";
import { listKeys } from "@/features/run-detail/listKeys";
import { OverrideSection } from "@/features/run-detail/OverrideSection";
import { RunFactsCard } from "@/features/run-detail/RunFactsCard";
import { RunHandoffCard } from "@/features/run-detail/RunHandoffCard";
import { RunReleaseCard } from "@/features/run-detail/RunReleaseCard";
import { RunStopCallout } from "./RunStopCallout";
import { RunVerdictLine } from "@/features/run-detail/RunVerdictLine";
import { Timeline } from "@/features/run-detail/Timeline";
import type { RunProgress } from "@/features/run-detail/useRunProgress";
import { Section } from "@/ui/PageLayout";

export interface RunOverviewProps {
  readonly run: Run;
  readonly progress: RunProgress;
  readonly streamError: unknown;
  readonly temporalUiUrl: string | null;
}

/**
 * The run's evidence. A finished run opens with its verdict in one line, then
 * whatever failed (open, first), the Timeline and the build log, then the
 * attempts, gates and compose services as closed one-line blocks. A block
 * with nothing in it yet is absent, not "None". The Timeline stays ahead of
 * anything that only exists once a stage has finished: "what is it doing
 * right now" comes first on a live run.
 */
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

  const over = runIsTerminalForDisplay(run);
  const attemptsFailed = run.attempts.length > 0 && attemptsSummary(run.attempts).failed;
  const gatesFailed = run.gateResults.length > 0 && gatesSummary(run.gateResults).failed;
  const attempts = <AttemptsBlock attempts={run.attempts} onOpenLog={openLog} />;
  const gates = <GatesBlock gates={run.gateResults} />;
  const notificationKeys = listKeys(run.notifications, (n) => `${n.sentAt}-${n.state}`);
  const overrideKeys = listKeys(run.overrides, (o) => `${o.at}-${o.by}`);
  return (
    <div className="grid items-start gap-4 lg:grid-cols-[minmax(0,1fr)_22rem]">
      <div className="flex min-w-0 flex-col gap-4">
        {over ? <RunVerdictLine run={run} /> : null}
        <RunStopCallout run={run} />
        <RunHandoffCard run={run} />
        {gatesFailed ? gates : null}
        {attemptsFailed ? attempts : null}
        <Section title="Timeline" card>
          <Timeline run={run} events={progress.events} error={progress.error} />
        </Section>
        <Section title="Build log" card>
          <div ref={logRef}>
            <BuildLogPane runId={run.id} enabled={logOn} onEnabledChange={setLogOn} />
          </div>
        </Section>
        {attemptsFailed ? null : attempts}
        {gatesFailed ? null : gates}
        <ComposeBlock phases={run.composePhases} />
        {run.notifications.length > 0 ? (
          <Section title="Notifications" card>
            {run.notifications.map((n, i) => (
              <EvidenceCard
                key={notificationKeys[i]}
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
                key={overrideKeys[i]}
                title={`${o.priorState} → ${o.newState}`}
                lines={[`By: ${o.by}`, `At: ${formatLocalTimestamp(o.at)}`, `Reason: ${o.reason}`]}
              />
            ))}
          </Section>
        ) : null}
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        {run.state === "quarantined" ? <OverrideSection runId={run.id} /> : null}
        <RunFactsCard run={run} streamError={streamError} temporalUiUrl={temporalUiUrl} />
        {/* Always offered, not only for an accepted run: the release view reports the project kill switch's state and history too, and a run with no decision is itself the answer to "was this released?", stated there explicitly. */}
        <RunReleaseCard runId={run.id} accepted={run.state === "accepted"} />
      </div>
    </div>
  );
}
