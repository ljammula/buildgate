// What the console starts with: the Http every screen uses, the server's
// console config, and where the gate token stands.
//
// The gate token (platform/gateToken) lets a console opened through a proxy
// read and do request writes. The console token (platform/consoleToken) does
// the same for the console on the host once serve requires a token from
// every caller; the server answers it as an accepted gate token, so the rest
// of the console tells the two apart nowhere but here. A gate token this tab
// holds comes first. The server answers a refused one with a plain
// 403, the same as a start-token or run-override route answers a console
// that holds the gate token alone, so a 403 never decides anything here:
// GET /console-config.json does, at startup and again after any 403.
import { type Http, type HttpConfig, createHttp } from "@/api/http";
import { fetchConsoleConfig } from "@/api/ops";
import type { ConsoleConfig } from "@/domain/ops";
import {
  clearStoredConsoleToken,
  getStoredConsoleToken,
  setStoredConsoleToken,
} from "@/platform/consoleToken";
import { clearStoredGateToken, getStoredGateToken, setStoredGateToken } from "@/platform/gateToken";

export interface ConsoleSession {
  readonly http: Http;
  /** GET /console-config.json as answered to the gate token `http` carries. */
  readonly config: ConsoleConfig;
  /** A gate token stored in this tab was refused at startup, and forgotten. */
  readonly storedTokenRefused: boolean;
  /** Whether the server stopped accepting the gate token since startup. */
  readonly gateLost: () => boolean;
  /** Calls `listener` when gateLost turns true. Returns the unsubscribe. */
  readonly subscribe: (listener: () => void) => () => void;
  /**
   * Offers a gate token the operator pasted. It is stored for this tab only
   * once the server has accepted it; the caller then starts the console
   * again, which picks it up. Never throws, and stores nothing unless it
   * resolves "accepted".
   */
  readonly offerToken: (token: string) => Promise<TokenOffer>;
}

/**
 * What became of a pasted gate token: the server accepted it and it is
 * stored; the server refused it; the server did not answer as a gated
 * console (unreachable, or its gate was turned off meanwhile), which says
 * nothing about the token; or this browser would not keep it for the tab.
 */
export type TokenOffer = "accepted" | "refused" | "no-answer" | "not-kept";

export interface SessionInputs {
  /** The gate token the address bar carried (captureGateTokenFromLocation), not yet trusted. */
  readonly candidate: string | null;
  /** The console token the address bar carried (captureConsoleTokenFromLocation), not yet trusted. */
  readonly consoleCandidate?: string | null;
  /** This bundle's HttpConfig around a gate token (app/config). */
  readonly httpConfig: (gateToken: string | null) => HttpConfig;
}

/**
 * Settles which gate token this tab uses and asks the server what it makes
 * of it. A token from the address bar is stored only once the server has
 * accepted it: a stale or mistyped link must not replace a stored token that
 * works. Never throws (fetchConsoleConfig is tolerant).
 */
export async function startSession({
  candidate,
  consoleCandidate = null,
  httpConfig,
}: SessionInputs): Promise<ConsoleSession> {
  const configFor = (token: string | null) => fetchConsoleConfig(createHttp(httpConfig(token)));

  let gateToken = getStoredGateToken();
  let config: ConsoleConfig | null = null;
  if (candidate !== null && candidate !== gateToken) {
    const answer = await configFor(candidate);
    if (answer.gate === "accepted") {
      setStoredGateToken(candidate);
      gateToken = candidate;
      config = answer;
    } else if (gateToken === null) {
      // Nothing to fall back on: a refused token is answered as no token is.
      config = answer;
    }
  }
  let consoleToken = getStoredConsoleToken();
  if (consoleCandidate !== null && consoleCandidate !== consoleToken) {
    const answer = await configFor(consoleCandidate);
    if (answer.gate === "accepted") {
      setStoredConsoleToken(consoleCandidate);
      consoleToken = consoleCandidate;
      if (gateToken === null) config = answer;
    }
  }
  // A refused gate-token link was answered as no token; the console token may do better.
  if (gateToken === null && consoleToken !== null && config?.gate !== "accepted") config = null;

  // Which store the token this session sends came from: a refusal clears
  // that store and no other, since the console token is shared by every tab.
  let source: "gate" | "console" | null =
    gateToken !== null ? "gate" : consoleToken !== null ? "console" : null;
  const tokenFrom = (from: typeof source) =>
    from === "gate" ? gateToken : from === "console" ? consoleToken : null;
  config ??= await configFor(tokenFrom(source));
  if (config.gate === "required" && source === "gate" && consoleToken !== null) {
    // The tab's gate token went stale; the host's console token is still here.
    clearStoredGateToken();
    source = "console";
    config = await configFor(consoleToken);
  }

  let storedTokenRefused = false;
  if (config.gate === "required" && source !== null) {
    if (source === "gate") clearStoredGateToken();
    else clearStoredConsoleToken();
    storedTokenRefused = true;
    source = null;
  }
  const token = tokenFrom(source);
  const sent = source;

  let lost = false;
  let checking = false;
  const listeners = new Set<() => void>();
  // One re-check at a time: a screen's reads fail together when a token is
  // rotated, and each of them lands here.
  const recheck = () => {
    if (lost || checking) return;
    checking = true;
    void fetchConsoleConfig(http).then((answer) => {
      checking = false;
      // Anything but "required" (a server that could not answer included)
      // leaves the token and the app alone.
      if (answer.gate !== "required") return;
      if (sent === "gate") clearStoredGateToken();
      else if (sent === "console") clearStoredConsoleToken();
      lost = true;
      for (const listener of listeners) listener();
    });
  };
  const http = createHttp(httpConfig(token), { onForbidden: recheck });

  return {
    http,
    config,
    storedTokenRefused,
    offerToken: async (offered) => {
      const token = offered.trim();
      if (token === "") return "refused";
      const answer = (await configFor(token)).gate;
      if (answer === "off") return "no-answer";
      if (answer !== "accepted") return "refused";
      setStoredGateToken(token);
      // Storage that is blocked drops the write without a word; starting
      // again would then show this screen once more, with no reason.
      return getStoredGateToken() === token ? "accepted" : "not-kept";
    },
    gateLost: () => lost,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}
