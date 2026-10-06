import { decodeRequestSummary } from "@/domain/request";
import { requestJson, ticketJson } from "@/test/requestFixtures";

import {
  artifactContent,
  decidesInPlace,
  isTypingTarget,
  triageReason,
  triageRequests,
} from "./triageModel";

function summary(o: Parameters<typeof requestJson>[0] & { spec?: string }) {
  return decodeRequestSummary({ ...requestJson(o), spec: o.spec ?? "" }, "test");
}

test("lists every request that needs the operator, oldest wait first", () => {
  const list = triageRequests([
    summary({ id: "working", state: "building" }),
    summary({ id: "new", state: "plan_review", enteredAt: "2026-09-12T08:00:00Z" }),
    summary({ id: "old", state: "spec_review", enteredAt: "2026-09-10T08:00:00Z" }),
    summary({ id: "oracle", state: "oracle_review", enteredAt: "2026-09-11T08:00:00Z" }),
    summary({ id: "halted", state: "halted", enteredAt: "2026-09-11T09:00:00Z" }),
    summary({ id: "done", state: "done" }),
  ]);
  expect(list.map((r) => r.id)).toEqual(["old", "oracle", "halted", "new"]);
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

test("only spec_review and plan_review are decided in place", () => {
  expect(decidesInPlace(summary({ id: "a", state: "spec_review" }))).toBe(true);
  expect(decidesInPlace(summary({ id: "a", state: "plan_review" }))).toBe(true);
  expect(decidesInPlace(summary({ id: "a", state: "oracle_review" }))).toBe(false);
  expect(decidesInPlace(summary({ id: "a", state: "halted" }))).toBe(false);
});

test("the reason is the server's next step, except in review states that name CLI commands", () => {
  const withNext = (state: string) =>
    decodeRequestSummary(
      { ...requestJson({ id: "a", state }), next_action: "Run factoryd x." },
      "t",
    );
  expect(triageReason(withNext("halted"))).toBe("Run factoryd x.");
  expect(triageReason(withNext("oracle_review"))).toBe(
    "The drafted acceptance tests wait for your review.",
  );
  expect(triageReason(summary({ id: "a", state: "resume_review" }))).toBe(
    "A resume plan waits for your approval.",
  );
});
