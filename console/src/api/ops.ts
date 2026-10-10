import { type Http, createHttp } from "@/api/http";
import { asObject } from "@/domain/decode";
import {
  type ConsoleConfig,
  type QueueRunStatus,
  type WorkspaceHint,
  decodeConsoleConfig,
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
 * GET /queue-run: whether `factoryd worker` is alive against this data dir.
 * Read-token gated and answering regardless of whether `factoryd serve` itself
 * manages the daemon lifecycle; see QueueRunStatus for why this is a separate route.
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
