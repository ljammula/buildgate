// The harness screen tests render with: the real providers and router, and a
// fake `fetch` answering from a route list, so a test states exactly what
// the server says and can assert exactly what the console sent.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";

import { ApiProvider } from "@/api/ApiProvider";
import { type HttpConfig, createHttp } from "@/api/http";
import type { ConsoleConfig } from "@/domain/ops";
import { readFixtureText } from "@/test/fixtures";
import { TooltipProvider } from "@/ui/Tooltip";

export interface RecordedRequest {
  readonly method: string;
  /** Path and query, e.g. "/requests/req-1/approve". */
  readonly url: string;
  readonly headers: Readonly<Record<string, string>>;
  /** The parsed JSON body, or null when the request had none. */
  readonly body: unknown;
}

export type FakeResponse = Response | (() => Response | Promise<Response>);

export interface FakeRoute {
  /** "GET /requests" or "POST /requests/req-1/approve"; the URL is matched exactly, query included. */
  readonly on: string;
  readonly reply: FakeResponse;
}

/** A JSON response. */
export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** The server's error shape: `{"error": message}`. */
export function apiErrorResponse(status: number, message: string): Response {
  return json({ error: message }, status);
}

/** A committed contract fixture (console/test/fixtures/api/<name>) as a response. */
export function fixtureResponse(name: string, status = 200): () => Response {
  return () => new Response(readFixtureText(`api/${name}`), { status });
}

/** An event stream that sends `events` (each a JSON-serializable value) and then stays open. */
export function sseResponse(eventName: string, events: readonly unknown[] = []): () => Response {
  return () =>
    new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          const encoder = new TextEncoder();
          for (const event of events) {
            controller.enqueue(
              encoder.encode(`event: ${eventName}\ndata: ${JSON.stringify(event)}\n\n`),
            );
          }
        },
      }),
    );
}

export interface FakeServer {
  readonly fetch: typeof globalThis.fetch;
  /** Every request made so far, in order. */
  readonly requests: RecordedRequest[];
  /** Replace or add a route while a test runs (e.g. make the next refresh fail). */
  readonly set: (on: string, reply: FakeResponse) => void;
  /** The requests made to one route, e.g. `sent("POST /requests/req-1/approve")`. */
  readonly sent: (on: string) => RecordedRequest[];
}

/**
 * A fake server. A request with no route gets a 404 in the server's error
 * shape, so a screen that calls something unexpected fails visibly.
 */
export function fakeServer(routes: readonly FakeRoute[] = []): FakeServer {
  const table = new Map<string, FakeResponse>(routes.map((route) => [route.on, route.reply]));
  const requests: RecordedRequest[] = [];
  const fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : input.toString();
    const method = init?.method ?? "GET";
    const body = typeof init?.body === "string" ? (JSON.parse(init.body) as unknown) : null;
    requests.push({
      method,
      url,
      headers: { ...(init?.headers as Record<string, string> | undefined) },
      body,
    });
    const reply = table.get(`${method} ${url}`);
    if (!reply) return apiErrorResponse(404, `no fake route for ${method} ${url}`);
    const response = typeof reply === "function" ? await reply() : reply;
    // A Response body can be read once; a route may be hit many times.
    return response.bodyUsed ? response : response.clone();
  }) as typeof globalThis.fetch;
  return {
    fetch,
    requests,
    set: (on, reply) => {
      table.set(on, reply);
    },
    sent: (on) => requests.filter((request) => `${request.method} ${request.url}` === on),
  };
}

export interface RenderAppOptions {
  /** The fake server, or its routes. */
  readonly server?: FakeServer | readonly FakeRoute[];
  /** Where the router starts, e.g. "/requests/req-1". Default "/". */
  readonly path?: string;
  /** The route pattern the element is mounted at, e.g. "/requests/:id". Default "*". */
  readonly pattern?: string;
  /** GET /console-config.json's answer. Default: writes enabled. */
  readonly config?: Partial<ConsoleConfig>;
  /** Tokens, to assert which one a route sends. Default: none configured. */
  readonly tokens?: Partial<
    Pick<HttpConfig, "readToken" | "startToken" | "overrideToken" | "gateToken">
  >;
}

export interface RenderAppResult extends RenderResult {
  readonly server: FakeServer;
  readonly queryClient: QueryClient;
  /** The router's current path and query, to assert a navigation. */
  readonly location: () => string;
}

/**
 * Renders a screen (or any component) inside the providers the app gives
 * it. Other paths render a marker, `Navigated to <path>`, so a test can
 * assert where a link or a redirect went.
 */
export function renderApp(ui: ReactElement, options: RenderAppOptions = {}): RenderAppResult {
  const server =
    options.server && "fetch" in options.server ? options.server : fakeServer(options.server ?? []);
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    gateToken: null,
    ...options.tokens,
    fetch: server.fetch,
  });
  const config: ConsoleConfig = {
    writesEnabled: true,
    gate: "off",
    temporalUiUrl: null,
    releasePolicyWarning: null,
    ...options.config,
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  let current = options.path ?? "/";
  function Track({ children }: { children: ReactNode }) {
    const location = useLocation();
    current = location.pathname + location.search;
    return children;
  }
  function Elsewhere() {
    const location = useLocation();
    return <p>Navigated to {location.pathname + location.search}</p>;
  }
  const result = render(
    <QueryClientProvider client={queryClient}>
      <ApiProvider http={http} config={config}>
        <TooltipProvider>
          <MemoryRouter initialEntries={[options.path ?? "/"]}>
            <Track>
              <Routes>
                <Route path={options.pattern ?? "*"} element={ui} />
                {options.pattern ? <Route path="*" element={<Elsewhere />} /> : null}
              </Routes>
            </Track>
          </MemoryRouter>
        </TooltipProvider>
      </ApiProvider>
    </QueryClientProvider>,
  );
  return { ...result, server, queryClient, location: () => current };
}
