import { readFixtureJson } from "@/test/fixtures";

import {
  missingTicketHeaderKeys,
  specAcceptanceCriteria,
  specStructure,
  ticketStructure,
  validateSpecSkeleton,
  validateTicketPlan,
} from "./specSkeleton";

interface SpecVector {
  name: string;
  text: string;
  valid: boolean;
  missing_heading: string;
  criteria_section_empty: boolean;
  criteria: string[];
}
interface TicketVector {
  name: string;
  text: string;
  plan_valid: boolean;
  missing_heading: string;
  empty_section: string;
  covered: number[];
  headers_missing: string[];
}

// The same file internal/request's skeleton_vectors_test.go reads and writes:
// the expectations are the server's own answers.
describe("golden vectors (test/fixtures/vectors/spec-skeleton.json)", () => {
  const vectors = readFixtureJson("vectors/spec-skeleton.json") as {
    specs: SpecVector[];
    tickets: TicketVector[];
  };

  test("the file carries both sections", () => {
    expect(vectors.specs.length).toBeGreaterThan(20);
    expect(vectors.tickets.length).toBeGreaterThan(20);
  });

  test.each(vectors.specs)("spec: $name", (v) => {
    const got = validateSpecSkeleton(v.text);
    expect({
      valid: got.missingHeading === null && !got.criteriaSectionEmpty,
      missing_heading: got.missingHeading ?? "",
      criteria_section_empty: got.criteriaSectionEmpty,
      criteria: specAcceptanceCriteria(v.text),
    }).toEqual({
      valid: v.valid,
      missing_heading: v.missing_heading,
      criteria_section_empty: v.criteria_section_empty,
      criteria: v.criteria,
    });
  });

  test.each(vectors.tickets)("ticket: $name", (v) => {
    const got = validateTicketPlan(v.text);
    expect({
      plan_valid: got.ok,
      missing_heading: got.missingHeading ?? "",
      empty_section: got.emptySection ?? "",
      covered: got.covered,
      headers_missing: missingTicketHeaderKeys(v.text),
    }).toEqual({
      plan_valid: v.plan_valid,
      missing_heading: v.missing_heading,
      empty_section: v.empty_section,
      covered: v.covered,
      headers_missing: v.headers_missing,
    });
  });
});

const SPEC = [
  "# Spec",
  "## Problem",
  "p",
  "## Scope",
  "s",
  "## Non-goals",
  "n",
  "## Affected services and packages",
  "a",
  "## Acceptance criteria",
  "1. It adds.",
  "2. It subtracts.",
  "## Risks",
  "r",
  "## Open questions",
  "none",
].join("\n");

const TICKET = [
  "Verify-Command: true",
  "Allowed-Files: a.go",
  "Required-Changed-Files: a.go",
  "## Goal",
  "g",
  "## Plan",
  "### Files to touch",
  "- a.go",
  "### Steps",
  "1. s",
  "### Tests to add",
  "- t",
  "### Acceptance criteria covered",
  "- 1",
  "- 3",
  "## Out of scope",
  "none",
].join("\n");

describe("specStructure", () => {
  test("a well-formed spec ticks every row and counts its criteria", () => {
    const check = specStructure(SPEC);
    expect(check.passes).toBe(true);
    expect(check.rows.map((r) => r.label)).toEqual([
      "# Spec",
      "## Problem",
      "## Scope",
      "## Non-goals",
      "## Affected services and packages",
      "## Acceptance criteria",
      "## Risks",
      "## Open questions",
      "Numbered criteria",
    ]);
    expect(check.rows.every((r) => r.ok)).toBe(true);
    expect(check.rows.at(-1)?.note).toBe("2 parsed");
  });

  test("every absent heading is listed at once, not only the first", () => {
    const check = specStructure(SPEC.replace("## Scope", "## scope").replace("## Risks", "Risks"));
    expect(check.passes).toBe(false);
    expect(check.rows.filter((r) => !r.ok).map((r) => `${r.label}: ${r.note}`)).toEqual([
      "## Scope: missing, or out of order",
      "## Risks: missing, or out of order",
    ]);
  });

  test("criteria written as prose fail the numbered row, not the heading", () => {
    const check = specStructure(SPEC.replace("1. It adds.\n2. It subtracts.", "It works."));
    expect(check.rows.filter((r) => !r.ok)).toEqual([
      { label: "Numbered criteria", ok: false, note: 'none parsed (want "1. ...")' },
    ]);
  });

  test("an empty criteria section says so on its heading", () => {
    const check = specStructure(SPEC.replace("1. It adds.\n2. It subtracts.\n", ""));
    expect(check.rows.find((r) => r.label === "## Acceptance criteria")).toEqual({
      label: "## Acceptance criteria",
      ok: false,
      note: "no content",
    });
  });
});

describe("ticketStructure", () => {
  test("a well-formed ticket ticks every row and lists what it covers", () => {
    const check = ticketStructure(TICKET);
    expect(check.passes).toBe(true);
    expect(check.rows).toHaveLength(11);
    expect(check.rows.at(-1)).toEqual({ label: "Criteria covered", ok: true, note: "1, 3" });
  });

  test("a missing header line and an empty section each fail their own row", () => {
    const check = ticketStructure(
      TICKET.replace("Allowed-Files: a.go\n", "").replace("## Goal\ng", "## Goal"),
    );
    expect(check.passes).toBe(false);
    expect(check.rows.filter((r) => !r.ok)).toEqual([
      { label: "Allowed-Files:", ok: false, note: "missing" },
      { label: "## Goal", ok: false, note: "no content" },
    ]);
  });

  test("a covered line that is not a number is quoted", () => {
    const check = ticketStructure(TICKET.replace("- 3", "- criterion three"));
    expect(check.rows.at(-1)).toEqual({
      label: "Criteria covered",
      ok: false,
      note: '"- criterion three" is not "- N"',
    });
  });

  test("the container heading may have nothing directly under it", () => {
    expect(ticketStructure(TICKET).rows.find((r) => r.label === "## Plan")?.ok).toBe(true);
  });
});
