import { type Http, createHttp } from "@/api/http";
import { asObject } from "@/domain/decode";
import {
  type ConsoleConfig,
  type DaemonStatus,
  type QueueRunStatus,
  type WorkspaceHint,
  decodeConsoleConfig,
  decodeDaemonList,
  decodeQueueRunStatus,
  decodeWorkspaceHintList,
} from "@/domain/ops";

const writesDisabled: ConsoleConfig = {
  writesEnabled: false,
  gate: "off",
  temporalUiUrl: null,
  releasePolicyWarning: null,
};

/**
 * GET /console-config.json, fetched at startup and again after a 403
 * (app/session.ts): whether this server will accept a write from this
 * console, where its gate token stands, and the Temporal UI base URL if
 * configured. Tolerant of a server predating this route (a 404, a
 * connection failure or a body that does not decode reads as "writes not
 * enabled, no gate, no Temporal UI"), so an older factoryd binary still
 * serves a read-only console rather than failing to start. Never throws.
 *
 * The route is public. It is sent the gate token and no other credential:
 * the server's answer (`gate`, `writes_enabled`) is about the token the
 * request carried, and no other token has a say in it.
 */
export async function fetchConsoleConfig(http: Http, signal?: AbortSignal): Promise<ConsoleConfig> {
  const bare = createHttp({
    ...http.config,
    readToken: null,
    startToken: null,
    overrideToken: null,
  });
  try {
    // With the read and override tokens gone, kind "gate" is the gate token or nothing.
    const json = await bare.getJson("/console-config.json", "gate", signal);
    return decodeConsoleConfig(
      asObject(json, "GET /console-config.json"),
      "GET /console-config.json",
    );
  } catch {
    return writesDisabled;
  }
}

/**
 * GET /daemons: whether `worker` (and any other companion daemon) is alive,
 * so the board can warn an operator that a request stuck in a working state
 * simply has nothing driving it forward, rather than looking like ordinary
 * progress. Start-token gated on the server (internal/api authorizeStart,
 * like the daemon lifecycle routes): sending the read token here left the
 * board's worker strip permanently hidden behind a 403 (found live,
 * 2026-09-24).
 */
export async function listDaemons(http: Http, signal?: AbortSignal): Promise<DaemonStatus[]> {
  return decodeDaemonList(await http.getJson("/daemons", "start", signal), "GET /daemons");
}

/**
 * GET /queue-run: whether `factoryd worker` is alive against this data dir.
 * Read-token gated (unlike listDaemons's start-token gated GET /daemons) and
 * answering regardless of whether `factoryd serve` itself manages the daemon
 * lifecycle; see QueueRunStatus for why this is a separate route.
 */
export async function getQueueRunStatus(http: Http, signal?: AbortSignal): Promise<QueueRunStatus> {
  const at = "GET /queue-run";
  return decodeQueueRunStatus(asObject(await http.getJson("/queue-run", "read", signal), at), at);
}

/**
 * GET /workspaces: every workspace POST /requests would currently accept,
 * each with the "New request" form's own resolved-verify-command hint. Read
 * token.
 */
export async function listWorkspaces(http: Http, signal?: AbortSignal): Promise<WorkspaceHint[]> {
  return decodeWorkspaceHintList(
    await http.getJson("/workspaces", "read", signal),
    "GET /workspaces",
  );
}
