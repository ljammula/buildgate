import { type RequestSummary, decodeRequestSummary } from "@/domain/request";

import { requestJson } from "@/test/requestFixtures";

import { anchorTargets } from "./reviewAnchors";

function request(patch: Record<string, unknown>): RequestSummary {
  return decodeRequestSummary(
    { ...requestJson({ id: "req-1", state: "spec_review" }), ...patch },
    "test",
  );
}

const SPEC =
  "# Spec\n\n## Problem\n\np\n\n## Acceptance criteria\n\n1. It adds.\n2. " +
  "A".repeat(100) +
  "\n\n## Risks\n\nr\n";

test("a spec offers the file, each section it has, and each criterion", () => {
  const targets = anchorTargets(request({ spec: SPEC }));

  expect(targets.map((t) => [t.path, t.section, t.item])).toEqual([
    ["spec.md", "", 0],
    ["spec.md", "# Spec", 0],
    ["spec.md", "## Problem", 0],
    ["spec.md", "## Acceptance criteria", 0],
    ["spec.md", "## Acceptance criteria", 1],
    ["spec.md", "## Acceptance criteria", 2],
    ["spec.md", "## Risks", 0],
  ]);
  expect(targets[0]?.label).toBe("spec.md · the whole file");
  expect(targets[4]?.label).toBe("spec.md · Acceptance criteria · 1. It adds.");
  expect(targets[5]?.label).toHaveLength("spec.md · Acceptance criteria · ".length + 80);
  expect(new Set(targets.map((t) => t.id)).size).toBe(targets.length);
});

test("a plan offers each ticket file by its request-relative path, with its sections", () => {
  const content = "## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a\n";
  const targets = anchorTargets(
    request({
      state: "plan_review",
      spec: SPEC,
      tickets: [
        { index: 1, spec_path: "/srv/data/requests/req-1/tickets/001.spec.md", content },
        { index: 2, spec_path: "tickets/002.spec.md", content: "" },
      ],
    }),
  );

  expect(targets.map((t) => t.label)).toEqual([
    "tickets/001.spec.md · the whole file",
    "tickets/001.spec.md · Goal",
    "tickets/001.spec.md · Plan",
    "tickets/001.spec.md · Files to touch",
  ]);
});

test.each(["oracle_review", "building", "quarantined"])("nothing to point at in %s", (state) => {
  expect(anchorTargets(request({ state, spec: SPEC }))).toEqual([]);
});
