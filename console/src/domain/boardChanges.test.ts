import { boardChanges } from "@/domain/boardChanges";
import { requestSummary } from "@/test/requestFixtures";

const req = (id: string, state: string) => requestSummary({ id, state });

describe("boardChanges", () => {
  test("the first load changes nothing and records every state", () => {
    const result = boardChanges(null, [req("a", "building"), req("b", "done")]);
    expect([...result.changed]).toEqual([]);
    expect([...result.next]).toEqual([
      ["a", "building"],
      ["b", "done"],
    ]);
  });

  test("a state change marks that request only", () => {
    const first = boardChanges(null, [req("a", "building"), req("b", "done")]);
    const result = boardChanges(first.next, [req("a", "pr_review"), req("b", "done")]);
    expect([...result.changed]).toEqual(["a"]);
    expect(result.next.get("a")).toBe("pr_review");
  });

  test("a request not seen before is marked", () => {
    const first = boardChanges(null, [req("a", "building")]);
    const result = boardChanges(first.next, [req("a", "building"), req("c", "submitted")]);
    expect([...result.changed]).toEqual(["c"]);
  });

  test("an unchanged poll marks nothing", () => {
    const list = [req("a", "building"), req("b", "done")];
    const first = boardChanges(null, list);
    expect([...boardChanges(first.next, list).changed]).toEqual([]);
  });

  test("a request that disappears is dropped, and is new when it comes back", () => {
    const first = boardChanges(null, [req("a", "building"), req("b", "done")]);
    const gone = boardChanges(first.next, [req("a", "building")]);
    expect([...gone.changed]).toEqual([]);
    expect(gone.next.has("b")).toBe(false);
    const back = boardChanges(gone.next, [req("a", "building"), req("b", "done")]);
    expect([...back.changed]).toEqual(["b"]);
  });
});
