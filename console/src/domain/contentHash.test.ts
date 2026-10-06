import { type JsonObject, asObject } from "@/domain/decode";
import {
  expectedSha256For,
  oracleExpectedSha256,
  sha256Hex,
  sha256HexBytes,
} from "@/domain/contentHash";
import { type RequestSummary, decodeRequestSummary } from "@/domain/request";
import { readFixtureJson } from "@/test/fixtures";

const route = "GET /requests/{id}";

interface TicketInput {
  readonly index: number;
  readonly specPath: string;
  readonly content: string;
}

function request(state: string, spec = "", tickets: readonly TicketInput[] = []): RequestSummary {
  const body: JsonObject = {
    id: "req-1",
    workspace: "/w",
    project: "p",
    state,
    submitted_at: "2026-09-28T00:00:00Z",
    updated_at: "2026-09-28T00:00:00Z",
    spec,
    tickets: tickets.map((t) => ({ index: t.index, spec_path: t.specPath, content: t.content })),
  };
  return decodeRequestSummary(body, route);
}

test("sha256Hex matches a known SHA-256 digest", () => {
  // echo -n "hello" | sha256sum
  expect(sha256Hex("hello")).toBe(
    "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
  );
});

test("sha256HexBytes hashes raw bytes, not decoded text (a non-UTF-8 oracle file must hash to the digest the server computed)", () => {
  // python3 -c "import hashlib; print(hashlib.sha256(bytes([255,254,0])).hexdigest())"
  expect(sha256HexBytes(new Uint8Array([0xff, 0xfe, 0x00]))).toBe(
    "ba778c0261008c8f71ae4061ad0162ffcbe63b52c91f89f236738131d1217ec7",
  );
  expect(sha256HexBytes(new TextEncoder().encode("hello"))).toBe(sha256Hex("hello"));
});

test("oracleExpectedSha256 keys each shown file as oracle/NAME", () => {
  expect(oracleExpectedSha256({ "RUN_COMMAND.txt": "aa", "a_test.go": "bb" })).toEqual({
    "oracle/RUN_COMMAND.txt": "aa",
    "oracle/a_test.go": "bb",
  });
  expect(oracleExpectedSha256({})).toEqual({});
});

describe("expectedSha256For", () => {
  test("spec_review hashes spec.md under that literal key", () => {
    expect(expectedSha256For(request("spec_review", "# Spec\n"))).toEqual({
      "spec.md": "603c828a03383f98b82bf9c6787deeb5c40c02871a1b8d6ba14b0d538020a02b",
    });
  });

  test("plan_review hashes each ticket under its own tickets/NNN.spec.md key, extracted from the (absolute, in production) specPath", () => {
    const req = request("plan_review", "", [
      { index: 1, specPath: "/data/requests/req-1/tickets/001.spec.md", content: "Ticket 1" },
      { index: 2, specPath: "/data/requests/req-1/tickets/002.spec.md", content: "Ticket 2" },
    ]);
    expect(expectedSha256For(req)).toEqual({
      "tickets/001.spec.md": "940953878845868d9c17752c27c2990b2eb48e102272385eba2da0a3162ed1cb",
      "tickets/002.spec.md": "58215765ceaf24d42b4d774738bdaa0406a05166036e23da18eb423ccf8b30f9",
    });
  });

  test('uses the final /tickets/ segment, not the first (regression: an absolute -data-dir that itself contains a directory literally named "tickets" -- a plausible real deployment path -- matched the outer occurrence first, producing a key the server never recognizes and silently dropping the ticket from the map)', () => {
    const req = request("plan_review", "", [
      {
        index: 1,
        specPath: "/srv/tickets/factory-data/requests/req-1/tickets/001.spec.md",
        content: "Ticket 1",
      },
    ]);
    expect(expectedSha256For(req)).toEqual({
      "tickets/001.spec.md": "940953878845868d9c17752c27c2990b2eb48e102272385eba2da0a3162ed1cb",
    });
  });

  test("any other state has nothing to bind an approval to", () => {
    expect(expectedSha256For(request("building"))).toEqual({});
  });
});

// The same file internal/request's golden_vectors_test.go reads: one set of
// inputs and expected digests for the console and the server.
describe("golden vectors (test/fixtures/vectors/content-hash.json)", () => {
  const vectors = asObject(readFixtureJson("vectors/content-hash.json"), "content-hash.json");

  interface DigestVector {
    name: string;
    text?: string;
    base64?: string;
    sha256: string;
  }
  interface ApprovalVector {
    name: string;
    state: string;
    spec: string;
    tickets: { index: number; spec_path: string; content: string }[];
    want: Record<string, string>;
  }
  interface OracleVector {
    name: string;
    shown: Record<string, string>;
    want: Record<string, string>;
  }

  const digests = vectors.digests as DigestVector[];
  const approvals = vectors.approvals as ApprovalVector[];
  const oracleApprovals = vectors.oracle_approvals as OracleVector[];

  test("every section is present", () => {
    expect(digests.length).toBeGreaterThan(0);
    expect(approvals.length).toBeGreaterThan(0);
    expect(oracleApprovals.length).toBeGreaterThan(0);
  });

  for (const d of digests) {
    test(`digest: ${d.name}`, () => {
      if (d.text !== undefined) {
        expect(sha256Hex(d.text)).toBe(d.sha256);
      } else {
        const bytes = Uint8Array.from(atob(d.base64 ?? ""), (c) => c.charCodeAt(0));
        expect(sha256HexBytes(bytes)).toBe(d.sha256);
      }
    });
  }

  for (const a of approvals) {
    test(`approval: ${a.name}`, () => {
      const tickets = a.tickets.map((t) => ({
        index: t.index,
        specPath: t.spec_path,
        content: t.content,
      }));
      expect(expectedSha256For(request(a.state, a.spec, tickets))).toEqual(a.want);
    });
  }

  for (const o of oracleApprovals) {
    test(`oracle approval: ${o.name}`, () => {
      expect(oracleExpectedSha256(o.shown)).toEqual(o.want);
    });
  }
});
