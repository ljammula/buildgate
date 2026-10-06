import { createHttp } from "@/api/http";
import {
  getRequestOracle,
  getRequestOracleFile,
  getRequestTicketOracle,
  getRequestTicketOracleFile,
  putRequestOracleRunCommand,
} from "@/api/oracle";
import { ApiError } from "@/domain/apiError";
import { readFixtureBytes, readFixtureText } from "@/test/fixtures";

interface Call {
  readonly url: string;
  readonly init: RequestInit;
}

function recording(respond: (call: Call) => Response) {
  const calls: Call[] = [];
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: input instanceof Request ? input.url : input.toString(), init: init ?? {} };
    calls.push(call);
    return Promise.resolve(respond(call));
  }) as unknown as typeof globalThis.fetch;
  return { fetch, calls };
}

const httpWith = (fetch: typeof globalThis.fetch) =>
  createHttp({
    baseUrl: "",
    readToken: "read-t",
    startToken: "start-t",
    overrideToken: "override-t",
    fetch,
  });

const READ = { Authorization: "Bearer read-t" };
const WRITE = { "Content-Type": "application/json", Authorization: "Bearer override-t" };
const FILE_SHA = "0e2df58c76e6bce05db5cd535c182f7b4c09dc25dc1aa6b18d55a386529b20e6";

describe("listings", () => {
  test("getRequestOracle sends the read token and decodes the committed fixture", async () => {
    const { fetch, calls } = recording(
      () => new Response(readFixtureText("api/request-oracle.json")),
    );
    const listing = await getRequestOracle(httpWith(fetch), "req-oracle-review");
    expect(calls[0]!.url).toBe("/requests/req-oracle-review/oracle");
    expect(calls[0]!.init.method).toBe("GET");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(listing.state).toBe("oracle_review");
    expect(listing.files.map((f) => [f.name, f.size, f.sha256])).toEqual([
      ["RUN_COMMAND.txt", 22, FILE_SHA],
      [
        "idempotency_test.go",
        15,
        "eb06238620bab6c0a14810ad6af6e5f2d34ed7d404a8d5025fe5001aeee2a5f2",
      ],
    ]);
    expect(listing.problems).toHaveLength(1);
    expect(listing.draftStatus).toBe("drafted");
    expect(listing.proposedCommand).toBe("go test ./.oracle/...");
  });

  test("getRequestTicketOracle fetches the ticket's own listing", async () => {
    const { fetch, calls } = recording(
      () => new Response(readFixtureText("api/ticket-oracle.json")),
    );
    const listing = await getRequestTicketOracle(httpWith(fetch), "req-plan-review", 1);
    expect(calls[0]!.url).toBe("/requests/req-plan-review/tickets/1/oracle");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(listing.files.length).toBeGreaterThan(0);
    expect(listing.files[0]!.name).toBe("RUN_COMMAND.txt");
  });
});

describe("files", () => {
  test("getRequestOracleFile hashes the bytes as received and encodes the name", async () => {
    const { fetch, calls } = recording(
      () => new Response(readFixtureBytes("api/request-oracle-file.txt") as BodyInit),
    );
    const file = await getRequestOracleFile(httpWith(fetch), "req 1", "RUN_COMMAND.txt");
    expect(calls[0]!.url).toBe("/requests/req%201/oracle/RUN_COMMAND.txt");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(file.content.text).toBe("go test ./.oracle/...\n");
    expect(file.content.validUtf8).toBe(true);
    expect(file.sha256).toBe(FILE_SHA);

    await getRequestOracleFile(httpWith(fetch), "req-1", "a/b.go");
    expect(calls[1]!.url).toBe("/requests/req-1/oracle/a%2Fb.go");
  });

  test("a file that is not valid UTF-8 is hashed over its original bytes", async () => {
    // 0xff is not UTF-8: decoding and re-encoding would change the bytes
    // and so the hash an approval is bound to.
    const bytes = new Uint8Array([0x61, 0xff, 0x0a]);
    const { fetch } = recording(() => new Response(bytes));
    const file = await getRequestOracleFile(httpWith(fetch), "req-1", "x.bin");
    expect(file.content.validUtf8).toBe(false);
    expect(Array.from(file.content.bytes)).toEqual([0x61, 0xff, 0x0a]);
    // sha256 of the three bytes 61 ff 0a
    expect(file.sha256).toBe("ba35dc0ae977a525da3dce526ea3488a8c3f537d01bf8db91ab63fa7474eeacb");
  });

  test("getRequestTicketOracleFile fetches ticket n's file", async () => {
    const { fetch, calls } = recording(
      () => new Response(readFixtureBytes("api/ticket-oracle-file.txt") as BodyInit),
    );
    const file = await getRequestTicketOracleFile(
      httpWith(fetch),
      "req-plan-review",
      1,
      "RUN_COMMAND.txt",
    );
    expect(calls[0]!.url).toBe("/requests/req-plan-review/tickets/1/oracle/RUN_COMMAND.txt");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(file.content.text).toBe("go test ./.oracle/...\n");
    expect(file.sha256).toBe(FILE_SHA);
  });
});

describe("putRequestOracleRunCommand", () => {
  test("PUTs the content with the override token, and base_sha256 only when given", async () => {
    const { fetch, calls } = recording(() => new Response("{}"));
    const http = httpWith(fetch);
    await putRequestOracleRunCommand(http, "req-1", "go test ./...\n", { baseSha256: "base1" });
    expect(calls[0]!.url).toBe("/requests/req-1/oracle/RUN_COMMAND.txt");
    expect(calls[0]!.init.method).toBe("PUT");
    expect(calls[0]!.init.headers).toEqual(WRITE);
    expect(calls[0]!.init.body).toBe('{"content":"go test ./...\\n","base_sha256":"base1"}');

    await putRequestOracleRunCommand(http, "req-1", "x");
    expect(calls[1]!.init.body).toBe('{"content":"x"}');
  });

  test("a 409 rejects with the server's current_sha256, a 422 with its reason", async () => {
    const respond = (status: number, body: unknown) =>
      recording(() => new Response(JSON.stringify(body), { status }));
    const conflict = respond(409, { error: "RUN_COMMAND.txt changed", current_sha256: "abc" });
    const error = (await putRequestOracleRunCommand(httpWith(conflict.fetch), "req-1", "x", {
      baseSha256: "old",
    }).then(
      () => null,
      (e: unknown) => e,
    )) as ApiError;
    expect(error).toBeInstanceOf(ApiError);
    expect(error.currentSha256).toBe("abc");

    const invalid = respond(422, { error: "command is empty" });
    const refused = (await putRequestOracleRunCommand(httpWith(invalid.fetch), "req-1", "").then(
      () => null,
      (e: unknown) => e,
    )) as ApiError;
    expect(refused.status).toBe(422);
    expect(refused.serverMessage).toBe("command is empty");
    expect(refused.currentSha256).toBeNull();
  });
});
