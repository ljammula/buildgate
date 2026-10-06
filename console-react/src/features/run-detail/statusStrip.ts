import { elapsedBetween, formatElapsedCompact, tryParseTimestamp } from "@/domain/elapsed";
import type { ProgressEvent, Run } from "@/domain/run";
import { runIsTerminalForDisplay } from "@/domain/run";
import { laterOf, stageLabels } from "@/domain/runDetail";

export interface StatusStrip {
  /** The server's waiting reason, else the current stage's label, else "Pending". */
  readonly label: string;
  /** "Round n/m" from the latest worker round line, null without a known maximum. */
  readonly round: string | null;
  readonly elapsed: string;
  /** Null for a run that is over: there is no "last activity" to report. */
  readonly lastActivity: string | null;
}

/**
 * The Timeline's one-line summary. A server-reported waiting_reason (queued
 * behind another run, say) replaces the stage label: "silence is a bug", so an
 * operator sees why nothing is happening, not "Pending" beside an unexplained
 * gap. The `finished`/`end` line, when present, is the moment the run stopped,
 * more precise than updatedAt, which can lag it. Last activity is the newer
 * of the server's field and the live feed, which can be ahead of the last
 * full run fetch.
 */
export function computeStatusStrip(
  events: readonly ProgressEvent[],
  run: Run,
  now: Date,
): StatusStrip {
  const factoryEvents = events.filter((e) => e.source === "factory");
  const currentStage = factoryEvents.at(-1)?.stage ?? "";
  const waiting = run.waitingReason ?? "";
  const label =
    waiting !== ""
      ? waiting
      : currentStage === ""
        ? "Pending"
        : (stageLabels[currentStage] ?? currentStage);

  const latestRound = events.filter((e) => e.source === "worker" && e.stage === "round").at(-1);
  const round =
    latestRound !== undefined && latestRound.maxRounds > 0
      ? `Round ${latestRound.round}/${latestRound.maxRounds}`
      : null;

  const finishedAt = factoryEvents
    .filter((e) => e.stage === "finished" && e.event === "end")
    .at(-1);
  const over = runIsTerminalForDisplay(run);
  const terminalAt = finishedAt !== undefined ? finishedAt.ts.toISOString() : run.updatedAt;
  const elapsed = formatElapsedCompact(
    elapsedBetween(run.createdAt, over ? terminalAt : null, now),
  );

  if (over) return { label, round, elapsed, lastActivity: null };
  const lastProgress = laterOf(
    factoryEvents.at(-1)?.ts ?? null,
    tryParseTimestamp(run.lastProgressAt ?? ""),
  );
  const since = (lastProgress ?? tryParseTimestamp(run.createdAt))?.toISOString() ?? run.createdAt;
  return {
    label,
    round,
    elapsed,
    lastActivity: formatElapsedCompact(elapsedBetween(since, null, now)),
  };
}
