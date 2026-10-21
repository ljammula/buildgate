import {
  addPendingComment,
  clearAllPendingCommentsForTest,
  freshPendingComments,
  getPendingComments,
  pendingKey,
} from "@/platform/pendingComments";

afterEach(clearAllPendingCommentsForTest);

const comment = (note: string) => ({
  path: "spec.md",
  section: "## Scope",
  item: 0,
  note,
  quote: "q",
});

test("writing a batch drops the same request's batches from earlier stage entries", () => {
  const old = pendingKey("req-1", "2026-01-01T00:00:00Z");
  const other = pendingKey("req-2", "2026-01-01T00:00:00Z");
  const now = pendingKey("req-1", "2026-02-01T00:00:00Z");
  addPendingComment(old, comment("old"));
  addPendingComment(other, comment("other request"));
  addPendingComment(now, comment("new"));
  clearAllPendingCommentsForTestKeepingStorage();
  expect(getPendingComments(old)).toEqual([]);
  expect(getPendingComments(other)).toHaveLength(1);
  expect(getPendingComments(now)).toHaveLength(1);
});

// Forgets what is held in memory only, so the next read goes to storage.
function clearAllPendingCommentsForTestKeepingStorage() {
  const kept: [string, string][] = Object.keys(window.localStorage).map((k) => [
    k,
    window.localStorage.getItem(k) ?? "",
  ]);
  clearAllPendingCommentsForTest();
  for (const [k, v] of kept) window.localStorage.setItem(k, v);
}

test("a change made by another tab is seen: the storage event drops what is held", () => {
  const key = pendingKey("req-1", "t");
  addPendingComment(key, comment("mine"));
  expect(getPendingComments(key)).toHaveLength(1);
  window.localStorage.removeItem(`factoryPendingComments:${key}`);
  expect(getPendingComments(key)).toHaveLength(1); // not yet told
  window.dispatchEvent(
    new StorageEvent("storage", { key: `factoryPendingComments:${key}`, newValue: null }),
  );
  expect(getPendingComments(key)).toEqual([]);
});

test("a fresh read takes the batch as storage holds it", () => {
  const key = pendingKey("req-1", "t");
  addPendingComment(key, comment("mine"));
  window.localStorage.setItem(
    `factoryPendingComments:${key}`,
    JSON.stringify([comment("a"), comment("b")]),
  );
  expect(freshPendingComments(key).map((c) => c.note)).toEqual(["a", "b"]);
});
