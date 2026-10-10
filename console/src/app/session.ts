// What the console starts with: the Http every screen uses, the server's
// console config, and where the gate token stands.
//
// The gate token (platform/gateToken) lets a console opened through a proxy
// read and do request writes. The server answers a refused one with a plain
// 403, the same as a start-token or run-override route answers a console
// that holds the gate token alone, so a 403 never decides anything here:
// GET /console-config.json does, at startup and again after any 403.
import { type Http, type HttpConfig, createHttp } from "@/api/http";
import { fetchConsoleConfig } from "@/api/ops";
import type { ConsoleConfig } from "@/domain/ops";
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
}

export interface SessionInputs {
  /** The gate token the address bar carried (captureGateTokenFromLocation), not yet trusted. */
  readonly candidate: string | null;
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
  config ??= await configFor(gateToken);

  let storedTokenRefused = false;
  if (config.gate === "required" && gateToken !== null) {
    clearStoredGateToken();
    storedTokenRefused = true;
    gateToken = null;
  }

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
      clearStoredGateToken();
      lost = true;
      for (const listener of listeners) listener();
    });
  };
  const http = createHttp(httpConfig(gateToken), { onForbidden: recheck });

  return {
    http,
    config,
    storedTokenRefused,
    gateLost: () => lost,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}
