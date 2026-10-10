// A run: one build of one ticket, as GET /runs, GET /runs/{id} and the run
// event stream send it (internal/api's runView, which embeds run.Run).
// Only the fields the console reads are decoded; the rest are ignored.
import {
  type JsonObject,
  decodeList,
  numberOr,
  objectList,
  optBoolean,
  optNumber,
  optObject,
  optString,
  reqBoolean,
  reqNumber,
  reqString,
  stringList,
  stringListOrNull,
  stringOrNull,
  booleanOrNull,
} from "@/domain/decode";
import { type ModelUsage, decodeModelUsage, decodeUsage, usageTotalTokens } from "@/domain/usage";

export interface Attempt {
  readonly command: readonly string[];
  readonly startedAt: string;
  readonly finishedAt: string;
  /** build, verify, full_suite_verify, review, ...: empty on a run.json predating the field. */
  readonly kind: string;
  readonly exitCode: number;
  readonly logPath: string;
  /**
   * The session-config role ("execution"/"review") this attempt ran under:
   * "" for an attempt kind that never resolves a role (verify,
   * full_suite_verify) and for a run recorded before the field existed.
   */
  readonly role: string;
  /** The coding-agent CLI the attempt's job ran under: "" for a run recorded before the field. */
  readonly harness: string;
  /** The reasoning-effort level the attempt was TOLD to use. */
  readonly thinking: string;
  /**
   * What the harness's own thinking-level map actually turns `thinking`
   * into, which is what must be compared against `relayReasoningEffort` for
   * a clamp hint: a model whose map legitimately renames "max" to "xhigh"
   * must not read as a clamp just because `thinking` differs from
   * `relayReasoningEffort` (found via review: the false positive comparing
   * `thinking` directly produced).
   */
  readonly expectedEffort: string;
  readonly relayWorkerModelId: string;
  /**
   * What the relay actually observed on the wire, which can legitimately
   * differ from `expectedEffort` (a real silent clamp).
   */
  readonly relayReasoningEffort: string;
  readonly relayReasoningEffortAnomaly: boolean;
}

function decodeAttempt(o: JsonObject, at: string): Attempt {
  return {
    kind: optString(o, "kind", at),
    command: stringList(o, "command", at),
    startedAt: reqString(o, "started_at", at),
    finishedAt: reqString(o, "finished_at", at),
    exitCode: reqNumber(o, "exit_code", at),
    logPath: reqString(o, "log_path", at),
    role: optString(o, "role", at),
    harness: optString(o, "harness", at),
    thinking: optString(o, "thinking", at),
    expectedEffort: optString(o, "expected_effort", at),
    relayWorkerModelId: optString(o, "relay_worker_model_id", at),
    relayReasoningEffort: optString(o, "relay_reasoning_effort", at),
    relayReasoningEffortAnomaly: optBoolean(o, "relay_reasoning_effort_anomaly", at),
  };
}

/**
 * One line of a run's progress feed (`GET /runs/{id}/progress`), mirroring
 * progress-contract.md's line schema. `source` is "factory" (a pipeline
 * stage's own start/end, stamped by factoryd) or "worker" (relayed from the
 * sandbox's stdout: untrusted, display-only, never used for any gate or
 * state decision). `round`/`maxRounds` are 0 and `outcome`/`detail` are ""
 * when not applicable, matching the server's `omitempty`. A missing or
 * unparseable `ts` decodes as the epoch rather than throwing: a malformed or
 * half-written progress line must never crash the screen.
 */
export interface ProgressEvent {
  readonly ts: Date;
  readonly source: string;
  readonly stage: string;
  readonly event: string;
  readonly round: number;
  readonly maxRounds: number;
  readonly outcome: string;
  readonly detail: string;
}

export function decodeProgressEvent(o: JsonObject, at: string): ProgressEvent {
  const ts = new Date(optString(o, "ts", at));
  return {
    ts: Number.isNaN(ts.getTime()) ? new Date(0) : ts,
    source: optString(o, "source", at),
    stage: optString(o, "stage", at),
    event: optString(o, "event", at),
    round: Math.trunc(numberOr(o, "round", at, 0)),
    maxRounds: Math.trunc(numberOr(o, "max_rounds", at, 0)),
    outcome: optString(o, "outcome", at),
    detail: optString(o, "detail", at),
  };
}

/** One sidecar a phase launched: reachable by the worker at alias:port. */
export interface ComposeService {
  readonly name: string;
  readonly alias: string;
  readonly image: string;
  readonly port: number;
  readonly digest: string;
}

function decodeComposeService(o: JsonObject, at: string): ComposeService {
  return {
    name: optString(o, "name", at),
    alias: optString(o, "alias", at),
    image: optString(o, "image", at),
    port: numberOr(o, "port", at, 0),
    digest: optString(o, "digest", at),
  };
}

/** Where the worker reaches this service on the run's compose network. */
export function composeServiceAddress(service: ComposeService): string {
  return service.port === 0 ? service.alias : `${service.alias}:${service.port}`;
}

/**
 * What one phase launched from the target repo's compose file: the API's
 * compose_phases entry (the run's `compose/services.<phase>.json`).
 */
export interface ComposePhase {
  readonly phase: string;
  readonly enabled: boolean;
  readonly disabledReason: string;
  readonly services: readonly ComposeService[];
}

export function decodeComposePhase(o: JsonObject, at: string): ComposePhase {
  return {
    phase: optString(o, "phase", at),
    enabled: optBoolean(o, "enabled", at),
    disabledReason: optString(o, "disabled_reason", at),
    services: objectList(o, "services", at, decodeComposeService),
  };
}

/**
 * internal/run.GateBaseCheck: what rerunning a failed command gate on the
 * commit the ticket's work started from showed. It never changes the gate's
 * own result.
 */
export interface GateBaseCheck {
  /** fails_same, fails_differently, passes or not_checked; the server may add one. */
  readonly outcome: string;
  readonly baseSha: string;
  /** Why the rerun reached no exit code; set for not_checked. */
  readonly reason: string;
}

function decodeGateBaseCheck(o: JsonObject, at: string): GateBaseCheck {
  return {
    outcome: reqString(o, "outcome", at),
    baseSha: optString(o, "base_sha", at),
    reason: optString(o, "reason", at),
  };
}

export interface GateResult {
  readonly check: string;
  readonly command: readonly string[];
  readonly passed: boolean;
  readonly exitCode: number;
  readonly durationMs: number;
  readonly logSha256: string;
  /** Null for a gate that passed and for a check that is not a command gate. */
  readonly baseCheck: GateBaseCheck | null;
}

function decodeGateResult(o: JsonObject, at: string): GateResult {
  return {
    check: reqString(o, "check", at),
    command: stringList(o, "command", at),
    passed: reqBoolean(o, "passed", at),
    exitCode: reqNumber(o, "exit_code", at),
    durationMs: reqNumber(o, "duration_ms", at),
    logSha256: reqString(o, "log_sha256", at),
    baseCheck: optObject(o, "base_check", at, decodeGateBaseCheck),
  };
}

export interface DiffStat {
  readonly filesChanged: number;
  readonly insertions: number;
  readonly deletions: number;
}

function decodeDiffStat(o: JsonObject, at: string): DiffStat {
  return {
    filesChanged: reqNumber(o, "files_changed", at),
    insertions: reqNumber(o, "insertions", at),
    deletions: reqNumber(o, "deletions", at),
  };
}

/**
 * The verify command's run on the untouched base commit, before the build
 * (run.BaselineVerify). Counts and lists are 0 / empty when the server
 * omitted them.
 */
export interface BaselineVerify {
  readonly command: string;
  readonly baseSha: string;
  readonly exitCode: number;
  readonly passed: boolean;
  readonly failingTests: readonly string[];
  readonly failingCount: number;
  readonly unnamed: readonly string[];
  readonly unnamedCount: number;
  /** A path the ticket creates that the command needed; "" when that was not the failure. */
  readonly needsCreated: string;
  readonly firstError: string;
  readonly expected: boolean;
  /** Paths the command left that diff_scope would flag (at most 20), and how many there were. */
  readonly leftOutOfScope: readonly string[];
  readonly leftOutOfScopeCount: number;
  readonly inheritedFrom: string;
  readonly logPath: string;
  readonly durationMs: number;
}

function decodeBaselineVerify(o: JsonObject, at: string): BaselineVerify {
  return {
    command: reqString(o, "command", at),
    baseSha: optString(o, "base_sha", at),
    exitCode: reqNumber(o, "exit_code", at),
    passed: reqBoolean(o, "passed", at),
    failingTests: o.failing_tests == null ? [] : stringList(o, "failing_tests", at),
    failingCount: numberOr(o, "failing_count", at, 0),
    unnamed: o.unnamed == null ? [] : stringList(o, "unnamed", at),
    unnamedCount: numberOr(o, "unnamed_count", at, 0),
    needsCreated: optString(o, "needs_created", at),
    firstError: optString(o, "first_error", at),
    expected: optBoolean(o, "expected", at),
    leftOutOfScope: o.left_out_of_scope == null ? [] : stringList(o, "left_out_of_scope", at),
    leftOutOfScopeCount: numberOr(o, "left_out_of_scope_count", at, 0),
    inheritedFrom: optString(o, "inherited_from", at),
    logPath: optString(o, "log_path", at),
    durationMs: numberOr(o, "duration_ms", at, 0),
  };
}

/** One notification record of a run (run.NotificationRecord). */
export interface RunNotification {
  readonly runId: string;
  readonly ticket: string;
  readonly reason: string;
  readonly state: string;
  readonly sentAt: string;
}

function decodeRunNotification(o: JsonObject, at: string): RunNotification {
  return {
    runId: reqString(o, "run_id", at),
    ticket: reqString(o, "ticket", at),
    reason: reqString(o, "reason", at),
    state: reqString(o, "state", at),
    sentAt: reqString(o, "sent_at", at),
  };
}

export interface Override {
  readonly by: string;
  readonly reason: string;
  readonly at: string;
  readonly priorState: string;
  readonly newState: string;
}

function decodeOverride(o: JsonObject, at: string): Override {
  return {
    by: reqString(o, "by", at),
    reason: reqString(o, "reason", at),
    at: reqString(o, "at", at),
    priorState: reqString(o, "prior_state", at),
    newState: reqString(o, "new_state", at),
  };
}

/**
 * One build_app.py corrective round, from `run.AgentEvidenceRound`'s
 * BUILD_EVIDENCE.json fields. Agent-reported, informational only (see
 * AgentEvidence).
 */
export interface AgentEvidenceRound {
  readonly index: number;
  readonly agentReturnCode: number;
  readonly agentTimedOut: boolean;
  /** Null when verification never ran: distinct from a failed verification. */
  readonly verifyPassed: boolean | null;
  readonly verifyTimedOut: boolean;
  readonly fastCheckRan: boolean;
  readonly fastCheckPassed: boolean | null;
  readonly durationS: number;
  readonly tokens: number;
  /**
   * Why the round did not finish clean: empty for a round that passed, null
   * for a round recorded by a worker that did not report it.
   */
  readonly blockers: readonly string[] | null;
  /** The files the round's agent turn changed; null when not reported. */
  readonly changedFiles: readonly string[] | null;
  /** Two rounds with the same non-empty signature failed the same way. */
  readonly failureSignature: string;
  /** Where the failing command's whole output was saved, in the build workspace. */
  readonly failureLog: string;
  /** What happened to the agent process when no command failed. */
  readonly agentNotes: string;
}

// The per-round token figure: the same definition as usageTotalTokens.
function roundTokens(value: unknown): number {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return 0;
  return usageTotalTokens(decodeUsage(value as JsonObject)) ?? 0;
}

export function decodeAgentEvidenceRound(o: JsonObject, at: string): AgentEvidenceRound {
  return {
    index: Math.trunc(numberOr(o, "index", at, 0)),
    agentReturnCode: Math.trunc(numberOr(o, "agent_returncode", at, 0)),
    agentTimedOut: optBoolean(o, "agent_timed_out", at),
    verifyPassed: booleanOrNull(o, "verify_passed", at),
    verifyTimedOut: optBoolean(o, "verify_timed_out", at),
    fastCheckRan: optBoolean(o, "fast_check_ran", at),
    fastCheckPassed: booleanOrNull(o, "fast_check_passed", at),
    durationS: numberOr(o, "duration_s", at, 0),
    tokens: roundTokens(o.usage),
    blockers: stringListOrNull(o, "blockers", at),
    changedFiles: stringListOrNull(o, "changed_files", at),
    failureSignature: optString(o, "failure_signature", at),
    failureLog: optString(o, "failure_log", at),
    agentNotes: optString(o, "agent_notes", at),
  };
}

/**
 * Mirrors run.AgentEvidenceRound.Outcome (internal/run/run.go) exactly: the same
 * priority order build_app.py's own round_blockers applies (timeout, then the
 * agent invocation itself failing, then a fast check substituting for
 * verification, then verification itself), classified from only the fields
 * the round carries. A round that reported blockers while every such field
 * reads clean (it changed nothing) is "fail (blocked)"; an empty blocker list
 * never turns a failing field into a pass.
 */
export function agentEvidenceRoundOutcome(round: AgentEvidenceRound): string {
  const outcome = outcomeFromFields(round);
  return outcome === "pass" && (round.blockers?.length ?? 0) > 0 ? "fail (blocked)" : outcome;
}

function outcomeFromFields(round: AgentEvidenceRound): string {
  if (round.agentTimedOut || round.verifyTimedOut) return "fail (timed out)";
  if (round.agentReturnCode !== 0) return "fail (error)";
  if (round.fastCheckRan && round.fastCheckPassed === false) return "fail (verify)";
  if (round.verifyPassed !== null) return round.verifyPassed ? "pass" : "fail (verify)";
  return "fail (error)";
}

/**
 * `run.AgentEvidence`: best-effort, agent-reported build evidence. Rendered
 * for a human to read (the Build stage's per-round rows); never consulted for
 * any gate or state decision.
 */
export interface AgentEvidence {
  readonly rounds: readonly AgentEvidenceRound[];
}

function decodeAgentEvidence(o: JsonObject, at: string): AgentEvidence {
  return { rounds: objectList(o, "rounds", at, decodeAgentEvidenceRound) };
}

export interface Run {
  readonly id: string;
  readonly ticket: string;
  readonly projectPath: string;
  readonly workspacePath: string;
  readonly specPath: string;
  readonly specSha256: string;
  readonly state: string;
  /**
   * Why a halted or quarantined run stopped, in the server's words
   * (run.Run.HaltError). Empty for a run that has not stopped that way. It
   * can be a long chain of wrapped errors: runStopCause digests it.
   */
  readonly haltError: string;
  /** The machine code of the stop (run.Run.HaltReasonCode), e.g. "compose_services_rejected". */
  readonly haltReasonCode: string;
  /** The factory's own triage sentence for a stopped run; empty when none was written. */
  readonly triage: string;
  /** Non-empty when the run left a handoff: GET /runs/{id}/handoff has it. */
  readonly handoffSha256: string;
  /**
   * Mirrors run.Run.HaltConfirmed; see runIsTerminal for why a "halted"
   * state alone is not enough to know a run is truly done. Absent on a
   * run.json predating the field, with the same conservative default as the
   * Go zero value: an old or unknown record is never a confirmed halt.
   */
  readonly haltConfirmed: boolean;
  readonly baseSha: string;
  readonly resultSha: string | null;
  readonly committedByFactoryd: boolean;
  /** Null when the server sent none: distinct from an empty list. */
  readonly changedFiles: readonly string[] | null;
  readonly diffStat: DiffStat | null;
  /** Null on a run recorded before the check existed, or not yet at it. */
  readonly baselineVerify: BaselineVerify | null;
  /**
   * Mirrors run.Run.DiffAvailable: whether a diff snapshot exists to fetch
   * (GET /runs/{id}/diff). False for a run predating the field, or one
   * whose evidence collection warned-and-continued on the diff step; a
   * "View diff" action should only be shown when this is true, not merely
   * when resultSha is set (found via review: a run with no snapshot but a
   * resultSha could otherwise show a button that only ever opens an error
   * screen).
   */
  readonly diffAvailable: boolean;
  readonly diffTruncated: boolean;
  readonly attempts: readonly Attempt[];
  readonly composePhases: readonly ComposePhase[];
  readonly gateResults: readonly GateResult[];
  readonly notifications: readonly RunNotification[];
  readonly overrides: readonly Override[];
  /**
   * Null when build_app.py predates BUILD_EVIDENCE.json, or factoryd could
   * not read or parse it: mirrors run.Run.AgentEvidence's nil pointer
   * meaning "not collected", distinct from evidence with no rounds.
   */
  readonly agentEvidence: AgentEvidence | null;
  readonly createdAt: string;
  readonly updatedAt: string;
  // The next five mirror internal/api's runView (progress-contract.md's
  // "silence is a bug" addition): computed at read time from the run's
  // progress feed, empty/zero for a terminal run since a finished run's feed
  // is never re-read. Best-effort on the server, so all are optional.
  readonly lastProgressAt: string | null;
  readonly currentStage: string | null;
  readonly currentRound: number;
  readonly maxRounds: number;
  readonly waitingReason: string | null;
  /**
   * The server-computed "silence is a bug" verdict (internal/progress.
   * Stalled), the single shared rule `factoryd status`, `factoryd watch` and
   * this console all delegate to, so they cannot disagree. False/null for a
   * terminal run or a server predating the field.
   */
  readonly stalled: boolean;
  readonly stalledSinceSeconds: number | null;
  /** The request this run was started for: "" for a directly started run or one predating the field. */
  readonly requestId: string;
  readonly temporalWorkflowId: string;
  /**
   * Server-computed per-model token breakdown across this run's own
   * attempts (runView.ByModel): the run detail's "model id and tokens spent"
   * line. Empty when nothing recorded a model id, or on a server predating
   * the field.
   */
  readonly byModel: readonly ModelUsage[];
  /**
   * False when `byModel` is a lower bound (runView.TokensComplete): a
   * crash-recovered relay spend, or an evidence round with no usable token
   * figure. Absent on an older server decodes as true.
   */
  readonly tokensComplete: boolean;
  /**
   * The -reference-oracle-dir this run was launched with: "" for a run with
   * no oracle stage or one predating the field. Hides the commit_oracles and
   * post_oracle_commit_verify timeline rows when there is no oracle stage to
   * report on.
   */
  readonly referenceOracleDir: string;
}

export function decodeRun(o: JsonObject, at: string): Run {
  return {
    id: reqString(o, "id", at),
    ticket: reqString(o, "ticket", at),
    projectPath: reqString(o, "project_path", at),
    workspacePath: reqString(o, "workspace_path", at),
    specPath: reqString(o, "spec_path", at),
    specSha256: reqString(o, "spec_sha256", at),
    state: reqString(o, "state", at),
    haltError: optString(o, "halt_error", at),
    haltReasonCode: optString(o, "halt_reason_code", at),
    triage: optString(o, "triage", at),
    handoffSha256: optString(o, "handoff_sha256", at),
    haltConfirmed: optBoolean(o, "halt_confirmed", at),
    baseSha: reqString(o, "base_sha", at),
    resultSha: stringOrNull(o, "result_sha", at),
    committedByFactoryd: reqBoolean(o, "committed_by_factoryd", at),
    changedFiles: o.changed_files == null ? null : stringList(o, "changed_files", at),
    diffStat: optObject(o, "diff_stat", at, decodeDiffStat),
    baselineVerify: optObject(o, "baseline_verify", at, decodeBaselineVerify),
    diffAvailable: optBoolean(o, "diff_available", at),
    diffTruncated: optBoolean(o, "diff_truncated", at),
    attempts: objectList(o, "attempts", at, decodeAttempt),
    gateResults: objectList(o, "gate_results", at, decodeGateResult),
    notifications: objectList(o, "notifications", at, decodeRunNotification),
    overrides: objectList(o, "overrides", at, decodeOverride),
    agentEvidence: optObject(o, "agent_evidence", at, decodeAgentEvidence),
    createdAt: reqString(o, "created_at", at),
    updatedAt: reqString(o, "updated_at", at),
    lastProgressAt: stringOrNull(o, "last_progress_at", at),
    currentStage: stringOrNull(o, "current_stage", at),
    currentRound: numberOr(o, "current_round", at, 0),
    maxRounds: numberOr(o, "max_rounds", at, 0),
    waitingReason: stringOrNull(o, "waiting_reason", at),
    stalled: optBoolean(o, "stalled", at),
    stalledSinceSeconds: optNumber(o, "stalled_since_seconds", at),
    requestId: optString(o, "request_id", at),
    temporalWorkflowId: optString(o, "temporal_workflow_id", at),
    byModel: objectList(o, "by_model", at, decodeModelUsage),
    tokensComplete: optBoolean(o, "tokens_complete", at, true),
    referenceOracleDir: optString(o, "reference_oracle_dir", at),
    composePhases: objectList(o, "compose_phases", at, decodeComposePhase),
  };
}

/**
 * 'quarantined' is deliberately excluded (found via review): an operator
 * override (POST /runs/{id}/override) can still move a quarantined run to
 * 'accepted' or 'halted' at any later time, so a screen that loaded while
 * (or after) a run was already quarantined must keep watching for that, not
 * treat it as final. 'accepted' is unconditionally final: nothing ever moves
 * a run out of it.
 *
 * 'halted' additionally requires haltConfirmed (found via review, mirroring
 * internal/api's own terminal() fix): a run can be durably recorded 'halted'
 * before its real outcome is positively known (a Temporal give-up path
 * records a halt locally without confirmation the underlying execution
 * actually stopped, precisely so a daemon's reclaim scan keeps polling for
 * what really happened). A screen that stopped watching on that unconfirmed
 * halt would never learn its later reconciliation to a different terminal
 * state.
 */
export function runIsTerminal(run: Run): boolean {
  return run.state === "accepted" || (run.state === "halted" && run.haltConfirmed);
}

/**
 * runIsTerminal plus 'quarantined': for display purposes only (an
 * elapsed/"last activity" calculation, never a decision to stop
 * polling/watching). Found via a console operator walkthrough: a quarantined
 * run's progress feed has already gone silent (nothing more will arrive), so
 * treating it as still "in flight" for elapsed/last-activity purposes
 * over-reported a growing stall/elapsed figure that `factoryd status` itself
 * never showed. runIsTerminal itself stays unchanged: see its doc comment
 * for why an operator override can still move a quarantined run onward,
 * which a screen must keep watching for.
 */
export function runIsTerminalForDisplay(run: Run): boolean {
  return runIsTerminal(run) || run.state === "quarantined";
}

export function decodeRunList(value: unknown, at: string): Run[] {
  return decodeList(value, at, decodeRun);
}

/**
 * GET /runs/{id}/diff's response: both the diff text and whether it was cut
 * short by the server's own storage cap. Found via review: a client that
 * only kept the diff silently presented a truncated diff as if it were the
 * complete change, with no way to warn an operator that it was not.
 */
export interface RunDiff {
  readonly diff: string;
  readonly truncated: boolean;
}

export function decodeRunDiff(o: JsonObject, at: string): RunDiff {
  return {
    diff: optString(o, "diff", at),
    truncated: optBoolean(o, "truncated", at),
  };
}
