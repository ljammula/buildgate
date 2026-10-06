// The run detail screen's pure rules: the Timeline's stage rows and glyphs,
// the attempt card's exit and model lines. Time is a parameter (`now`).
import { formatTokenCount } from "@/domain/cost";
import { formatDuration } from "@/domain/elapsed";
import type { Attempt, GateResult, ProgressEvent, Run } from "@/domain/run";
import { agentEvidenceRoundOutcome, runIsTerminal } from "@/domain/run";

/**
 * The exit line for an attempt. A combined review launch exits
 * `combinedReviewExitBase` + bits (bit 0: spec conformity did not pass, bit
 * 1: code review did not pass; reviewstep.CombinedExitBase in Go), so its 40
 * reads as the pass it is rather than an unexplained failure code.
 */
export function attemptExitText(attempt: Pick<Attempt, "kind" | "exitCode">): string {
  const bits = attempt.exitCode - combinedReviewExitBase;
  if (attempt.kind === "review" && bits >= 0 && bits <= 3) {
    const conformity = (bits & 1) === 0 ? "passed" : "failed";
    const codeReview = (bits & 2) === 0 ? "passed" : "failed";
    return `Exit code: ${attempt.exitCode} (spec conformity ${conformity}, code review ${codeReview})`;
  }
  return `Exit code: ${attempt.exitCode}`;
}

/** combined_review.py's exit-status offset; must match reviewstep.CombinedExitBase. */
export const combinedReviewExitBase = 40;

/**
 * The attempt card's role/harness/model/thinking line, or null when the
 * attempt recorded none of them. The "(sent: X)" suffix appears only when
 * the relay actually observed a different effort than what this attempt's
 * role was told to use -- the same silent-clamp signal `factoryd
 * watch`/`status` surface as "(requested X, sent Y)".
 */
export function attemptModelLine(
  attempt: Pick<
    Attempt,
    | "role"
    | "harness"
    | "thinking"
    | "relayWorkerModelId"
    | "expectedEffort"
    | "relayReasoningEffort"
    | "relayReasoningEffortAnomaly"
  >,
): string | null {
  if (
    attempt.role === "" &&
    attempt.harness === "" &&
    attempt.thinking === "" &&
    attempt.relayWorkerModelId === ""
  ) {
    return null;
  }
  const parts: string[] = [];
  if (attempt.role !== "") parts.push(`Role: ${attempt.role}`);
  if (attempt.harness !== "") parts.push(`Harness: ${attempt.harness}`);
  if (attempt.relayWorkerModelId !== "") parts.push(`Model: ${attempt.relayWorkerModelId}`);
  if (attempt.thinking !== "") {
    const effort = attempt.relayReasoningEffort;
    let thinking = `Thinking: ${attempt.thinking}`;
    // Compares expectedEffort (what Pi's own thinkingLevelMap translation
    // actually turns thinking into), not thinking directly: a model whose
    // thinkingLevelMap legitimately renames "max" to "xhigh" must not read
    // as a clamp just because thinking ("max") differs from effort
    // ("xhigh") -- found via review, the false positive comparing thinking
    // directly produced.
    const expected = attempt.expectedEffort;
    if (expected !== "" && effort !== "" && expected !== effort) {
      thinking += ` (sent: ${effort})`;
    }
    if (attempt.relayReasoningEffortAnomaly) thinking += " (anomaly)";
    parts.push(thinking);
  }
  return parts.join(" · ");
}

/**
 * The order the Timeline renders factory stages in -- matches
 * progress-contract.md's own "Factory stages" list exactly. `gate` is
 * singular here (one row key) even though several distinct gates can each
 * emit their own start/end pair under it (detail = gate name) -- see
 * `gateRow`, which fans those out into that one row's sub-rows.
 */
export const stageOrder: readonly string[] = [
  "prepare_workspace",
  "preflight",
  "build",
  "post_build",
  "verify",
  "full_suite",
  "gate",
  "evidence",
  "conformity_review",
  "code_review",
  // 'review' is the combined-launch stage (reviewstep.CombinedStep): when
  // both spec-conformity and code review are enabled for a run, factoryd
  // runs ONE combined_review.py launch in place of the two standalone stages
  // above (measured live, a Flutter + Go app repo request, 2026-09-28:
  // review was 50% of buildgate's whole spend, split across two separate
  // fresh sandboxed sessions over the same diff). The two standalone stages
  // stay in this order for a run with only one review enabled -- this is
  // additive, not a replacement.
  "review",
  "commit_oracles",
  "post_oracle_commit_verify",
  "evaluate",
  "finished",
];

export const stageLabels: Readonly<Record<string, string>> = {
  prepare_workspace: "Prepare workspace",
  preflight: "Preflight",
  build: "Build",
  post_build: "Post-build",
  verify: "Verify",
  full_suite: "Full suite",
  gate: "Gates",
  evidence: "Evidence",
  conformity_review: "Conformity review",
  code_review: "Code review",
  review: "Review (conformity + code)",
  commit_oracles: "Commit oracles",
  post_oracle_commit_verify: "Re-verify after oracle commit",
  evaluate: "Evaluate",
  finished: "Finished",
};

export type StageGlyph = "pending" | "running" | "passed" | "failed" | "skipped";

export interface TimelineSubRow {
  readonly label: string;
  readonly glyph: StageGlyph;
  readonly durationText: string | null;
  /**
   * True for a sub-row rendered from the factory's own AgentEvidence (a
   * per-round evidence summary), rendered in normal body text -- false for
   * the worker-relayed round/agent-note lines, which stay in their
   * secondary style (untrusted, display-only).
   */
  readonly factoryAuthored: boolean;
}

export interface TimelineRow {
  readonly rowKey: string;
  readonly label: string;
  readonly glyph: StageGlyph;
  readonly durationText: string | null;
  readonly subRows: readonly TimelineSubRow[];
  readonly noteLines: readonly string[];
  /**
   * The latest factory `build` note's detail (progress-contract.md's
   * "Additions" one-line round summary, e.g. "3 rounds · r1 fail (verify) ·
   * r3 pass · 41.2k tokens · $0.12") -- shown as this row's own subtitle,
   * distinct from subRows/noteLines below it.
   */
  readonly subtitle: string | null;
}

/** The later of two possibly-null timestamps -- null only when both are. */
export function laterOf(a: Date | null, b: Date | null): Date | null {
  if (a === null) return b;
  if (b === null) return a;
  return a.getTime() > b.getTime() ? a : b;
}

export function firstStart(events: readonly ProgressEvent[]): Date | null {
  for (const event of events) {
    if (event.event === "start") return event.ts;
  }
  return null;
}

export function lastEnd(events: readonly ProgressEvent[]): Date | null {
  let end: Date | null = null;
  for (const event of events) {
    if (event.event === "end") end = event.ts;
  }
  return end;
}

export function endOutcome(events: readonly ProgressEvent[]): string {
  let outcome = "";
  for (const event of events) {
    if (event.event === "end") outcome = event.outcome;
  }
  return outcome;
}

/**
 * Derives one stage/gate/round's display glyph from its own start/end
 * events. `finished` (the run's own progress feed has a `stage: "finished",
 * event: "end"` line) distinguishes "not yet reached" ("pending") from "the
 * run ended without this stage ever appearing" ("skipped") --
 * progress-contract.md's own "a stage that is skipped by config simply
 * never appears".
 */
export function glyphFor(args: {
  readonly start: Date | null;
  readonly outcome: string;
  readonly end: Date | null;
  readonly finished: boolean;
}): StageGlyph {
  const { start, outcome, end, finished } = args;
  if (end !== null) {
    if (outcome === "fail") return "failed";
    // The `finished` stage's own outcome is the terminal run state
    // (accepted/quarantined/halted), not pass/fail -- only "accepted" reads
    // as passed.
    if (outcome === "pass" || outcome === "accepted" || outcome === "") return "passed";
    return "failed";
  }
  if (start !== null) return "running";
  return finished ? "skipped" : "pending";
}

export function durationText(
  start: Date | null,
  end: Date | null,
  now: Date,
  glyph: StageGlyph,
): string | null {
  if (start === null) return null;
  if (end !== null) return formatDuration(end.getTime() - start.getTime());
  if (glyph === "running") return formatDuration(now.getTime() - start.getTime());
  return null;
}

export function overallGlyph(glyphs: readonly StageGlyph[], finished: boolean): StageGlyph {
  if (glyphs.length === 0) return finished ? "skipped" : "pending";
  if (glyphs.some((g) => g === "failed")) return "failed";
  if (glyphs.some((g) => g === "running")) return "running";
  if (glyphs.every((g) => g === "passed")) return "passed";
  return "pending";
}

/** The two oracle-commit stages that only ever run for a request launched with `-draft-oracles`. */
const optionalOracleStages: ReadonlySet<string> = new Set([
  "commit_oracles",
  "post_oracle_commit_verify",
]);

/** The run the Timeline is computed for. */
export type TimelineRun = Run;

function row(
  r: Pick<TimelineRow, "rowKey" | "label" | "glyph"> & Partial<TimelineRow>,
): TimelineRow {
  return { durationText: null, subRows: [], noteLines: [], subtitle: null, ...r };
}

function subRow(
  r: Pick<TimelineSubRow, "label" | "glyph"> & Partial<TimelineSubRow>,
): TimelineSubRow {
  return { durationText: null, factoryAuthored: false, ...r };
}

function label(stage: string): string {
  return stageLabels[stage] ?? stage;
}

function simpleStageRow(
  stage: string,
  events: readonly ProgressEvent[],
  finished: boolean,
  now: Date,
): TimelineRow {
  const start = firstStart(events);
  const end = lastEnd(events);
  const glyph = glyphFor({ start, end, outcome: endOutcome(events), finished });
  return row({
    rowKey: stage,
    label: label(stage),
    glyph,
    durationText: durationText(start, end, now, glyph),
  });
}

/**
 * Builds the Gates row directly from `run.gate_results` (the built-in policy
 * gates' own recorded evidence) when the progress feed has no named-gate
 * events to show -- see `gateRow`'s doc comment.
 */
export function gateRowFromResults(results: readonly GateResult[]): TimelineRow {
  const failed = results.filter((g) => !g.passed);
  const glyph: StageGlyph = failed.length === 0 ? "passed" : "failed";
  const subtitle =
    failed.length === 0
      ? `${results.length} passed: ${results.map((g) => g.check).join(", ")}`
      : `${failed.length} failed of ${results.length}: ${results
          .map((g) => `${g.check} (${g.passed ? "pass" : "fail"})`)
          .join(", ")}`;
  return row({ rowKey: "gate", label: label("gate"), glyph, subtitle });
}

function subRowFor(
  name: string,
  events: readonly ProgressEvent[],
  finished: boolean,
  now: Date,
): TimelineSubRow {
  const start = firstStart(events);
  const end = lastEnd(events);
  const glyph = glyphFor({ start, end, outcome: endOutcome(events), finished });
  return subRow({ label: name, glyph, durationText: durationText(start, end, now, glyph) });
}

function gateRow(
  events: readonly ProgressEvent[],
  finished: boolean,
  now: Date,
  run: TimelineRun,
): TimelineRow {
  const byGate = new Map<string, ProgressEvent[]>();
  for (const event of events) {
    const name = event.detail === "" ? "(unnamed gate)" : event.detail;
    const list = byGate.get(name);
    if (list === undefined) byGate.set(name, [event]);
    else list.push(event);
  }
  // No named-gate progress events (no lint/security_audit/etc. configured)
  // does NOT mean no gates ran: the built-in policy gates (canonical_verify,
  // diff_scope, ...) are always evaluated but never emit their own `gate`
  // progress events (found live, 2026-09-26 operator walk) -- falling
  // through to overallGlyph on an empty subRows list here read as
  // "skipped", which looked like the factory skipped its gates entirely.
  // The run's own recorded gate_results is the source of truth for those
  // checks, so use it whenever the feed has nothing to show.
  if (byGate.size === 0 && run.gateResults.length > 0) {
    return gateRowFromResults(run.gateResults);
  }
  const subRows = [...byGate].map(([name, list]) => subRowFor(name, list, finished, now));
  return row({
    rowKey: "gate",
    label: label("gate"),
    glyph: overallGlyph(
      subRows.map((r) => r.glyph),
      finished,
    ),
    subRows,
  });
}

/**
 * The latest factory `build`/`note` event's detail (progress-contract.md's
 * "Additions" one-line round summary, written once after evidence
 * collection) -- "latest" in case a chained/reconciled run ever produced
 * more than one, though today's call sites emit at most one per run.
 */
export function latestBuildNote(factoryEvents: readonly ProgressEvent[]): string | null {
  let note: string | null = null;
  for (const event of factoryEvents) {
    if (event.event === "note" && event.detail !== "") note = event.detail;
  }
  return note;
}

/**
 * One sub-row per AgentEvidenceRound, factory-authored from
 * BUILD_EVIDENCE.json -- only once the run is terminal and evidence was
 * actually collected (see run.Run.AgentEvidence's own doc comment: recorded
 * after policy gates decide the run's terminal state, so a still-running
 * build has none of this yet).
 */
export function evidenceRoundSubRows(run: TimelineRun): readonly TimelineSubRow[] {
  if (!runIsTerminal(run)) return [];
  const rounds = run.agentEvidence?.rounds ?? [];
  return rounds.map((rd) =>
    subRow({
      label: [
        `Round ${rd.index}`,
        agentEvidenceRoundOutcome(rd),
        ...(rd.tokens > 0 ? [`${formatTokenCount(rd.tokens)} tokens`] : []),
        `${Math.round(rd.durationS)}s`,
      ].join(" · "),
      glyph: agentEvidenceRoundOutcome(rd) === "pass" ? "passed" : "failed",
      factoryAuthored: true,
    }),
  );
}

interface RoundInfo {
  readonly round: number;
  readonly maxRounds: number;
  start: Date | null;
  end: Date | null;
  outcome: string;
  detail: string;
}

function roundSubRow(round: RoundInfo, now: Date): TimelineSubRow {
  const base = `Round ${round.round} of ${round.maxRounds}`;
  const text =
    round.detail !== ""
      ? `${base} · ${round.detail}`
      : round.outcome !== ""
        ? `${base} · ${round.outcome}`
        : base;
  const glyph: StageGlyph =
    round.end === null ? "running" : round.outcome === "fail" ? "failed" : "passed";
  const duration =
    round.start === null
      ? null
      : formatDuration((round.end ?? now).getTime() - round.start.getTime());
  return subRow({ label: text, glyph, durationText: duration });
}

function buildStageRow(
  allEvents: readonly ProgressEvent[],
  finished: boolean,
  now: Date,
  run: TimelineRun,
): TimelineRow {
  const factoryEvents = allEvents.filter((e) => e.source === "factory" && e.stage === "build");
  const start = firstStart(factoryEvents);
  const end = lastEnd(factoryEvents);
  const glyph = glyphFor({ start, end, outcome: endOutcome(factoryEvents), finished });

  // Worker rounds, in first-seen order (see progress-contract.md's own
  // "Worker stages" section for the round start/end/agent-note shape).
  const rounds = new Map<number, RoundInfo>();
  for (const event of allEvents) {
    if (event.source !== "worker" || event.stage !== "round") continue;
    let info = rounds.get(event.round);
    if (info === undefined) {
      info = {
        round: event.round,
        maxRounds: event.maxRounds,
        start: null,
        end: null,
        outcome: "",
        detail: "",
      };
      rounds.set(event.round, info);
    }
    if (event.event === "start") {
      info.start = event.ts;
    } else if (event.event === "end") {
      info.end = event.ts;
      info.outcome = event.outcome;
      info.detail = event.detail;
    }
  }
  // A terminal run's own evidence rounds already carry every worker round's
  // index as a more informative "Round N · outcome · tokens · duration" line
  // -- rendering the worker-relayed "Round N of M · outcome" line for the
  // same index too was a visible duplicate. Only rounds evidence hasn't
  // recorded (a still-running build, or evidence that predates or is
  // missing a round for some reason) fall back to the worker-relayed line.
  const evidenceRoundIndexes = new Set((run.agentEvidence?.rounds ?? []).map((r) => r.index));
  const subRows = [
    ...evidenceRoundSubRows(run),
    ...[...rounds.values()]
      .filter((r) => !evidenceRoundIndexes.has(r.round))
      .map((r) => roundSubRow(r, now)),
  ];

  // Agent notes under the current (latest) round only -- last 8, newest at
  // the bottom. They render as plain, untrusted text.
  const order = [...rounds.keys()];
  const currentRound = order.length > 0 ? order[order.length - 1] : null;
  const notes = allEvents
    .filter(
      (e) =>
        e.source === "worker" &&
        e.stage === "agent" &&
        e.event === "note" &&
        (currentRound === null || e.round === currentRound),
    )
    .map((e) => e.detail);
  const lastNotes = notes.length > 8 ? notes.slice(notes.length - 8) : notes;

  return row({
    rowKey: "build",
    label: label("build"),
    glyph,
    durationText: durationText(start, end, now, glyph),
    subRows,
    noteLines: lastNotes,
    subtitle: latestBuildNote(factoryEvents),
  });
}

/** The Timeline's rows, one per factory stage in `stageOrder`, from the run's progress events. */
export function computeTimeline(
  events: readonly ProgressEvent[],
  now: Date,
  run: TimelineRun,
): readonly TimelineRow[] {
  const byStage = new Map<string, ProgressEvent[]>();
  for (const event of events) {
    if (event.source !== "factory") continue;
    const list = byStage.get(event.stage);
    if (list === undefined) byStage.set(event.stage, [event]);
    else list.push(event);
  }
  const finished = (byStage.get("finished") ?? []).some((e) => e.event === "end");
  const rows: TimelineRow[] = [];
  for (const stage of stageOrder) {
    // A run launched with no -reference-oracle-dir never visits the two
    // oracle-commit stages -- omit them rather than showing a
    // pending/skipped row for a stage this run will never reach (operator
    // demo, 2026-09-26). A run that DID emit events for one (an older
    // record, or a race with the field landing) still shows it, so real
    // evidence is never hidden.
    if (
      run.referenceOracleDir === "" &&
      optionalOracleStages.has(stage) &&
      (byStage.get(stage)?.length ?? 0) === 0
    ) {
      continue;
    }
    if (stage === "build") rows.push(buildStageRow(events, finished, now, run));
    else if (stage === "gate") rows.push(gateRow(byStage.get("gate") ?? [], finished, now, run));
    else rows.push(simpleStageRow(stage, byStage.get(stage) ?? [], finished, now));
  }
  return rows;
}
