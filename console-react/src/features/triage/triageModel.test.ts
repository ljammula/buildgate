import { decodeRequestSummary } from "@/domain/request";
import { requestJson, ticketJson } from "@/test/requestFixtures";

import { artifactContent, isTypingTarget, triageRequests } from "./triageModel";

function summary(o: Parameters<typeof requestJson>[0] & { spec?: string }) {
  return decodeRequestSummary({ ...requestJson(o), spec: o.spec ?? "" }, "test");
}

test("lists only spec_review and plan_review, oldest wait first", () => {
  const list = triageRequests([
    summary({ id: "working", state: "building" }),
    summary({ id: "new", state: "plan_review", enteredAt: "2026-09-12T08:00:00Z" }),
    summary({ id: "old", state: "spec_review", enteredAt: "2026-09-10T08:00:00Z" }),
    summary({ id: "oracle", state: "oracle_review" }),
    summary({ id: "halted", state: "halted" }),
  ]);
  expect(list.map((r) => r.id)).toEqual(["old", "new"]);
});

test("a spec_review artifact is the spec", () => {
  expect(artifactContent(summary({ id: "a", state: "spec_review", spec: "# Spec" }))).toBe(
    "# Spec",
  );
});

test("a plan_review artifact is the ticket plans, not the approved spec", () => {
  const detail = summary({
    id: "a",
    state: "plan_review",
    spec: "# Approved",
    tickets: [
      ticketJson({ index: 1, content: "Plan one" }),
      ticketJson({ index: 2 }),
      ticketJson({ index: 3, content: "Plan three" }),
    ],
  });
  expect(artifactContent(detail)).toBe(
    "--- ticket 1 ---\nPlan one\n\n--- ticket 3 ---\nPlan three\n\n",
  );
});

test("typing targets are inputs, textareas, selects and editable elements", () => {
  expect(isTypingTarget(document.createElement("input"))).toBe(true);
  expect(isTypingTarget(document.createElement("textarea"))).toBe(true);
  expect(isTypingTarget(document.createElement("select"))).toBe(true);
  expect(isTypingTarget(document.createElement("button"))).toBe(false);
  expect(isTypingTarget(null)).toBe(false);
});
