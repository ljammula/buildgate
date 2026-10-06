import { type RequestSummary, decodeRequestSummary } from "@/domain/request";

import { requestJson } from "@/test/requestFixtures";

import {
  anchorExcerpt,
  anchorPlace,
  anchoredChanges,
  anchorTargets,
  rejectionForRevision,
} from "./reviewAnchors";

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

const OLD_SPEC =
  "# Spec\n\n## Problem\n\nold problem\n\n\n## Acceptance criteria\n\n1. It adds.\n2. A key is scoped.\n\n## Risks\n\nr\n";
const NEW_SPEC =
  "# Spec\n\n## Problem\n\nnew problem\n\n## Acceptance criteria\n\n1. It adds.\n2. A key is scoped to one account.\n\n## Risks\n\nr\n";

function anchor(path: string, section: string, item: number) {
  return { path, section, item, note: "n" };
}

test("an anchor's place reads file, section and item", () => {
  expect(anchorPlace(anchor("spec.md", "## Acceptance criteria", 2))).toBe(
    "spec.md · Acceptance criteria · number 2",
  );
  expect(anchorPlace(anchor("spec.md", "", 0))).toBe("spec.md");
});

test("an excerpt is the criterion, the section without trailing blank lines, or the file", () => {
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "## Acceptance criteria", 2))).toBe(
    "2. A key is scoped.",
  );
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "## Problem", 0))).toBe(
    "## Problem\n\nold problem",
  );
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "## Risks", 0))).toBe("## Risks\n\nr");
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "", 0))).toBe(OLD_SPEC);
  const ticket =
    "Verify-Command: true\n\n## Goal\n\ng\n\n## Plan\n\n### Steps\n\n1. s\n\n## Out of scope\n\nnone\n";
  expect(anchorExcerpt(ticket, anchor("tickets/001.spec.md", "### Steps", 0))).toBe(
    "### Steps\n\n1. s",
  );
});

test("an excerpt is null where the place does not exist or the file has no sections the console reads", () => {
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "## Acceptance criteria", 3))).toBeNull();
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "## Scope", 0))).toBeNull();
  expect(anchorExcerpt(OLD_SPEC, anchor("spec.md", "## Not a heading", 0))).toBeNull();
  expect(anchorExcerpt("x", anchor("oracle/RUN_COMMAND.txt", "## Problem", 0))).toBeNull();
});

test("each anchored note is set against its place then and now", () => {
  const rejection = {
    by: "jane",
    at: "2026-09-10T09:00:00Z",
    reason: "r",
    fromState: "spec_review",
    forStage: null,
    note: "",
    anchors: [
      anchor("spec.md", "## Acceptance criteria", 2),
      anchor("spec.md", "## Risks", 0),
      anchor("tickets/001.spec.md", "## Goal", 0),
    ],
  };
  const changes = anchoredChanges(rejection, { "spec.md": OLD_SPEC }, (path) =>
    path === "spec.md" ? NEW_SPEC : "",
  );
  expect(changes.map((c) => [c.place, c.before, c.after])).toEqual([
    [
      "spec.md · Acceptance criteria · number 2",
      "2. A key is scoped.",
      "2. A key is scoped to one account.",
    ],
    ["spec.md · Risks", "## Risks\n\nr", "## Risks\n\nr"],
    ["tickets/001.spec.md · Goal", null, null],
  ]);
});

test("a revision's rejection is the one with its instant and operator", () => {
  const base = { reason: "r", fromState: "spec_review", forStage: null, note: "", anchors: [] };
  const first = { ...base, by: "jane", at: "2026-09-10T09:00:00Z" };
  const second = { ...base, by: "jane", at: "2026-09-10T10:00:00.5Z" };
  expect(
    rejectionForRevision([first, second], { by: "jane", at: "2026-09-10T10:00:00.500Z" }),
  ).toBe(second);
  expect(
    rejectionForRevision([first, second], { by: "bob", at: "2026-09-10T09:00:00Z" }),
  ).toBeNull();
});
