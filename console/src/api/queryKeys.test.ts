import { QueryClient } from "@tanstack/react-query";

import { isOracleKey, isUnderRequest, queryKeys } from "@/api/queryKeys";

describe("query keys", () => {
  test("a file at a hash extends the file's key, so oracle invalidation still covers it", () => {
    const k = queryKeys.requests;
    expect(k.oracleFileAt("r1", "a.txt", "abc")).toEqual([...k.oracleFile("r1", "a.txt"), "abc"]);
    expect(k.ticketOracleFileAt("r1", 2, "a.txt", "abc")).toEqual([
      ...k.ticketOracleFile("r1", 2, "a.txt"),
      "abc",
    ]);
    expect(isOracleKey(k.oracleFileAt("r1", "a.txt", "abc"))).toBe(true);
    expect(isOracleKey(k.ticketOracleFileAt("r1", 2, "a.txt", "abc"))).toBe(true);
  });

  test("a run's diff and release sit under its evidence prefix, and its detail does not", () => {
    const k = queryKeys.runs;
    expect(k.diff("run-1").slice(0, k.evidence("run-1").length)).toEqual(k.evidence("run-1"));
    expect(k.release("run-1").slice(0, k.evidence("run-1").length)).toEqual(k.evidence("run-1"));
    const client = new QueryClient();
    client.setQueryData(k.detail("run-1"), "detail");
    client.setQueryData(k.diff("run-1"), "diff");
    client.setQueryData(k.release("run-1"), "release");
    client.setQueryData(k.diff("run-2"), "other run");
    void client.invalidateQueries({ queryKey: k.evidence("run-1") });
    const stale = (key: readonly unknown[]) => client.getQueryState(key)?.isInvalidated;
    expect(stale(k.diff("run-1"))).toBe(true);
    expect(stale(k.release("run-1"))).toBe(true);
    expect(stale(k.detail("run-1"))).toBe(false);
    expect(stale(k.diff("run-2"))).toBe(false);
  });
});

describe("isUnderRequest", () => {
  const k = queryKeys.requests;
  test("matches revisions, oracle listings and files, ticket oracle", () => {
    for (const key of [
      k.revisions("r1"),
      k.revision("r1", 0),
      k.oracle("r1"),
      k.oracleFile("r1", "a"),
      k.oracleFileAt("r1", "a", "h"),
      k.ticketOracle("r1", 1),
      k.ticketOracleFile("r1", 1, "a"),
      k.ticketOracleFileAt("r1", 1, "a", "h"),
    ]) {
      expect(isUnderRequest(key, "r1")).toBe(true);
    }
  });

  test("does not match the detail, the list, another request or another resource", () => {
    expect(isUnderRequest(k.detail("r1"), "r1")).toBe(false);
    expect(isUnderRequest(k.list(), "r1")).toBe(false);
    expect(isUnderRequest(k.revisions("r2"), "r1")).toBe(false);
    expect(isUnderRequest(queryKeys.runs.diff("r1"), "r1")).toBe(false);
    expect(isUnderRequest(queryKeys.projects.stats("r1"), "r1")).toBe(false);
  });

  test("isOracleKey is false for revisions", () => {
    expect(isOracleKey(k.revisions("r1"))).toBe(false);
  });
});
