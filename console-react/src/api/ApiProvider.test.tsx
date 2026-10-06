import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiProvider, useApi } from "@/api/ApiProvider";
import { createHttp } from "@/api/http";

const fetch = (() => Promise.resolve(new Response("[]"))) as typeof globalThis.fetch;
const config = { writesEnabled: false, temporalUiUrl: null, releasePolicyWarning: null };

function apiFor(overrideToken: string | null) {
  const http = createHttp({ baseUrl: "", readToken: null, startToken: null, overrideToken, fetch });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ApiProvider http={http} config={config}>
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
