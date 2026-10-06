import { asObject } from "@/domain/decode";
import {
  decodeOracleListing,
  keptOpenOracleKeys,
  keptOracleContentKeys,
  oracleFileContentFromBytes,
  oracleFileSubtitle,
  parseOracleManifest,
  shownOracleFiles,
} from "@/domain/oracle";
import { readFixtureBytes, readFixtureJson } from "@/test/fixtures";

const RUN_COMMAND_SHA = "0e2df58c76e6bce05db5cd535c182f7b4c09dc25dc1aa6b18d55a386529b20e6";

test("GET /requests/{id}/oracle decodes, with the drafting record", () => {
  const listing = decodeOracleListing(
    asObject(readFixtureJson("api/request-oracle.json"), "request-oracle.json"),
    "GET /requests/{id}/oracle",
  );
  expect(listing.files).toEqual([
    { name: "RUN_COMMAND.txt", size: 22, sha256: RUN_COMMAND_SHA },
    {
      name: "idempotency_test.go",
      size: 15,
      sha256: "eb06238620bab6c0a14810ad6af6e5f2d34ed7d404a8d5025fe5001aeee2a5f2",
    },
  ]);
  expect(listing.problems).toHaveLength(1);
  expect(listing.problems[0]).toMatch(/^no MANIFEST\.json -- add MANIFEST\.json mapping/);
  expect(listing.state).toBe("oracle_review");
  expect(listing.draftStatus).toBe("drafted");
  expect(listing.draftDetail).toBe("one criterion is covered");
  expect(listing.proposedCommand).toBe("go test ./.oracle/...");
});

test("a ticket's oracle listing has no drafting record", () => {
  const listing = decodeOracleListing(
    asObject(readFixtureJson("api/ticket-oracle.json"), "ticket-oracle.json"),
    "GET /requests/{id}/tickets/{n}/oracle",
  );
  expect(listing.files).toEqual([{ name: "RUN_COMMAND.txt", size: 22, sha256: RUN_COMMAND_SHA }]);
  expect(listing.problems).toHaveLength(1);
  expect(listing.problems[0]).toMatch(/^the runtime canary cannot be built/);
  expect(listing.state).toBe("plan_review");
  expect(listing.draftStatus).toBe("");
  expect(listing.draftDetail).toBe("");
  expect(listing.proposedCommand).toBe("");
});

test("a file without a name names the route and the field", () => {
  expect(() => decodeOracleListing({ files: [{ size: 1 }] }, "GET /requests/{id}/oracle")).toThrow(
    /GET \/requests\/\{id\}\/oracle\.files\[0\]\.name:/,
  );
});

test("fetched oracle files keep their exact bytes and decode as text", () => {
  for (const file of ["api/request-oracle-file.txt", "api/ticket-oracle-file.txt"]) {
    const bytes = readFixtureBytes(file);
    const content = oracleFileContentFromBytes(bytes);
    expect(content.bytes).toBe(bytes);
    expect(content.text).toBe("go test ./.oracle/...\n");
    expect(content.validUtf8).toBe(true);
  }
});

test("invalid UTF-8 is flagged and never substituted with U+FFFD", () => {
  const content = oracleFileContentFromBytes(new Uint8Array([0x61, 0xff, 0xc3, 0x28, 0x62]));
  expect(content.validUtf8).toBe(false);
  expect(content.text).toBe("a\uDCFF\uDCC3(b");
  expect(content.text).not.toContain("�");
});

test("a byte order mark and multi-byte characters are kept", () => {
  const content = oracleFileContentFromBytes(
    new Uint8Array([0xef, 0xbb, 0xbf, 0xe2, 0x82, 0xac, 0xf0, 0x9f, 0x98, 0x80]),
  );
  expect(content.validUtf8).toBe(true);
  expect(content.text).toBe("﻿€\u{1F600}");
});

test("an empty file is valid and empty", () => {
  const content = oracleFileContentFromBytes(new Uint8Array());
  expect(content.text).toBe("");
  expect(content.validUtf8).toBe(true);
});

test("parseOracleManifest tolerates non-array and non-object input", () => {
  expect(parseOracleManifest("not json")).toBeNull();
  expect(parseOracleManifest('{"a":1}')).toBeNull();
  expect(parseOracleManifest('[1, {"criterion":"x"}]')).toHaveLength(1);
});

test("parseOracleManifest reads every field and defaults the missing ones", () => {
  const entries = parseOracleManifest(
    JSON.stringify([
      {
        criterion: "AC1",
        oracle_file: "a_test.go",
        rationale: "covers it",
        target_path: "pkg/a.go",
        supersedes: ["old_test.go", 7],
        criterion_index: 2,
      },
      { criterion: "AC2", oracle_file: null },
    ]),
  );
  expect(entries).toEqual([
    {
      criterion: "AC1",
      oracleFile: "a_test.go",
      rationale: "covers it",
      targetPath: "pkg/a.go",
      supersedes: ["old_test.go", "7"],
      criterionIndex: 2,
    },
    {
      criterion: "AC2",
      oracleFile: null,
      rationale: "",
      targetPath: "",
      supersedes: [],
      criterionIndex: null,
    },
  ]);
});

test("a file is shown only while open, listed, and hashed as listed", () => {
  const listed = new Map([
    ["a", "h1"],
    ["b", "h2"],
    ["c", "h3"],
  ]);
  const opened = new Set(["a", "b", "gone"]);
  const hashes = new Map([
    ["a", "h1"],
    ["b", "stale"],
    ["c", "h3"],
  ]);
  expect([...shownOracleFiles(listed, opened, hashes)]).toEqual([["a", "h1"]]);
});

test("a fresh listing forgets moved or unlisted content and closes unlisted tiles", () => {
  const listed = new Map([
    ["a", "h1"],
    ["b", "h2"],
  ]);
  const hashes = new Map([
    ["a", "h1"],
    ["b", "old"],
    ["z", "h9"],
  ]);
  expect(keptOracleContentKeys(listed, hashes)).toEqual(["a"]);
  expect(keptOpenOracleKeys(listed, new Set(["a", "z"]))).toEqual(["a"]);
});

test("a file tile's subtitle shortens the hash to 12 characters", () => {
  expect(oracleFileSubtitle({ name: "n", size: 22, sha256: RUN_COMMAND_SHA })).toBe(
    "22 bytes · sha256 0e2df58c76e6",
  );
  expect(oracleFileSubtitle({ name: "n", size: 0, sha256: "abc" })).toBe("0 bytes · sha256 abc");
});
