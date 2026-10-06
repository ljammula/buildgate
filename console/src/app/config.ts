import type { HttpConfig } from "@/api/http";

// Build-time settings (Vite env). A token set here is baked into the bundle, so it is for a
// console built for one deployment; the server never serves one at runtime.
const env = import.meta.env;

const text = (value: unknown): string => (typeof value === "string" ? value : "");

/**
 * The HTTP configuration for this bundle. `storedStartToken` is the token
 * `factoryd serve` handed over in the URL fragment (platform/startToken).
 *
 * A production build talks to its own origin unless VITE_API_BASE_URL says
 * otherwise; the dev server defaults to a local factoryd.
 */
export function httpConfigFromEnv(storedStartToken: string | null): HttpConfig {
  const auth = text(env.VITE_API_AUTH_TOKEN);
  const start = text(env.VITE_API_START_TOKEN);
  const override = text(env.VITE_API_OVERRIDE_TOKEN);
  const read = text(env.VITE_API_READ_TOKEN);
  const baseUrl =
    typeof env.VITE_API_BASE_URL === "string"
      ? env.VITE_API_BASE_URL
      : env.DEV
        ? "http://localhost:8090"
        : "";
  return resolveHttpConfig({ baseUrl, auth, start, override, read, storedStartToken });
}

export interface TokenInputs {
  readonly baseUrl: string;
  readonly auth: string;
  readonly start: string;
  readonly override: string;
  readonly read: string;
  readonly storedStartToken: string | null;
}

/**
 * Which token each kind of route gets. The start and override tokens are
 * separate credentials on the server and may be distinct; one auth token
 * stands in for whichever is not set. The stored start token is used only
 * when the bundle carries neither a start nor an auth token.
 */
export function resolveHttpConfig(input: TokenInputs): HttpConfig {
  const orNull = (value: string): string | null => (value === "" ? null : value);
  return {
    baseUrl: input.baseUrl,
    readToken: orNull(input.read),
    startToken: orNull(input.start) ?? orNull(input.auth) ?? input.storedStartToken,
    overrideToken: orNull(input.override) ?? orNull(input.auth),
  };
}
