import { type ReactNode, createContext, useContext, useMemo } from "react";

import type { Http } from "@/api/http";
import type { ConsoleConfig } from "@/domain/ops";

/** What every screen needs to talk to the server and to decide what to offer. */
export interface Api {
  readonly http: Http;
  /** GET /console-config.json, fetched once at startup. */
  readonly config: ConsoleConfig;
  /** Whether an override token is configured in this bundle. */
  readonly hasOverrideToken: boolean;
  /**
   * Whether to offer a write action at all: the server accepts an
   * unauthenticated write from this origin (its loopback Host/Origin check),
   * or an override token is configured. A write control is never disabled
   * only because no token is configured when the server would still accept
   * the request: a refused write is shown as the server's 403 and its
   * reason, not as a greyed-out button with no explanation.
   */
  readonly canWrite: boolean;
}

const ApiContext = createContext<Api | null>(null);

export interface ApiProviderProps {
  readonly http: Http;
  readonly config: ConsoleConfig;
  readonly children: ReactNode;
}

export function ApiProvider({ http, config, children }: ApiProviderProps) {
  const api = useMemo<Api>(() => {
    const hasOverrideToken = (http.config.overrideToken ?? "") !== "";
    return { http, config, hasOverrideToken, canWrite: config.writesEnabled || hasOverrideToken };
  }, [http, config]);
  return <ApiContext value={api}>{children}</ApiContext>;
}

export function useApi(): Api {
  const api = useContext(ApiContext);
  if (!api) throw new Error("useApi needs an <ApiProvider> above it");
  return api;
}
