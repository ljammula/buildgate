// Test fixtures for the oracle panels: a fake factoryd serving the oracle
// routes from mutable file maps, and request summaries to render them with.
import { within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { decodeRequestSummary, type RequestSummary } from "@/domain/request";
import { sha256HexBytes } from "@/domain/contentHash";
import { type FakeRoute, type FakeServer, apiErrorResponse, fakeServer, json } from "@/test/render";

/** A response whose body is exactly `bytes`. */
export function bytesResponse(bytes: Uint8Array | undefined): Response {
  return new Response(new Uint8Array(bytes ?? []));
}

export const enc = (text: string): Uint8Array => new TextEncoder().encode(text);

export function requestSummary(
  overrides: Record<string, unknown> = {},
  tickets: readonly { index: number; specPath: string }[] = [],
): RequestSummary {
  return decodeRequestSummary(
    {
      id: "req-1",
      workspace: "w",
      project: "p",
      state: "oracle_review",
      submitted_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      title: "T",
      tickets: tickets.map((t) => ({ index: t.index, spec_path: t.specPath })),
      ...overrides,
    },
    "test",
  );
}

export interface OracleServer {
  readonly server: FakeServer;
  /** name -> bytes; mutate to change what the next listing and fetch serve. */
  files: Map<string, Uint8Array>;
  state: string;
  problems: string[];
  draft: Record<string, unknown> | null;
  failListing: boolean;
  putError: string | null;
}

function listingBody(s: OracleServer) {
  return {
    files: [...s.files].map(([name, bytes]) => ({
      name,
      size: bytes.length,
      sha256: sha256HexBytes(bytes),
    })),
    problems: s.problems,
    oracle_draft: s.draft,
    state: s.state,
  };
}

/** A fake factoryd for `/requests/req-1/oracle`; every name in `names` gets a file route. */
export function oracleServer(
  initial: Record<string, string | Uint8Array>,
  extraNames: readonly string[] = [],
): OracleServer {
  const files = new Map(
    Object.entries(initial).map(([name, v]) => [name, typeof v === "string" ? enc(v) : v]),
  );
  const state: OracleServer = {
    server: fakeServer(),
    files,
    state: "oracle_review",
    problems: [],
    draft: null,
    failListing: false,
    putError: null,
  };
  const routes: FakeRoute[] = [
    {
      on: "GET /requests/req-1/oracle",
      reply: () =>
        state.failListing ? new Response("boom", { status: 500 }) : json(listingBody(state)),
    },
  ];
  for (const name of [...files.keys(), ...extraNames]) {
    const path = `/requests/req-1/oracle/${encodeURIComponent(name)}`;
    routes.push({
      on: `GET ${path}`,
      reply: () => bytesResponse(files.get(name)),
    });
    routes.push({
      on: `PUT ${path}`,
      reply: () =>
        state.putError === null ? json({ name }) : apiErrorResponse(422, state.putError),
    });
  }
  const server = fakeServer(routes);
  return Object.assign(state, { server });
}

/** The tile of a file, by the name its row is shown under. */
export function tileOf(container: HTMLElement, keyId: string): HTMLElement {
  return within(container).getByTestId(`oracle-file-${keyId}`);
}

/** Toggles a file's tile, as an operator clicking its row. */
export async function toggle(container: HTMLElement, keyId: string): Promise<void> {
  const user = userEvent.setup();
  await user.click(within(tileOf(container, keyId)).getAllByRole("button")[0] as HTMLElement);
}
