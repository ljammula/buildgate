import {
  REQUEST_VERBS,
  killSwitchDisplay,
  requestVerbs,
  stateLabel,
  statusForReleaseDecision,
  statusForPRState,
  statusForToken,
  statusIcon,
  statusTone,
  type Status,
} from "@/domain/status";

// Table test over every state/PR-review string literal that used to be
// hand-coloured inside one of the five pre-status chip implementations. Every
// literal here must map to the exact semantic status STATUS_BY_TOKEN assigns
// it, so a future edit that silently drifts one of these strings is caught
// here rather than discovered as a wrong-coloured chip in the running console.
describe("statusForToken maps every known chip literal", () => {
  const expected: [string, Status][] = [
    // Request state.
    ["spec_review", "needsHuman"],
    ["oracle_review", "needsHuman"],
    ["plan_review", "needsHuman"],
    ["spec_drafting", "working"],
    ["oracle_drafting", "working"],
    ["planning", "working"],
    ["building", "working"],
    ["pr_review", "working"],
    ["done", "done"],
    // Terminal request states.
    ["quarantined", "failed"],
    ["halted", "needsHuman"],
    ["resume_review", "needsHuman"],
    ["cancelled", "failed"],
    // PR review state.
    ["approved", "done"],
    ["changes_requested", "needsHuman"],
    ["merged", "done"],
    ["closed", "failed"],
    ["open", "working"],
    ["draft", "working"],
    // Run state.
    ["accepted", "done"],
    ["slice_running", "working"],
  ];

  for (const [token, status] of expected) {
    test(`${token} -> Status.${status}`, () => {
      expect(statusForToken(token)).toBe(status);
    });
  }

  test("an unmapped/unrecognized string is Status.unknown, never hidden", () => {
    expect(statusForToken("some_future_state_nobody_wrote_a_case_for")).toBe("unknown");
    expect(statusForToken("constructor")).toBe("unknown");
  });

  test("'submitted'/'ready'/'verifying' map to working (no longer " + 'unmapped/raw "?")', () => {
    expect(statusForToken("submitted")).toBe("working");
    expect(statusForToken("ready")).toBe("working");
    expect(statusForToken("verifying")).toBe("working");
  });
});

test("a ticket PR that is ready or stacked needs a human; other states follow the shared table", () => {
  expect(statusForPRState("ready")).toBe("needsHuman");
  expect(statusForPRState("stacked")).toBe("needsHuman");
  expect(statusForPRState("merged")).toBe("done");
  expect(statusForPRState("zzz")).toBe("unknown");
});

test("a release decision maps allowed/denied to done/failed", () => {
  expect(statusForReleaseDecision(true)).toBe("done");
  expect(statusForReleaseDecision(false)).toBe("failed");
});

// Ports the PR #64 regression: a fetch-failed or not-yet-loaded kill switch
// (engaged === null) must never render as "clear".
describe("killSwitchDisplay", () => {
  test("never renders 'clear' from a null (unknown) input", () => {
    const shown = killSwitchDisplay(null);
    expect(shown.label).toBe("Kill switch unknown");
    expect(shown.outlined).toBe(true);
  });

  test("engaged is distinct from clear", () => {
    expect(killSwitchDisplay(true).label).toBe("Kill switch engaged");
    expect(killSwitchDisplay(false).label).toBe("Kill switch clear");
    expect(killSwitchDisplay(true).tone).not.toBe(killSwitchDisplay(false).tone);
  });

  test("unknown takes the lighter grey in dark mode", () => {
    expect(killSwitchDisplay(null, "light").tone).toBe("neutral");
    expect(killSwitchDisplay(null, "dark").tone).toBe("neutralOnDark");
  });
});

test("statusColor(Status.unknown) picks a distinct shade per brightness", () => {
  const light = statusTone("unknown", "light");
  const dark = statusTone("unknown", "dark");
  expect(light).not.toBe(dark);
  expect(light).toBe("neutral");
  expect(dark).toBe("neutralOnDark");
});

test(
  "the four filled statuses are unaffected by brightness -- only the " +
    'outlined "unknown" variant needs a dark-mode shade',
  () => {
    const filled: Status[] = ["needsHuman", "working", "done", "failed"];
    for (const status of filled) {
      expect(statusTone(status, "light")).toBe(statusTone(status, "dark"));
    }
    expect(filled.map((s) => statusTone(s))).toEqual(["warning", "info", "success", "danger"]);
    expect(filled.map(statusIcon)).toEqual(["priority_high", "autorenew", "check_circle", "error"]);
    expect(statusIcon("unknown")).toBe("help_outline");
  },
);

test("stateLabel passes composed or unknown labels through unchanged", () => {
  expect(stateLabel("slice_running")).toBe("Building");
  expect(stateLabel("Accepted · awaiting PR")).toBe("Accepted · awaiting PR");
  expect(stateLabel("never_seen")).toBe("never_seen");
  expect(stateLabel("spec_review")).toBe("Spec review");
});

test("each request state has one label and the verbs an operator has there", () => {
  expect(stateLabel("resume_review")).toBe("Needs resume");
  expect(stateLabel("slice_running")).toBe("Building");
  expect(requestVerbs("spec_review").map((v) => REQUEST_VERBS[v])).toEqual([
    "Approve",
    "Request changes",
  ]);
  expect(requestVerbs("plan_review")).toEqual(["approve", "requestChanges"]);
  expect(requestVerbs("oracle_review")).toEqual(["approve", "requestChanges"]);
  expect(requestVerbs("quarantined").map((v) => REQUEST_VERBS[v])).toEqual([
    "Retry request",
    "Send back",
    "Cancel request",
  ]);
  expect(requestVerbs("resume_review").map((v) => REQUEST_VERBS[v])).toEqual([
    "Resume",
    "Rebuild from scratch",
    "Rerun step",
    "Cancel request",
  ]);
  expect(requestVerbs("building")).toEqual([]);
  expect(requestVerbs("a_state_from_a_newer_server")).toEqual([]);
});
