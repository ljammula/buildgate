import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { ApiProvider } from "@/api/ApiProvider";
import { createHttp } from "@/api/http";

// A fake server for hook tests: routes match on the request URL, every call is recorded.
export interface Route {
  readonly match: (url: string, init: RequestInit) => boolean;
  readonly respond: () => Response | Promise<Response>;
}

export function harness(routes: Route[]) {
  const calls: { url: string; init: RequestInit }[] = [];
  const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : input.toString();
    calls.push({ url, init: init ?? {} });
    const route = routes.find((r) => r.match(url, init ?? {}));
    if (!route) return Promise.resolve(new Response('{"error": "no route"}', { status: 404 }));
    return Promise.resolve(route.respond());
  }) as typeof globalThis.fetch;
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    fetch,
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ApiProvider
        http={http}
        config={{ writesEnabled: true, temporalUiUrl: null, releasePolicyWarning: null }}
      >
        {children}
      </ApiProvider>
    </QueryClientProvider>
  );
  return { wrapper, client, calls };
}

export const hang = () => new Response(new ReadableStream<Uint8Array>({ start: () => undefined }));
