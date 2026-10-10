import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiProvider, useApi } from "@/api/ApiProvider";
import { createHttp } from "@/api/http";
import type { ConsoleConfig } from "@/domain/ops";

const fetch = (() => Promise.resolve(new Response("[]"))) as typeof globalThis.fetch;
const config: ConsoleConfig = {
  writesEnabled: false,
  gate: "off",
  temporalUiUrl: null,
  releasePolicyWarning: null,
};

function apiFor(
  overrideToken: string | null,
  gateToken: string | null = null,
  served: ConsoleConfig = config,
) {
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken,
    gateToken,
    fetch,
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ApiProvider http={http} config={served}>
      {children}
    </ApiProvider>
  );
  return renderHook(() => useApi(), { wrapper }).result.current;
}

test("hasOverrideToken reflects whether an override token was configured", () => {
  expect(apiFor(null).hasOverrideToken).toBe(false);
  expect(apiFor("").hasOverrideToken).toBe(false);
  expect(apiFor("override-secret").hasOverrideToken).toBe(true);
});

test("an accepted gate token offers writes, as the server says, but is no override token", () => {
  const api = apiFor(null, "gate-secret", { ...config, writesEnabled: true, gate: "accepted" });
  expect(api.canWrite).toBe(true);
  expect(api.hasOverrideToken).toBe(false);
});

test("a gate token alone offers no write: only the server's answer or an override token does", () => {
  expect(apiFor(null, "gate-secret").canWrite).toBe(false);
  expect(apiFor("override-secret").canWrite).toBe(true);
});
