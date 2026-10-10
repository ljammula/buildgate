import { type RequestSummary, decodeRequestSummary } from "@/domain/request";

import {
  approvedLine,
  contentFiles,
  currentContentFor,
  editBlockedReason,
  editChangeNotice,
  isAutomatedActor,
  nextActionText,
  pipelineOutcome,
  terminalEntries,
  terminalEntryLine,
  parseAcceptanceCriteria,
  pipelineSteps,
  recoveryPlan,
  rejectionHeading,
  resumePlan,
  revisionDiffText,
  showsNextBanner,
  stepShowsDetail,
  ticketsLeadContent,
} from "./requestDetailLogic";
import { historyWire, requestWire, ticketWire } from "./testRequests";

function req(state: string, rest: Record<string, unknown> = {}): RequestSummary {
  return decodeRequestSummary(requestWire({ state, ...rest }), "test");
}

describe("parseAcceptanceCriteria", () => {
  test("lists the numbered items under the heading and stops at the next heading", () => {
    const spec =
      "# Spec\n\n## Acceptance criteria\n\n1. First\n2) Second  \n  3. Third\n\n## Non-goals\n\n4. Not a criterion\n";
    expect(parseAcceptanceCriteria(spec)).toEqual(["First", "Second", "Third"]);
  });

  test("matches the heading at any level, in any case", () => {
    expect(parseAcceptanceCriteria("### ACCEPTANCE CRITERIA\n1. x")).toEqual(["x"]);
  });

  test("is tolerant: no heading, or no numbered items, is an empty list", () => {
    expect(parseAcceptanceCriteria("1. not under a heading")).toEqual([]);
    expect(parseAcceptanceCriteria("## Acceptance criteria\n\nprose only\n")).toEqual([]);
    expect(parseAcceptanceCriteria("")).toEqual([]);
  });

  test("only the exact heading counts", () => {
    expect(parseAcceptanceCriteria("## Acceptance criteria notes\n1. x")).toEqual([]);
  });
});

describe("currentContentFor", () => {
  const request = req("plan_review", {
    spec: "SPEC",
    tickets: [
      ticketWire({
        index: 1,
        specPath: "/data/requests/req-1/tickets/001.spec.md",
        content: "ONE",
      }),
      ticketWire({
        index: 2,
        specPath: "C:\\data\\requests\\req-1\\tickets\\002.spec.md",
        content: "TWO",
      }),
    ],
  });

  test("spec.md is the request's spec", () => {
    expect(currentContentFor(request, "spec.md")).toBe("SPEC");
  });

  test("a relative ticket key matches an absolute spec path by suffix, with either separator", () => {
    expect(currentContentFor(request, "tickets/001.spec.md")).toBe("ONE");
    expect(currentContentFor(request, "tickets/002.spec.md")).toBe("TWO");
  });

  test("a path that is only a suffix of a longer file name does not match", () => {
    expect(currentContentFor(request, "01.spec.md")).toBe("");
    expect(currentContentFor(request, "tickets/003.spec.md")).toBe("");
  });
});

test("revisionDiffText diffs each snapshotted file against its current content", () => {
  const request = req("spec_review", { spec: "new\n" });
  const text = revisionDiffText(request, {
    index: 2,
    at: "",
    by: "",
    reason: "",
    fromState: "spec_review",
    files: { "spec.md": "old\n" },
  });
  expect(text.startsWith("--- spec.md (revision 2)\n+++ spec.md (current)\n")).toBe(true);
  expect(text).toContain("- old");
  expect(text).toContain("+ new");
});

describe("next action", () => {
  test("the server's sentence wins, then the awaiting-PR label, else nothing", () => {
    expect(nextActionText(req("building", { next_action: "Wait." }))).toBe("Wait.");
    expect(nextActionText(req("halted", { halt_kind: "accepted_no_pr", id: "r9" }))).toMatch(
      /^Accepted, awaiting pull request: .*factoryd retry r9/,
    );
    expect(nextActionText(req("building"))).toBeNull();
  });

  test.each([
    ["halted", false],
    ["quarantined", false],
    ["resume_review", false],
    ["spec_review", false],
    ["oracle_review", false],
    ["plan_review", false],
    ["building", true],
    ["done", true],
  ])("the banner for %s: %s", (state, shown) => {
    expect(showsNextBanner(req(state))).toBe(shown);
  });
});

test("tickets lead the content in every state but the three review states", () => {
  const tickets = [ticketWire({ index: 1 })];
  for (const state of ["spec_review", "oracle_review", "plan_review"]) {
    expect(ticketsLeadContent(req(state, { tickets }))).toBe(false);
  }
  for (const state of [
    "building",
    "pr_review",
    "halted",
    "quarantined",
    "done",
    "cancelled",
    "resume_review",
  ]) {
    expect(ticketsLeadContent(req(state, { tickets }))).toBe(true);
  }
  expect(ticketsLeadContent(req("building"))).toBe(false);
});

describe("contentFiles", () => {
  test("a ticket's plan takes precedence over spec.md and is editable only at plan_review with write access", () => {
    const tickets = [ticketWire({ index: 3, content: "x" }), ticketWire({ index: 4 })];
    const review = req("plan_review", { spec: "S", tickets });
    expect(contentFiles(review, true).map((f) => [f.id, f.editable])).toEqual([["ticket-3", true]]);
    expect(contentFiles(review, false)[0]?.editable).toBe(false);
    expect(contentFiles(req("building", { spec: "S", tickets }), true)[0]?.editable).toBe(false);
  });

  test("spec.md is editable only at spec_review with write access; nothing before a spec exists", () => {
    expect(contentFiles(req("spec_review", { spec: "S" }), true)).toMatchObject([
      { id: "spec", ticket: null, editable: true },
    ]);
    expect(contentFiles(req("spec_review", { spec: "S" }), false)[0]?.editable).toBe(false);
    expect(contentFiles(req("planning", { spec: "S" }), true)[0]?.editable).toBe(false);
    expect(contentFiles(req("spec_drafting"), true)).toEqual([]);
  });
});

describe("recoveryPlan", () => {
  test("a plain halt leads with Retry and its CLI command", () => {
    const plan = recoveryPlan(req("halted"));
    expect(plan).toMatchObject({
      headline: "This request is halted.",
      retryLabel: "Retry request",
      showSendBack: false,
      sendBackIsPrimary: false,
      cliEquivalent: "factoryd retry req-1",
      overrideTicket: null,
    });
    expect(plan.explanation).toBe(
      "Retry the request to move it forward again, or cancel it to abandon it.",
    );
  });

  test("accepted-awaiting-PR is neutral and its Retry says it rebuilds", () => {
    expect(recoveryPlan(req("halted", { halt_kind: "accepted_no_pr" }))).toMatchObject({
      awaitingPullRequest: true,
      headline: "Built and verified; no pull request was opened.",
      retryLabel: "Retry request (rebuilds)",
    });
  });

  test("a spec_conformity quarantine leads with Send back to spec, even when planning is allowed", () => {
    const plan = recoveryPlan(
      req("quarantined", {
        can_send_back: true,
        can_send_back_to_plan: true,
        quarantine_check: "spec_conformity",
      }),
    );
    expect(plan).toMatchObject({
      sendBackLabel: "Send back to spec",
      sendBackIsPrimary: true,
      cliEquivalent: 'factoryd reject -to spec -reason "<what to change>" req-1',
    });
  });

  test("another quarantine sends back to planning when allowed, else to spec, and Retry leads", () => {
    expect(
      recoveryPlan(req("quarantined", { can_send_back: true, can_send_back_to_plan: true })),
    ).toMatchObject({
      sendBackLabel: "Send back to planning",
      sendBackIsPrimary: false,
      cliEquivalent: "factoryd retry req-1",
    });
    expect(recoveryPlan(req("quarantined", { can_send_back: true })).sendBackLabel).toBe(
      "Send back to spec",
    );
  });

  test("the override link targets the ticket at the current index, else the first with a run", () => {
    const tickets = [
      ticketWire({ index: 1, runId: "run-a" }),
      ticketWire({ index: 2, runId: "run-b" }),
      ticketWire({ index: 3 }),
    ];
    expect(
      recoveryPlan(req("quarantined", { tickets, ticket_index: 2 })).overrideTicket?.runId,
    ).toBe("run-b");
    expect(
      recoveryPlan(req("quarantined", { tickets, ticket_index: 3 })).overrideTicket?.runId,
    ).toBe("run-a");
    expect(recoveryPlan(req("halted", { tickets, ticket_index: 2 })).overrideTicket).toBeNull();
  });
});

describe("resumePlan", () => {
  test("names the lost step and prefers the error over next_action as the prompt", () => {
    const plan = resumePlan(
      req("resume_review", {
        error: "worker stopped",
        next_action: "n",
        resume: { from_state: "building", refused: ["why"] },
      }),
    );
    expect(plan).toEqual({
      headline: "The Building step was lost when the worker stopped.",
      prompt: "worker stopped",
      refused: ["why"],
      isBuild: true,
    });
  });

  test("an unknown lost step is offered both decisions; a drafting step is not a build", () => {
    expect(resumePlan(req("resume_review"))).toMatchObject({
      headline: "A step was lost when the worker stopped.",
      isBuild: true,
    });
    expect(
      resumePlan(req("resume_review", { resume: { from_state: "spec_drafting" } })).isBuild,
    ).toBe(false);
  });
});

describe("pipelineSteps", () => {
  const at = (n: number) => `2026-09-15T09:0${n}:00Z`;

  test("steps before the current one are done, after it pending; the latest entry reaching a step is shown", () => {
    const steps = pipelineSteps(
      req("spec_review", {
        history: [
          historyWire({ from: "submitted", to: "spec_drafting", at: at(0), by: "a" }),
          historyWire({
            from: "spec_drafting",
            to: "spec_review",
            at: at(1),
            by: "b",
            reason: "first",
          }),
          historyWire({ from: "spec_review", to: "spec_drafting", at: at(2), by: "c" }),
          historyWire({
            from: "spec_drafting",
            to: "spec_review",
            at: at(3),
            by: "d",
            reason: "second",
          }),
        ],
      }),
    );
    expect(steps.map((s) => [s.step, s.status])).toEqual([
      ["submitted", "done"],
      ["spec_drafting", "done"],
      ["spec_review", "current"],
      ["planning", "pending"],
      ["plan_review", "pending"],
      ["building", "pending"],
      ["pr_review", "pending"],
      ["done", "pending"],
    ]);
    expect(steps[2]?.entry?.reason).toBe("second");
    expect(steps[1]?.entry?.by).toBe("c");
    expect(steps[3]?.entry).toBeNull();
  });

  test("a terminal request fails the step it moved out of, with the terminal entry as its detail", () => {
    const steps = pipelineSteps(
      req("halted", {
        history: [
          historyWire({ from: "plan_review", to: "building", at: at(0), by: "j" }),
          historyWire({ from: "building", to: "halted", at: at(1), by: "f", reason: "boom" }),
        ],
      }),
    );
    const building = steps.find((s) => s.step === "building");
    expect(building).toMatchObject({ status: "failed", showsBuildProgress: false });
    expect(building?.entry?.reason).toBe("boom");
    expect(steps.find((s) => s.step === "pr_review")?.status).toBe("pending");
    expect(steps.find((s) => s.step === "plan_review")?.status).toBe("done");
  });

  test("a terminal request with no history marks nothing", () => {
    expect(pipelineSteps(req("halted")).every((s) => s.status === "pending")).toBe(true);
  });

  test("accepted-awaiting-PR needs the operator rather than failing", () => {
    const steps = pipelineSteps(
      req("halted", {
        halt_kind: "accepted_no_pr",
        history: [historyWire({ from: "pr_review", to: "halted", at: at(1), by: "f" })],
      }),
    );
    expect(steps.find((s) => s.step === "pr_review")?.status).toBe("needsYou");
  });

  test("resume_review marks the lost step as needing the operator", () => {
    const steps = pipelineSteps(req("resume_review", { resume: { from_state: "building" } }));
    expect(steps.find((s) => s.step === "building")?.status).toBe("needsYou");
    expect(steps.find((s) => s.step === "planning")?.status).toBe("done");
  });

  test("the building step shows build progress while current", () => {
    expect(
      pipelineSteps(req("building")).find((s) => s.step === "building")?.showsBuildProgress,
    ).toBe(true);
  });

  test("the oracle steps appear for -draft-oracles, for a visit in the history, or at the state itself", () => {
    const has = (r: RequestSummary) => pipelineSteps(r).some((s) => s.step === "oracle_drafting");
    expect(has(req("spec_review"))).toBe(false);
    expect(has(req("spec_review", { draft_oracles: true }))).toBe(true);
    expect(has(req("oracle_review"))).toBe(true);
    expect(
      has(
        req("planning", {
          history: [
            historyWire({ from: "spec_review", to: "oracle_drafting", at: at(0), by: "f" }),
          ],
        }),
      ),
    ).toBe(true);
  });
});

test("audit and rejection lines", () => {
  expect(approvedLine("jane", "not a time")).toBe("Approved by jane at not a time");
  const base = { by: "bob", at: "x", fromState: "spec_review" };
  expect(rejectionHeading({ ...base, forStage: null })).toBe(
    "Rejected by bob at x (from spec_review)",
  );
  expect(rejectionHeading({ ...base, forStage: "plan_review" })).toBe(
    "Sent back by bob at x (from spec_review, for plan_review)",
  );
});

describe("stepShowsDetail", () => {
  const entry = (by: string) => ({
    from: "a",
    to: "b",
    at: "2026-09-10T09:00:00Z",
    by,
    reason: "",
  });

  test("a pending step is a bare line", () => {
    expect(stepShowsDetail({ status: "pending", entry: null })).toBe(false);
  });

  test("a completed step made by the factory is one line, whichever spelling it used", () => {
    expect(stepShowsDetail({ status: "done", entry: entry("factory") })).toBe(false);
    expect(stepShowsDetail({ status: "done", entry: entry("factoryd") })).toBe(false);
    expect(stepShowsDetail({ status: "done", entry: null })).toBe(false);
  });

  test("a completed step an operator decided keeps who and when", () => {
    expect(stepShowsDetail({ status: "done", entry: entry("jane") })).toBe(true);
  });

  test("the current, failed and needs-you steps always keep their lines", () => {
    for (const status of ["current", "failed", "needsYou"] as const) {
      expect(stepShowsDetail({ status, entry: null })).toBe(true);
    }
  });
});

describe("isAutomatedActor", () => {
  test("only a person's name is not automated", () => {
    expect(isAutomatedActor("factory")).toBe(true);
    expect(isAutomatedActor("factoryd")).toBe(true);
    expect(isAutomatedActor("")).toBe(true);
    expect(isAutomatedActor("jane")).toBe(false);
  });
});

// The history of a real cancelled request (spec drafting halted and retried,
// built, quarantined, then cancelled by the operator), as the server sent it.
const cancelledHistory = [
  { from: "submitted", to: "spec_drafting", at: "2026-10-06T03:55:07Z", by: "factory" },
  {
    from: "spec_drafting",
    to: "halted",
    at: "2026-10-06T04:00:00Z",
    by: "factory",
    reason: "spec drafting failed: draft_spec.py exited 2",
  },
  {
    from: "halted",
    to: "spec_drafting",
    at: "2026-10-06T04:03:35Z",
    by: "dogfood-operator",
    reason: "retried after: spec drafting failed",
  },
  {
    from: "spec_drafting",
    to: "spec_review",
    at: "2026-10-06T04:07:10Z",
    by: "factory",
    reason: "spec drafted",
  },
  {
    from: "spec_review",
    to: "planning",
    at: "2026-10-06T04:07:21Z",
    by: "dogfood-operator",
    reason: "approved",
  },
  {
    from: "planning",
    to: "plan_review",
    at: "2026-10-06T04:07:54Z",
    by: "factory",
    reason: "plan drafted: 1 tickets",
  },
  {
    from: "plan_review",
    to: "building",
    at: "2026-10-06T04:07:59Z",
    by: "dogfood-operator",
    reason: "approved",
  },
  {
    from: "building",
    to: "quarantined",
    at: "2026-10-06T04:13:12Z",
    by: "factory",
    reason: "ticket 1/1 quarantined: required_files_changed: internal/domain/domain.go untouched",
  },
  {
    from: "quarantined",
    to: "cancelled",
    at: "2026-10-06T04:13:53Z",
    by: "dogfood-operator",
    reason: "The modulo operation already exists in domain.go; the ticket was redundant.",
  },
];

describe("a request cancelled out of quarantine", () => {
  const request = req("cancelled", { history: cancelledHistory });

  test("steps before the one it stopped at are done, that step is stopped, the rest pending", () => {
    const status = Object.fromEntries(pipelineSteps(request).map((s) => [s.step, s.status]));
    expect(status).toEqual({
      submitted: "done",
      spec_drafting: "done",
      spec_review: "done",
      planning: "done",
      plan_review: "done",
      building: "stopped",
      pr_review: "pending",
      done: "pending",
    });
  });

  test("the stopped step carries the move that took it out of the step, not the cancel", () => {
    const building = pipelineSteps(request).find((s) => s.step === "building");
    expect(building?.entry?.to).toBe("quarantined");
  });

  test("the outcome names the terminal state with who, when and why", () => {
    const outcome = pipelineOutcome(request);
    expect(outcome?.label).toBe("Cancelled");
    expect(outcome?.entry?.by).toBe("dogfood-operator");
    expect(outcome?.entry?.reason).toContain("already exists");
    expect(outcome?.reasonShownAtStep).toBe(false);
  });

  test("the audit lists every halt, quarantine and cancel, oldest first", () => {
    const lines = terminalEntries(request).map((e) => terminalEntryLine(e).split(" at ")[0]);
    expect(lines).toEqual([
      "Halted by factory",
      "Quarantined by factory",
      "Cancelled by dogfood-operator",
    ]);
  });
});

describe("a halted request", () => {
  test("is failed at the step it halted in, and its outcome does not repeat that step's reason", () => {
    const request = req("halted", {
      history: [
        { from: "submitted", to: "spec_drafting", at: "2026-10-06T03:55:07Z", by: "factory" },
        {
          from: "spec_drafting",
          to: "halted",
          at: "2026-10-06T04:00:00Z",
          by: "factory",
          reason: "boom",
        },
      ],
    });
    expect(pipelineSteps(request).find((s) => s.status === "failed")?.step).toBe("spec_drafting");
    expect(pipelineOutcome(request)?.reasonShownAtStep).toBe(true);
  });

  test("accepted with no pull request is needs-you and has no outcome row", () => {
    const request = req("halted", {
      halt_kind: "accepted_no_pr",
      history: [
        { from: "building", to: "halted", at: "2026-10-06T04:00:00Z", by: "factory", reason: "x" },
      ],
    });
    expect(pipelineSteps(request).find((s) => s.step === "building")?.status).toBe("needsYou");
    expect(pipelineOutcome(request)).toBeNull();
  });
});

describe("retry label", () => {
  const awaiting = {
    halt_kind: "accepted_no_pr",
    ticket_count: 1,
    tickets: [ticketWire({ index: 1, runId: "r1" })],
  };

  test("says nothing about a rebuild when the server says there is none", () => {
    const plan = recoveryPlan(
      req("halted", {
        ...awaiting,
        next_action: "`factoryd retry x` re-attempts only the PR (no rebuild)",
      }),
    );
    expect(plan.retryLabel).toBe("Retry request (opens the PR only)");
  });

  test("keeps the rebuild warning when the server is silent, and the plain name for any other halt", () => {
    expect(recoveryPlan(req("halted", awaiting)).retryLabel).toBe("Retry request (rebuilds)");
    expect(recoveryPlan(req("halted", { next_action: "no rebuild here" })).retryLabel).toBe(
      "Retry request",
    );
  });
});

describe("editor notices", () => {
  test("a changed request states its new state; a locked file says Save is off", () => {
    expect(editChangeNotice({ state: "planning", contentChanged: false, editable: false })).toBe(
      "This request changed while you were editing: it is now Planning. " +
        "This file can no longer be edited in this state, so Save is off; your text stays here to copy.",
    );
    expect(
      editChangeNotice({ state: "spec_review", contentChanged: true, editable: true }),
    ).toContain("Save will show you the difference.");
    expect(editBlockedReason("planning", true)).toBeNull();
    expect(editBlockedReason("planning", false)).toBe(
      "Save is off: this file can no longer be edited (the request is now Planning).",
    );
  });
});
