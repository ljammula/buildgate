import {
  cardAge,
  cardAlert,
  cardFacts,
  cardSince,
  needsYouAsk,
  queueLine,
} from "@/domain/boardCard";
import { columnForRequest } from "@/domain/boardColumns";
import { decodeRequestList, decodeRequestSummary } from "@/domain/request";
import { readFixtureJson } from "@/test/fixtures";
import { requestJson, ticketJson } from "@/test/requestFixtures";

const wire = (o: Parameters<typeof requestJson>[0], extra: Record<string, unknown> = {}) =>
  decodeRequestSummary({ ...requestJson(o), ...extra }, "test");

const fixture = (id: string) => {
  const found = decodeRequestList(readFixtureJson("api/requests.json"), "GET /requests").find(
    (r) => r.id === id,
  );
  if (found === undefined) throw new Error(`no fixture request ${id}`);
  return found;
};

describe("queueLine", () => {
  test("names the server's position and what the request is behind", () => {
    expect(queueLine(wire({ id: "a", state: "submitted" }, { queue_position: 2 }))).toBe(
      "Queued · position 2",
    );
    expect(
      queueLine(
        wire({ id: "a", state: "building" }, { queue_position: 1, waiting_on: "req-ahead" }),
      ),
    ).toBe("Queued · position 1 · behind req-ahead");
    expect(queueLine(wire({ id: "a", state: "building" }, { waiting_on: "run-9" }))).toBe(
      "Queued behind run-9",
    );
  });

  test("is null for a request that is not waiting", () => {
    expect(queueLine(wire({ id: "a", state: "building" }))).toBeNull();
  });
});

describe("cardFacts", () => {
  test("Drafting: the state, the running job, and the queue position", () => {
    const drafting = wire(
      { id: "a", state: "planning" },
      { active_job: { stage: "planning", role: "planning", model: "luna", harness: "pi" } },
    );
    expect(cardFacts(drafting, "drafting").lines).toEqual([
      "Planning",
      "planning role · luna · harness pi",
    ]);
    const queued = wire({ id: "b", state: "submitted" }, { queue_position: 3 });
    expect(cardFacts(queued, "drafting")).toMatchObject({
      lines: ["Submitted"],
      queue: "Queued · position 3",
    });
  });

  test("Drafting: a job left behind by another stage is not shown as running", () => {
    const stale = wire(
      { id: "a", state: "planning" },
      { active_job: { stage: "spec_drafting", role: "planning", model: "luna" } },
    );
    expect(cardFacts(stale, "drafting").lines).toEqual(["Planning"]);
  });

  test("Building: ticket n of m, the stage and the stalled verdict, from the fixture's build", () => {
    const building = fixture("req-building");
    expect(columnForRequest(building)).toBe("building");
    expect(cardFacts(building, "building")).toMatchObject({
      lines: ["Ticket 2 of 2", "build"],
      stalled: true,
      queue: null,
    });
  });

  test("Building: the round, with its limit when the server sends one", () => {
    const build = { run_id: "run-1", ticket: 1, tickets: 3, stage: "verify", round: 2 };
    expect(cardFacts(wire({ id: "a", state: "building" }, { build }), "building").lines).toEqual([
      "Ticket 1 of 3",
      "Round 2",
      "verify",
    ]);
    expect(
      cardFacts(
        wire({ id: "a", state: "building" }, { build: { ...build, max_rounds: 3 } }),
        "building",
      ).lines,
    ).toEqual(["Ticket 1 of 3", "Round 2 of 3", "verify"]);
  });

  test("Building: before its run starts, the queue position and the request's own ticket count", () => {
    const waiting = wire(
      { id: "a", state: "building", ticketIndex: 1, ticketCount: 2 },
      { queue_position: 1, waiting_on: "req-ahead" },
    );
    expect(cardFacts(waiting, "building")).toMatchObject({
      lines: ["Ticket 1 of 2"],
      queue: "Queued · position 1 · behind req-ahead",
      stalled: false,
    });
  });

  test("Needs you: one line of what is asked, and the reason of a quarantine in red", () => {
    expect(cardFacts(fixture("req-spec-review"), "needsYou")).toMatchObject({
      lines: ["Review the drafted spec."],
      alert: null,
    });
    const quarantined = cardFacts(fixture("req-quarantined"), "needsYou");
    expect(quarantined.lines).toEqual(["Retry, send back or cancel."]);
    expect(quarantined.alert?.label).toBe("Quarantined");
    expect(quarantined.alert?.reason).toMatch(
      /^ticket 1 was quarantined: verify failed after 3 ro/,
    );
  });

  test("PR review and Done: a pull request per ticket, and the review rounds they took", () => {
    const reviewing = wire({
      id: "a",
      state: "pr_review",
      tickets: [
        {
          ...ticketJson({ index: 1, prState: "draft" }),
          rounds: [{ index: 1, run_id: "run-2", outcome: "accepted", at: "2026-09-10T09:40:00Z" }],
        },
        ticketJson({ index: 2 }),
      ],
    });
    expect(cardFacts(reviewing, "prReview")).toMatchObject({
      lines: ["1 review round"],
      pullRequests: [
        {
          ticket: 1,
          prState: "draft",
          url: "https://github.com/acme/app/pull/1",
          reviewRounds: 1,
        },
      ],
    });
    const done = cardFacts(fixture("req-done"), "done");
    expect(done.lines).toEqual([]);
    expect(done.pullRequests.map((pr) => pr.prState)).toEqual(["merged"]);
    expect(cardFacts(wire({ id: "c", state: "cancelled" }), "done").lines).toEqual(["Cancelled"]);
  });

  test("a pull request address that is not http(s) is never a link", () => {
    const request = wire({
      id: "a",
      state: "done",
      tickets: [{ index: 1, pr_url: "javascript:alert(1)", pr_state: "merged" }],
    });
    expect(cardFacts(request, "done").pullRequests[0]?.url).toBeNull();
  });
});

describe("needsYouAsk", () => {
  test("a fixed sentence per state the console knows", () => {
    expect(needsYouAsk(wire({ id: "a", state: "plan_review" }))).toBe(
      "Review the drafted tickets.",
    );
    expect(needsYouAsk(wire({ id: "a", state: "oracle_review" }))).toBe(
      "Review the drafted acceptance tests.",
    );
    expect(needsYouAsk(wire({ id: "a", state: "resume_review" }))).toBe(
      "Decide how to resume the step a lost worker left.",
    );
    expect(needsYouAsk(wire({ id: "a", state: "pr_review" }))).toBe(
      "Review and merge its pull requests.",
    );
  });

  test("an accepted ticket with no pull request asks for the pull request", () => {
    expect(needsYouAsk(fixture("req-halted"))).toBe(
      "Open its pull request: retry, or merge by hand.",
    );
  });

  test("a state it does not know takes the first line of the server's next action", () => {
    expect(
      needsYouAsk(
        wire(
          { id: "a", state: "security_review" },
          { next_action: "Sign off the scan.\nThen wait." },
        ),
      ),
    ).toBe("Sign off the scan.");
    expect(needsYouAsk(wire({ id: "a", state: "security_review" }))).toBe(
      "This request waits on you.",
    );
    // An invisible character in the server's text is shown, not obeyed.
    expect(
      needsYouAsk(wire({ id: "a", state: "security_review" }, { next_action: "Sign\u202eoff" })),
    ).not.toContain("\u202e");
  });
});

describe("cardAlert", () => {
  test("a halt names its reason's first line", () => {
    const halted = wire({ id: "a", state: "halted" }, { error: "gh: not logged in\nrun gh auth" });
    expect(cardAlert(halted)).toEqual({ label: "Halted", reason: "gh: not logged in" });
  });

  test("a quarantine with no error names its check", () => {
    const quarantined = wire({ id: "a", state: "quarantined" }, { quarantine_check: "verify" });
    expect(cardAlert(quarantined)).toEqual({ label: "Quarantined", reason: "check: verify" });
  });

  test("accepted work that only lacks its pull request is not marked as a failure", () => {
    expect(cardAlert(fixture("req-halted"))).toBeNull();
  });

  test("no other state is marked", () => {
    expect(cardAlert(wire({ id: "a", state: "spec_review" }, { error: "left over" }))).toBeNull();
  });
});

describe("cardAge", () => {
  const now = new Date("2026-09-10T10:00:00Z");

  test("counts from the wait's start in Needs you, from the state's entry elsewhere", () => {
    const waiting = wire({
      id: "a",
      state: "spec_review",
      enteredAt: "2026-09-10T09:00:00Z",
      waitingSince: "2026-09-10T09:15:00Z",
    });
    expect(cardSince(waiting, "needsYou")).toBe("2026-09-10T09:15:00Z");
    expect(cardAge(waiting, "needsYou", now)).toBe("for 45m");
    const building = wire({ id: "b", state: "building", enteredAt: "2026-09-10T07:55:00Z" });
    expect(cardAge(building, "building", now)).toBe("for 2h 05m");
  });

  test("under a minute is just now; no usable time is no age", () => {
    const fresh = wire({ id: "a", state: "building", enteredAt: "2026-09-10T09:59:30Z" });
    expect(cardAge(fresh, "building", now)).toBe("just now");
    expect(
      cardAge(wire({ id: "a", state: "building", enteredAt: "" }), "building", now),
    ).toBeNull();
  });
});
