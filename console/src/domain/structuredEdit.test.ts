import {
  requiredSpecHeadings,
  requiredTicketHeadings,
  requiredTicketHeaderKeys,
  specAcceptanceCriteria,
  validateTicketPlan,
} from "@/domain/specSkeleton";
import { readFixtureJson } from "@/test/fixtures";

import {
  addCriterion,
  criteriaItems,
  headerList,
  moveCriterion,
  removeCriterion,
  sectionBodyProblem,
  sections,
  setCoveredCriteria,
  setSectionBody,
  setCriterionBody,
  setTicketHeaderValue,
  ticketHeaderValue,
} from "./structuredEdit";

const SPEC = [
  "# Spec",
  "",
  "## Problem",
  "",
  "p",
  "",
  "## Acceptance criteria",
  "",
  "1. It adds",
  "   two numbers:",
  "   1) ints",
  "",
  "2. It subtracts.",
  "",
  "3. It multiplies.",
  "",
  "## Risks",
  "",
  "r",
  "",
].join("\n");

const vectors = readFixtureJson("vectors/spec-skeleton.json") as {
  specs: { name: string; text: string }[];
  tickets: { name: string; text: string }[];
};

describe("criteriaItems", () => {
  test("reads each criterion without its number, continuation lines as written", () => {
    expect(criteriaItems(SPEC)).toEqual([
      { body: "It adds\n   two numbers:\n   1) ints" },
      { body: "It subtracts." },
      { body: "It multiplies." },
    ]);
  });

  test("is null without the heading and empty under an empty one", () => {
    expect(criteriaItems("# Spec\n")).toBeNull();
    expect(criteriaItems("## Acceptance criteria\n\nprose\n")).toEqual([]);
  });

  // The server's reader and this one must cut the list at the same places.
  test.each(vectors.specs)("agrees with the server's criteria on: $name", ({ text }) => {
    const items = criteriaItems(text) ?? [];
    expect(items).toHaveLength(specAcceptanceCriteria(text).length);
  });
});

describe("an edit that changes nothing returns the same text", () => {
  test.each(vectors.specs)("spec: $name", ({ text }) => {
    (criteriaItems(text) ?? []).forEach((item, i) => {
      expect(setCriterionBody(text, i, item.body)).toBe(text);
    });
    expect(setCriterionBody(text, 99, "x")).toBe(text);
    expect(removeCriterion(text, 99)).toBe(text);
    expect(moveCriterion(text, 0, -1)).toBe(text);
  });

  test.each(vectors.tickets)("ticket: $name", ({ text }) => {
    for (const key of requiredTicketHeaderKeys) {
      const value = ticketHeaderValue(text, key);
      if (value !== null && text.includes(`${key} ${value}`)) {
        expect(setTicketHeaderValue(text, key, value, requiredTicketHeaderKeys)).toBe(text);
      }
    }
    const plan = validateTicketPlan(text);
    if (plan.ok && text.includes(plan.covered.map((n) => `- ${n}`).join("\n"))) {
      expect(setCoveredCriteria(text, plan.covered)).toBe(text);
    }
  });
});

describe("setCriterionBody", () => {
  test("rewrites only that criterion's lines", () => {
    expect(setCriterionBody(SPEC, 1, "It subtracts\n   exactly.")).toBe(
      SPEC.replace("2. It subtracts.", "2. It subtracts\n   exactly."),
    );
  });

  test("keeps the criterion's own number, delimiter and spacing", () => {
    const text = "## Acceptance criteria\n7)  Odd.\n";
    expect(setCriterionBody(text, 0, "Even.")).toBe("## Acceptance criteria\n7)  Even.\n");
  });

  test("a continuation that would read as a new criterion is indented", () => {
    const out = setCriterionBody(SPEC, 1, "It subtracts.\n9. Not a new one.");
    expect(out).toContain("2. It subtracts.\n   9. Not a new one.\n");
    expect(specAcceptanceCriteria(out)).toHaveLength(3);
  });

  test("keeps CRLF line endings", () => {
    const text = "## Acceptance criteria\r\n1. One.\r\n2. Two.\r\n";
    expect(setCriterionBody(text, 0, "Uno.\n   more")).toBe(
      "## Acceptance criteria\r\n1. Uno.\r\n   more\r\n2. Two.\r\n",
    );
  });
});

describe("addCriterion", () => {
  test("appends after the last one, spaced as the list is, numbered next", () => {
    expect(addCriterion(SPEC, "It divides.")).toBe(
      SPEC.replace("3. It multiplies.\n", "3. It multiplies.\n\n4. It divides.\n"),
    );
  });

  test("a tight list stays tight and keeps its delimiter", () => {
    expect(addCriterion("## Acceptance criteria\n1) a\n2) b\n\n## Risks\n", "c")).toBe(
      "## Acceptance criteria\n1) a\n2) b\n3) c\n\n## Risks\n",
    );
  });

  test("starts a list in an empty section, set off by blank lines", () => {
    expect(addCriterion("## Acceptance criteria\n\n## Risks\n", "First.")).toBe(
      "## Acceptance criteria\n\n1. First.\n\n## Risks\n",
    );
    expect(addCriterion("## Acceptance criteria\nprose\n## Risks\n", "First.")).toBe(
      "## Acceptance criteria\nprose\n\n1. First.\n\n## Risks\n",
    );
  });

  test("extends a file that ends on its last criterion without a newline", () => {
    expect(addCriterion("## Acceptance criteria\n1. a", "b")).toBe(
      "## Acceptance criteria\n1. a\n2. b",
    );
    expect(addCriterion("## Acceptance criteria\r\n1. a\r\n2. b", "c")).toBe(
      "## Acceptance criteria\r\n1. a\r\n2. b\r\n3. c",
    );
  });

  test("does nothing without the heading", () => {
    expect(addCriterion("# Spec\n", "x")).toBe("# Spec\n");
  });
});

describe("removeCriterion", () => {
  test("drops the criterion with its spacing and renumbers the rest", () => {
    expect(removeCriterion(SPEC, 0)).toBe(
      SPEC.replace("1. It adds\n   two numbers:\n   1) ints\n\n", "")
        .replace("2. It subtracts.", "1. It subtracts.")
        .replace("3. It multiplies.", "2. It multiplies."),
    );
  });

  test("removing the last one keeps the blank line before the next heading", () => {
    expect(removeCriterion(SPEC, 2)).toBe(SPEC.replace("\n3. It multiplies.\n", ""));
  });

  test("removing the only one leaves the section empty", () => {
    expect(removeCriterion("## Acceptance criteria\n\n1. a\n\n## Risks\n", 0)).toBe(
      "## Acceptance criteria\n\n\n## Risks\n",
    );
  });
});

describe("moveCriterion", () => {
  test("swaps neighbours whole, continuation lines included, and renumbers", () => {
    const moved = moveCriterion(SPEC, 0, 1);
    expect(moved).toContain(
      "\n\n1. It subtracts.\n\n2. It adds\n   two numbers:\n   1) ints\n\n3. It multiplies.\n\n## Risks",
    );
    expect(moveCriterion(moved, 1, -1)).toBe(SPEC);
  });

  test("does nothing at either end", () => {
    expect(moveCriterion(SPEC, 0, -1)).toBe(SPEC);
    expect(moveCriterion(SPEC, 2, 1)).toBe(SPEC);
  });

  test("a CRLF file ending on its last criterion keeps its line endings", () => {
    expect(moveCriterion("## Acceptance criteria\r\n1. a\r\n2. b", 0, 1)).toBe(
      "## Acceptance criteria\r\n1. b\r\n2. a",
    );
  });

  test("everything outside the section is untouched", () => {
    const [head] = SPEC.split("## Acceptance criteria");
    const tail = SPEC.slice(SPEC.indexOf("## Risks"));
    const moved = moveCriterion(SPEC, 1, 1);
    expect(moved.startsWith(`${head}## Acceptance criteria\n`)).toBe(true);
    expect(moved.endsWith(tail)).toBe(true);
  });
});

const TICKET = [
  "Verify-Command: go test ./...",
  "Allowed-Files: a.go, a_test.go",
  "Required-Changed-Files: a.go",
  "",
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
  "",
  "- 1",
  "- 3",
  "",
  "## Out of scope",
  "none",
  "",
].join("\n");

describe("ticket header lines", () => {
  test("reads a value, trimmed; null when the line is absent or only inside a fence", () => {
    expect(ticketHeaderValue(TICKET, "Allowed-Files:")).toBe("a.go, a_test.go");
    expect(ticketHeaderValue("## Goal\n", "Allowed-Files:")).toBeNull();
    expect(ticketHeaderValue("```\nAllowed-Files: x\n```\n", "Allowed-Files:")).toBeNull();
    expect(ticketHeaderValue("  Allowed-Files: x\n", "Allowed-Files:")).toBeNull();
  });

  test("rewrites the line in place and nothing else", () => {
    expect(
      setTicketHeaderValue(TICKET, "Verify-Command:", "make verify", requiredTicketHeaderKeys),
    ).toBe(TICKET.replace("Verify-Command: go test ./...", "Verify-Command: make verify"));
  });

  test("adds a missing line after the header lines that are there, or first", () => {
    const without = TICKET.replace("Required-Changed-Files: a.go\n", "");
    expect(
      setTicketHeaderValue(without, "Required-Changed-Files:", "a.go", requiredTicketHeaderKeys),
    ).toBe(TICKET);
    expect(
      setTicketHeaderValue("## Goal\n", "Verify-Command:", "true", requiredTicketHeaderKeys),
    ).toBe("Verify-Command: true\n## Goal\n");
  });

  test("a line inside a fence is not the header: the real one is added outside it", () => {
    const text = "```\nVerify-Command: example\n```\n## Goal\n";
    expect(setTicketHeaderValue(text, "Verify-Command:", "true", requiredTicketHeaderKeys)).toBe(
      `Verify-Command: true\n${text}`,
    );
  });

  test("splits a comma-separated value", () => {
    expect(headerList(" a.go ,b/c.go,, ")).toEqual(["a.go", "b/c.go"]);
    expect(headerList("")).toEqual([]);
  });
});

describe("setCoveredCriteria", () => {
  test("rewrites the list, keeping the blank lines around it", () => {
    const out = setCoveredCriteria(TICKET, [2]);
    expect(out).toBe(TICKET.replace("- 1\n- 3\n", "- 2\n"));
    expect(validateTicketPlan(out).covered).toEqual([2]);
  });

  test("writes into an empty section", () => {
    const empty = TICKET.replace("- 1\n- 3\n\n", "");
    expect(setCoveredCriteria(empty, [1, 2])).toBe(TICKET.replace("- 1\n- 3\n\n", "- 1\n- 2\n"));
    const bare = TICKET.replace("\n- 1\n- 3\n\n", "");
    expect(setCoveredCriteria(bare, [1])).toBe(TICKET.replace("\n- 1\n- 3\n\n", "- 1\n\n"));
  });

  test("replaces a list that does not parse", () => {
    const out = setCoveredCriteria(TICKET.replace("- 3", "- the third"), [1, 3]);
    expect(out).toBe(TICKET);
  });

  test("does nothing to a ticket without the plan headings", () => {
    expect(setCoveredCriteria("## Goal\n- 1\n", [1])).toBe("## Goal\n- 1\n");
  });
});

describe("sections", () => {
  const H = ["# Spec", "## Problem", "## Scope"];
  const doc = "# Spec\n\n## Problem\n\n  p one  \n\np two\n\n## Scope\ns\n";

  test("a body is the lines after its heading, without the blank lines before the next heading", () => {
    expect(sections(doc, H)).toEqual([
      { heading: "# Spec", body: "", present: true },
      { heading: "## Problem", body: "\n  p one  \n\np two", present: true },
      { heading: "## Scope", body: "s", present: true },
    ]);
  });

  test("a missing heading is absent with an empty body", () => {
    expect(sections("# Spec\n\n## Scope\nx", H)[1]).toEqual({
      heading: "## Problem",
      body: "",
      present: false,
    });
  });

  test("a ticket's Plan body ends at its first sub-heading", () => {
    const t = "hdr: 1\n\n## Goal\ng\n\n## Plan\nintro\n\n### Files to touch\n- a\n";
    const plan = sections(t, requiredTicketHeadings)[1];
    expect(plan).toEqual({ heading: "## Plan", body: "intro", present: true });
    expect(sections(t, requiredTicketHeadings)[2]?.body).toBe("- a");
  });

  test("setSectionBody replaces exactly one body and keeps the blank lines around it", () => {
    expect(setSectionBody(doc, H, "## Problem", "new\nlines")).toBe(
      "# Spec\n\n## Problem\nnew\nlines\n\n## Scope\ns\n",
    );
    expect(setSectionBody(doc, H, "## Scope", "S2")).toBe(doc.replace("\ns\n", "\nS2\n"));
    expect(setSectionBody(doc, H, "## Problem", "")).toBe("# Spec\n\n## Problem\n\n## Scope\ns\n");
  });

  test("an empty section takes a body and keeps the blank line before the next heading", () => {
    expect(setSectionBody("# Spec\n\n## Problem\n\n## Scope\n", H, "## Problem", "x")).toBe(
      "# Spec\n\n## Problem\nx\n\n## Scope\n",
    );
  });

  test("CRLF text keeps its line endings, old lines and new", () => {
    const crlf = "# Spec\r\n\r\n## Problem\r\np\r\n\r\n## Scope\r\ns\r\n";
    expect(setSectionBody(crlf, H, "## Problem", "a\nb")).toBe(
      "# Spec\r\n\r\n## Problem\r\na\r\nb\r\n\r\n## Scope\r\ns\r\n",
    );
    expect(setSectionBody(crlf, H, "## Problem", "p")).toBe(crlf);
  });

  test("a file without a final newline stays without one", () => {
    expect(setSectionBody("# Spec\n## Problem\np\n## Scope\ns", H, "## Scope", "t")).toBe(
      "# Spec\n## Problem\np\n## Scope\nt",
    );
  });

  test("an absent heading returns the text unchanged", () => {
    const t = "# Spec\n\n## Scope\nx\n";
    expect(setSectionBody(t, H, "## Problem", "z")).toBe(t);
    expect(setSectionBody(t, H, "## Nope", "z")).toBe(t);
  });

  test("round trip: writing back a section's own body returns the identical text", () => {
    const fixtures = [
      doc,
      "# Spec\r\n\r\n## Problem\r\np\r\n\r\n## Scope\r\ns\r\n",
      "# Spec\n## Problem\np\n## Scope\ns",
      "# Spec\n\n## Scope\nx\n",
      "# Spec\n\n## Problem\n\n\n## Scope\n\n\n",
      "pre\n# Spec\n## Problem  \n a \t\n\n\nb\n\n## Scope\n",
      requiredSpecHeadings.join("\n\nx\n\n") + "\n",
      "",
    ];
    for (const text of fixtures) {
      for (const slice of sections(text, requiredSpecHeadings)) {
        expect(setSectionBody(text, requiredSpecHeadings, slice.heading, slice.body)).toBe(text);
      }
      for (const slice of sections(text, H)) {
        expect(setSectionBody(text, H, slice.heading, slice.body)).toBe(text);
      }
    }
  });

  test("sectionBodyProblem refuses a line equal to any required heading, trimmed", () => {
    expect(sectionBodyProblem(H, "fine\n### Not required")).toBeNull();
    expect(sectionBodyProblem(H, "a\n  ## Scope \nb")).toMatch(/## Scope/);
    expect(sectionBodyProblem(H, "# Spec")).not.toBeNull();
  });
});
