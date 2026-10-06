// Release decisions and the per-project kill switch: GET /runs/{id}/release
// and GET /projects/{project}/release. Merge and deploy stay human actions;
// these are the records the console displays, never a trigger.
import {
  type JsonObject,
  objectList,
  optBoolean,
  optObject,
  reqBoolean,
  reqObject,
  reqString,
  stringList,
} from "@/domain/decode";

/** internal/release.Decision: whether a run's change may be released. */
export interface ReleaseDecision {
  readonly runId: string;
  readonly project: string;
  readonly allowed: boolean;
  /** Omitted by the server (omitempty) when an allowed decision had nothing to report. */
  readonly reasons: readonly string[];
  readonly evaluatedAt: string;
}

function decodeReleaseDecision(o: JsonObject, at: string): ReleaseDecision {
  return {
    runId: reqString(o, "run_id", at),
    project: reqString(o, "project", at),
    allowed: reqBoolean(o, "allowed", at),
    reasons: stringList(o, "reasons", at),
    evaluatedAt: reqString(o, "evaluated_at", at),
  };
}

/** One attributable change to a project's kill switch: who flipped it, why, and when. */
export interface KillSwitchTransition {
  readonly engaged: boolean;
  readonly by: string;
  readonly reason: string;
  readonly at: string;
}

function decodeKillSwitchTransition(o: JsonObject, at: string): KillSwitchTransition {
  return {
    engaged: reqBoolean(o, "engaged", at),
    by: reqString(o, "by", at),
    reason: reqString(o, "reason", at),
    at: reqString(o, "at", at),
  };
}

/**
 * A project's durable kill-switch state plus its append-only transition
 * history. A project that has never been engaged reports engaged == false
 * with an empty history.
 */
export interface KillSwitchRecord {
  readonly project: string;
  readonly engaged: boolean;
  readonly history: readonly KillSwitchTransition[];
}

function decodeKillSwitchRecord(o: JsonObject, at: string): KillSwitchRecord {
  return {
    project: reqString(o, "project", at),
    engaged: optBoolean(o, "engaged", at),
    history: objectList(o, "history", at, decodeKillSwitchTransition),
  };
}

/**
 * The durable marker the factory leaves when it evaluated a run's release
 * decision but could not durably save it (internal/release.DecisionFailure).
 * A run in this state was evaluated, unlike one with no decision recorded at
 * all.
 */
export interface DecisionRecordingFailure {
  readonly error: string;
  readonly at: string;
  readonly killSwitchReadable: boolean;
}

function decodeDecisionRecordingFailure(o: JsonObject, at: string): DecisionRecordingFailure {
  return {
    error: reqString(o, "error", at),
    at: reqString(o, "at", at),
    killSwitchReadable: optBoolean(o, "kill_switch_readable", at),
  };
}

/**
 * GET /runs/{id}/release: one run's release decision and the project kill
 * switch it was evaluated against.
 */
export interface ReleaseView {
  readonly runId: string;
  readonly project: string;
  /**
   * Null for a run with no decision recorded: nothing records one until a run
   * is accepted. A screen must render that as "no decision", never as an
   * allowed one.
   */
  readonly decision: ReleaseDecision | null;
  /**
   * Set instead of `decision` when the run WAS evaluated but the decision
   * could not be durably recorded (e.g. an unreadable or corrupted
   * kill-switch.json). Distinct from `decision` null, which means recording
   * was never attempted.
   */
  readonly recordingFailure: DecisionRecordingFailure | null;
  readonly killSwitch: KillSwitchRecord;
}

export function decodeReleaseView(o: JsonObject, at: string): ReleaseView {
  return {
    runId: reqString(o, "run_id", at),
    project: reqString(o, "project", at),
    decision: optObject(o, "decision", at, decodeReleaseDecision),
    recordingFailure: optObject(o, "recording_failure", at, decodeDecisionRecordingFailure),
    killSwitch: reqObject(o, "kill_switch", at, decodeKillSwitchRecord),
  };
}

/**
 * GET /projects/{project}/release: one project's kill switch state and
 * history, addressed by project id alone rather than derived from a run. It
 * closes the gap ReleaseView alone left: a project whose kill switch is
 * engaged but which has no runs had no console surface at all.
 */
export interface ProjectReleaseView {
  readonly project: string;
  readonly killSwitch: KillSwitchRecord;
}

export function decodeProjectReleaseView(o: JsonObject, at: string): ProjectReleaseView {
  return {
    project: reqString(o, "project", at),
    killSwitch: reqObject(o, "kill_switch", at, decodeKillSwitchRecord),
  };
}
