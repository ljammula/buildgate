// Operational shapes: workspace hints, the console's own config, daemon and
// worker liveness, the health probe and the server's error body.
import type { StatusTone } from "@/domain/status";
import {
  type JsonObject,
  decodeList,
  numberOr,
  optBoolean,
  optString,
  reqString,
  stringOrNull,
} from "@/domain/decode";

/** internal/api.workspaceHintView: one entry of GET /workspaces. */
export interface WorkspaceHint {
  readonly workspace: string;
  readonly hasFactoryYml: boolean;
  readonly resolvedVerifyCommand: string;
  readonly verifyCommandSource: string;
}

function decodeWorkspaceHint(o: JsonObject, at: string): WorkspaceHint {
  return {
    workspace: optString(o, "workspace", at),
    hasFactoryYml: optBoolean(o, "has_factory_yml", at),
    resolvedVerifyCommand: optString(o, "resolved_verify_command", at),
    verifyCommandSource: optString(o, "verify_command_source", at),
  };
}

export function decodeWorkspaceHintList(value: unknown, at: string): WorkspaceHint[] {
  return decodeList(value, at, decodeWorkspaceHint);
}

/**
 * What the server says about the gate token the request for
 * /console-config.json carried:
 *
 *   off       none is needed here: the feature is off, or the console is on
 *             the machine itself
 *   required  reads and writes here need one, and the request carried none,
 *             or one that is invalid or expired
 *   accepted  the request carried a valid one
 */
export type GateState = "off" | "required" | "accepted";

/**
 * GET /console-config.json: whether this server accepts a write from this
 * console (from its own origin unauthenticated, or with the gate token the
 * request carried), where the gate token stands, and the Temporal UI base
 * URL when configured.
 */
export interface ConsoleConfig {
  readonly writesEnabled: boolean;
  /** "off" when the server omits it, as one that predates the gate token does. */
  readonly gate: GateState;
  /** Null when the server has no -temporal-ui-url. */
  readonly temporalUiUrl: string | null;
  /**
   * Non-null (and non-empty) exactly when the server's configured release policy can never
   * allow a release decision, so the board can explain why every PR is
   * silently withheld instead of leaving the operator to discover it after a
   * first accepted run produces nothing.
   */
  readonly releasePolicyWarning: string | null;
}

// A value this console does not know reads as "off", like a missing one.
// "required" replaces the whole app with the gate screen and forgets the
// stored token, so it is shown only when the server says exactly that. The
// server enforces the gate whatever is drawn here: reading a new value as
// "off" costs at most a 403 on each call, shown with its reason, while
// reading it as "required" would lock an operator out of a console that may
// well be working.
function decodeGateState(value: string): GateState {
  return value === "required" || value === "accepted" ? value : "off";
}

// An absent field is null; a present one is kept as sent, "" included.
export function decodeConsoleConfig(o: JsonObject, at: string): ConsoleConfig {
  return {
    writesEnabled: optBoolean(o, "writes_enabled", at),
    gate: decodeGateState(optString(o, "gate", at)),
    temporalUiUrl: stringOrNull(o, "temporal_ui_url", at),
    releasePolicyWarning: stringOrNull(o, "release_policy_warning", at),
  };
}

/**
 * internal/api.DaemonStatus: one entry of GET /daemons, a serve-managed
 * supervisor child. A best-effort operational signal, never gated on.
 */
export interface DaemonStatus {
  readonly repository: string;
  readonly state: string;
  /** 0 when the server omits it. */
  readonly pid: number;
  readonly startedAt: string;
  readonly heartbeatUpdatedAt: string;
}

function decodeDaemonStatus(o: JsonObject, at: string): DaemonStatus {
  return {
    repository: optString(o, "repository", at),
    state: optString(o, "state", at),
    pid: numberOr(o, "pid", at, 0),
    startedAt: optString(o, "started_at", at),
    heartbeatUpdatedAt: optString(o, "heartbeat_updated_at", at),
  };
}

export function decodeDaemonList(value: unknown, at: string): DaemonStatus[] {
  return decodeList(value, at, decodeDaemonStatus);
}

/**
 * GET /queue-run: whether a live `factoryd worker` is draining this data
 * directory, from the on-disk heartbeat file. Unlike GET /daemons (start-token
 * gated, and 404 whenever the server is not managing a daemon lifecycle, the
 * common setup), this route is read-token gated like every other read route
 * and always answers when the server supports it, so the board's "worker
 * isn't running" strip fires however the worker was started. `state` is
 * "absent" (no heartbeat file was ever written), "stale" (older than the
 * server's staleness threshold) or "alive".
 */
export interface QueueRunStatus {
  readonly state: string;
  readonly lastHeartbeat: string;
}

export function decodeQueueRunStatus(o: JsonObject, at: string): QueueRunStatus {
  return {
    state: optString(o, "state", at, "absent"),
    lastHeartbeat: optString(o, "last_heartbeat", at),
  };
}

/** The body of every non-2xx response: `{"error": message}` (api.writeError). */
export interface ApiErrorBody {
  readonly error: string;
}

export function decodeApiErrorBody(o: JsonObject, at: string): ApiErrorBody {
  return { error: reqString(o, "error", at) };
}

/** GET /healthz. */
export interface Health {
  readonly status: string;
}

export function decodeHealth(o: JsonObject, at: string): Health {
  return { status: reqString(o, "status", at) };
}

/** How a worker heartbeat state reads to an operator, and the tone it is drawn in. */
export interface WorkerLiveness {
  readonly label: "Running" | "Stale" | "Not running";
  readonly tone: StatusTone;
  readonly alive: boolean;
}

/**
 * GET /queue-run's `state` for display. Anything but "alive" and "stale"
 * (a state a newer server adds included) reads as not running: the card
 * then offers the command that starts one.
 */
export function workerLiveness(state: string): WorkerLiveness {
  if (state === "alive") return { label: "Running", tone: "success", alive: true };
  if (state === "stale") return { label: "Stale", tone: "warning", alive: false };
  return { label: "Not running", tone: "danger", alive: false };
}
