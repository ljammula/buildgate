import { readFixtureJson } from "@/test/fixtures";

import { planCoverage } from "./planCoverage";

interface PlanVector {
  name: string;
  spec: string;
  tickets: string[];
  criteria_count: number;
  unclaimed: number[];
}

// The "plans" section of the file internal/request's skeleton_vectors_test.go
// writes: the count and the unclaimed numbers are the server's own answers.
describe("golden vectors (test/fixtures/vectors/spec-skeleton.json, plans)", () => {
  const { plans } = readFixtureJson("vectors/spec-skeleton.json") as { plans: PlanVector[] };

  test("the section is there", () => {
    expect(plans.length).toBeGreaterThan(5);
  });

  test.each(plans)("$name", (v) => {
    const got = planCoverage(
      v.spec,
      v.tickets.map((content, i) => ({ index: i + 1, content })),
    );
    expect({ criteria_count: got.criteria.length, unclaimed: got.unclaimed }).toEqual({
      criteria_count: v.criteria_count,
      unclaimed: v.unclaimed,
    });
  });
});

const SPEC = "## Acceptance criteria\n1. It adds.\n2. It subtracts.\n3. It multiplies.\n";

function ticket(index: number, covered: string) {
  return {
    index,
    content:
      "## Goal\ng\n## Plan\n### Files to touch\n- a\n### Steps\n1. s\n### Tests to add\n- t\n" +
      `### Acceptance criteria covered\n${covered}\n## Out of scope\nnone\n`,
  };
}

test("each criterion lists the tickets that claim it, by ticket index", () => {
  const got = planCoverage(SPEC, [ticket(1, "- 1\n- 2"), ticket(4, "- 2")]);

  expect(got.criteria).toEqual([
    { number: 1, text: "1. It adds.", tickets: [1] },
    { number: 2, text: "2. It subtracts.", tickets: [1, 4] },
    { number: 3, text: "3. It multiplies.", tickets: [] },
  ]);
  expect(got.unclaimed).toEqual([3]);
  expect(got.stray).toEqual([]);
  expect(got.unreadable).toEqual([]);
});

test("a criterion is known by its position, not by the number written on it", () => {
  const got = planCoverage("## Acceptance criteria\n5. First.\n9. Second.\n", [ticket(1, "- 2")]);

  expect(got.criteria.map((c) => [c.number, c.text, c.tickets])).toEqual([
    [1, "5. First.", []],
    [2, "9. Second.", [1]],
  ]);
});

test("claims outside the spec's list, and tickets that do not parse, are reported", () => {
  const got = planCoverage(SPEC, [
    ticket(1, "- 1\n- 2\n- 3\n- 7\n- 0\n- 7"),
    ticket(2, "- the third one"),
  ]);

  expect(got.unclaimed).toEqual([]);
  expect(got.stray).toEqual([{ ticket: 1, numbers: [7, 0] }]);
  expect(got.unreadable).toEqual([2]);
});
