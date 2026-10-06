import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { apiErrorResponse, json } from "@/test/render";

import { openRequest, seedOperator, stubRadix } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeAll(stubRadix);
beforeEach(seedOperator);

const heading = (name: string) => screen.findByRole("heading", { level: 1, name });
const callout = () => screen.getByTestId("recovery-callout");

describe("the halted/quarantined recovery callout", () => {
  test("accepted-awaiting-PR shows a neutral 'built and verified' callout whose retry says it rebuilds", async () => {
    openRequest(
      requestWire({
        state: "halted",
        halt_kind: "accepted_no_pr",
        title: "Accepted, no PR",
        next_action: "Merge branch factoryd/run-1 by hand.",
        tickets: [ticketWire({ index: 1, runId: "run-1" })],
      }),
    );
    await heading("Accepted, no PR");

    expect(screen.getByText("Built and verified; no pull request was opened.")).toBeInTheDocument();
    expect(screen.queryByText("This request is halted.")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry request (rebuilds)" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel request" })).toBeInTheDocument();
    expect(screen.getAllByText("Merge branch factoryd/run-1 by hand.")).toHaveLength(1);
  });

  test("a plain halt says so, with the generic explanation when the server gave no next_action", async () => {
    openRequest(requestWire({ state: "halted", title: "Halted" }));
    await heading("Halted");
    expect(screen.getByText("This request is halted.")).toBeInTheDocument();
    expect(
      screen.getByText("Retry the request to move it forward again, or cancel it to abandon it."),
    ).toBeInTheDocument();
  });

  test("the quarantined callout links to the run override with the reason pre-filled", async () => {
    openRequest(
      requestWire({
        state: "quarantined",
        title: "Quarantined",
        ticket_index: 1,
        tickets: [ticketWire({ index: 1, runId: "run-1" })],
      }),
    );
    await heading("Quarantined");

    expect(
      within(callout()).getByRole("link", { name: "Review the ticket's run override" }),
    ).toHaveAttribute("href", "/runs/run-1?reason=Request%20req-1%20ticket%201%20quarantined");
  });

  test("the quarantined callout links to the run override with a write path configured", async () => {
    openRequest(
      requestWire({
        state: "quarantined",
        title: "Quarantined",
        ticket_index: 1,
        tickets: [ticketWire({ index: 1, runId: "run-1" })],
      }),
    );
    await heading("Quarantined");

    expect(callout()).toBeInTheDocument();
    expect(
      within(callout()).getByRole("link", { name: "Review the ticket's run override" }),
    ).toHaveAttribute("href", expect.stringMatching(/^\/runs\/run-1/));
    // Retry and the run override are independent actions, not a sequence:
    // the callout names `factoryd retry` as the recovery and says the
    // override is separate and optional (found in review).
    expect(screen.getByText("factoryd retry req-1")).toBeInTheDocument();
    expect(screen.getByText(/separate, optional action/)).toBeInTheDocument();
  });

  test("a quarantine with no ticket run offers no override link", async () => {
    openRequest(requestWire({ state: "quarantined", title: "Q" }));
    await heading("Q");
    expect(
      screen.queryByRole("link", { name: "Review the ticket's run override" }),
    ).not.toBeInTheDocument();
  });

  test("Retry on a halted request calls POST /requests/{id}/retry with a reason", async () => {
    const { server } = openRequest(requestWire({ state: "halted", title: "T" }), {
      extra: [
        {
          on: "POST /requests/req-1/retry",
          reply: json(requestWire({ state: "building", title: "T" })),
        },
      ],
    });
    await heading("T");
    await userEvent.click(screen.getByRole("button", { name: "Retry request" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "fixed the setup error");
    await userEvent.click(within(dialog).getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/retry")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/retry")[0]?.body).toEqual({
      reason: "fixed the setup error",
      by: "operator",
    });
    // The server's new record replaces the callout.
    await waitFor(() => {
      expect(screen.queryByTestId("recovery-callout")).not.toBeInTheDocument();
    });
  });

  test("Cancel on a quarantined request calls POST /requests/{id}/cancel with a reason", async () => {
    const { server } = openRequest(requestWire({ state: "quarantined", title: "T" }), {
      extra: [
        {
          on: "POST /requests/req-1/cancel",
          reply: json(requestWire({ state: "cancelled", title: "T" })),
        },
      ],
    });
    await heading("T");
    await userEvent.click(screen.getByRole("button", { name: "Cancel request" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "no longer needed");
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel request" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/cancel")).toHaveLength(1);
    });
    expect((server.sent("POST /requests/req-1/cancel")[0]?.body as { reason: string }).reason).toBe(
      "no longer needed",
    );
  });

  test("a refused retry stays in the dialog with the server's message", async () => {
    openRequest(requestWire({ state: "halted", title: "T" }), {
      extra: [
        {
          on: "POST /requests/req-1/retry",
          reply: apiErrorResponse(409, "request is not halted any more"),
        },
      ],
    });
    await heading("T");
    await userEvent.click(screen.getByRole("button", { name: "Retry request" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "again");
    await userEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
    expect(await within(dialog).findByText(/request is not halted any more/)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  test("the send-back button is shown for a quarantined request that can send back", async () => {
    openRequest(requestWire({ state: "quarantined", title: "T", can_send_back: true }));
    await heading("T");
    expect(screen.getByRole("button", { name: /^Send back to / })).toBeInTheDocument();
  });

  test("the send-back button is absent when the server says a ticket has already been accepted", async () => {
    openRequest(
      requestWire({ id: "req-2", state: "quarantined", title: "T", can_send_back: false }),
      {
        id: "req-2",
      },
    );
    await heading("T");
    expect(screen.queryByRole("button", { name: /^Send back to / })).not.toBeInTheDocument();
  });

  test("Send back on a quarantined request calls POST /requests/{id}/reject with reason and to", async () => {
    const { server } = openRequest(
      requestWire({
        state: "quarantined",
        title: "T",
        can_send_back: true,
        can_send_back_to_plan: true,
      }),
      {
        extra: [
          {
            on: "POST /requests/req-1/reject",
            reply: json(requestWire({ state: "planning", title: "T" })),
          },
        ],
      },
    );
    await heading("T");
    await userEvent.click(screen.getByRole("button", { name: "Send back to planning" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(
      within(dialog).getByLabelText("Reason"),
      "diff_scope: allow the contract test",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Send back" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/reject")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/reject")[0]?.body).toEqual({
      reason: "diff_scope: allow the contract test",
      by: "operator",
      to: "plan",
    });
  });

  test("Send back defaults to spec when the server disallows the plan target (can_send_back_to_plan false)", async () => {
    const { server } = openRequest(
      requestWire({ state: "quarantined", title: "T", can_send_back: true }),
      {
        extra: [
          {
            on: "POST /requests/req-1/reject",
            reply: json(requestWire({ state: "planning", title: "T" })),
          },
        ],
      },
    );
    await heading("T");
    await userEvent.click(screen.getByRole("button", { name: "Send back to spec" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(
      within(dialog).getByLabelText("Reason"),
      "diff_scope: allow the contract test",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Send back" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/reject")).toHaveLength(1);
    });
    expect((server.sent("POST /requests/req-1/reject")[0]?.body as { to: string }).to).toBe("spec");
  });

  test("the send-back button reads 'Send back to spec' when only the spec target is allowed, and the CLI hint stays on the leading Retry", async () => {
    openRequest(requestWire({ state: "quarantined", title: "T", can_send_back: true }));
    await heading("T");
    expect(screen.getByRole("button", { name: "Send back to spec" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Send back to planning" })).not.toBeInTheDocument();
    // Retry leads for a quarantine that is not spec_conformity, so the
    // copyable command is the retry, not a send-back.
    expect(screen.getByText("factoryd retry req-1")).toBeInTheDocument();
    expect(screen.queryByText(/factoryd reject/)).not.toBeInTheDocument();
  });

  test("'Send back to spec' is the primary, first action for a spec_conformity quarantine", async () => {
    openRequest(
      requestWire({
        state: "quarantined",
        title: "T",
        can_send_back: true,
        can_send_back_to_plan: true,
        quarantine_check: "spec_conformity",
      }),
    );
    await heading("T");
    const buttons = within(callout()).getAllByRole("button");
    const names = buttons.map((b) => b.textContent);
    // First among the callout's own actions, ahead of Retry.
    expect(names.indexOf("Send back to spec")).toBeLessThan(names.indexOf("Retry request"));
    expect(names.indexOf("Send back to spec")).toBe(0);
    // Targets spec even though planning is allowed, and the CLI hint mirrors that action.
    expect(screen.queryByRole("button", { name: "Send back to planning" })).not.toBeInTheDocument();
    expect(
      screen.getByText('factoryd reject -to spec -reason "<what to change>" req-1'),
    ).toBeInTheDocument();
  });
});

describe("resume_review", () => {
  const lostBuild = (refused: string[] = []) =>
    requestWire({
      state: "resume_review",
      title: "T",
      error: "the factoryd worker stopped while the build ran",
      resume: {
        from_state: "building",
        lost_run_id: "run-1",
        generation: 1,
        at: "2026-09-10T09:05:00Z",
        ...(refused.length > 0 ? { refused } : {}),
      },
    });
  const lostStep = (step: string) =>
    requestWire({
      state: "resume_review",
      title: "T",
      resume: { from_state: step, generation: 1, at: "2026-09-10T09:05:00Z" },
    });
  const resumeCallout = () => screen.getByTestId("resume-callout");

  test("callout shows the lost step, the prompt and three actions", async () => {
    openRequest(lostBuild());
    await heading("T");
    expect(screen.getByText(/Building step was lost/)).toBeInTheDocument();
    expect(
      within(resumeCallout()).getByText("the factoryd worker stopped while the build ran"),
    ).toBeInTheDocument();
    for (const name of ["Resume", "Rebuild from scratch", "Cancel"]) {
      expect(within(resumeCallout()).getByRole("button", { name })).toBeInTheDocument();
    }
    expect(screen.queryByText(/^- /)).not.toBeInTheDocument();
  });

  test("refusal reasons are shown and Resume is withheld", async () => {
    openRequest(lostBuild(["a sandbox container is still running", "history moved"]));
    await heading("T");
    expect(screen.getByText("- a sandbox container is still running")).toBeInTheDocument();
    expect(screen.getByText("- history moved")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resume" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Rebuild from scratch" })).toBeInTheDocument();
  });

  test.each([
    ["Resume", "Resume", "round"],
    ["Rebuild from scratch", "Rebuild", "scratch"],
  ])("%s posts from=%s after confirmation", async (button, confirm, from) => {
    const { server } = openRequest(lostBuild(), {
      extra: [
        {
          on: "POST /requests/req-1/resume",
          reply: json(requestWire({ state: "building", title: "T" })),
        },
      ],
    });
    await heading("T");

    await userEvent.click(within(resumeCallout()).getByRole("button", { name: button }));
    const dialog = await screen.findByRole("dialog");
    // Nothing is sent before confirming.
    expect(server.sent("POST /requests/req-1/resume")).toHaveLength(0);
    await userEvent.click(within(dialog).getByRole("button", { name: confirm }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/resume")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/resume")[0]?.body).toEqual({ from, by: "operator" });
    await waitFor(() => {
      expect(screen.queryByTestId("resume-callout")).not.toBeInTheDocument();
    });
  });

  test("backing out of the confirmation sends nothing", async () => {
    const { server } = openRequest(lostBuild());
    await heading("T");
    await userEvent.click(within(resumeCallout()).getByRole("button", { name: "Resume" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Back" }));

    expect(server.sent("POST /requests/req-1/resume")).toHaveLength(0);
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(screen.getByTestId("resume-callout")).toBeInTheDocument();
  });

  test("a 409 refusal shows the server reasons in the dialog, which stays open", async () => {
    openRequest(lostBuild(), {
      extra: [
        {
          on: "POST /requests/req-1/resume",
          reply: apiErrorResponse(
            409,
            "request req-1: cannot resume the lost build: a sandbox container is still running",
          ),
        },
      ],
    });
    await heading("T");
    await userEvent.click(within(resumeCallout()).getByRole("button", { name: "Resume" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Resume" }));

    expect(
      await within(dialog).findByText(/a sandbox container is still running/),
    ).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  test("a 503 is shown as a failure the operator can retry in place", async () => {
    openRequest(lostBuild(), {
      extra: [
        {
          on: "POST /requests/req-1/resume",
          reply: apiErrorResponse(503, "could not check the lost build"),
        },
      ],
    });
    await heading("T");
    await userEvent.click(within(resumeCallout()).getByRole("button", { name: "Resume" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Resume" }));

    expect(await within(dialog).findByText(/could not check the lost build/)).toBeInTheDocument();
    expect(within(dialog).getByText("(temporary: try again)")).toBeInTheDocument();
    // The confirm button is live again: Resume can simply be pressed once more.
    expect(within(dialog).getByRole("button", { name: "Resume" })).toBeEnabled();
  });

  test("Cancel runs the existing cancel flow", async () => {
    const { server } = openRequest(lostBuild(), {
      extra: [
        {
          on: "POST /requests/req-1/cancel",
          reply: json(requestWire({ state: "cancelled", title: "T" })),
        },
      ],
    });
    await heading("T");
    await userEvent.click(within(resumeCallout()).getByRole("button", { name: "Cancel" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "not needed");
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel request" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/cancel")).toHaveLength(1);
    });
    expect((server.sent("POST /requests/req-1/cancel")[0]?.body as { reason: string }).reason).toBe(
      "not needed",
    );
  });

  test("a lost drafting step offers one Rerun step button", async () => {
    const { server } = openRequest(lostStep("spec_drafting"), {
      extra: [
        {
          on: "POST /requests/req-1/resume",
          reply: json(requestWire({ state: "spec_drafting", title: "T" })),
        },
      ],
    });
    await heading("T");
    expect(screen.queryByRole("button", { name: "Rebuild from scratch" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resume" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Rerun step" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Run the lost Drafting spec step again/)).toBeInTheDocument();
    expect(within(dialog).queryByText(/paid run/)).not.toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Rerun step" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/resume")).toHaveLength(1);
    });
    expect((server.sent("POST /requests/req-1/resume")[0]?.body as { from: string }).from).toBe(
      "round",
    );
  });

  test("a lost build still offers Resume and Rebuild", async () => {
    openRequest(lostStep("building"));
    await heading("T");
    expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Rebuild from scratch" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rerun step" })).not.toBeInTheDocument();
  });

  test("a failed cancel is labelled by the cancel dialog and leaves the callout, not a resume failure", async () => {
    openRequest(lostBuild(), {
      extra: [
        { on: "POST /requests/req-1/cancel", reply: apiErrorResponse(409, "cannot cancel now") },
      ],
    });
    await heading("T");
    await userEvent.click(within(resumeCallout()).getByRole("button", { name: "Cancel" }));
    const dialog = await screen.findByRole("dialog", { name: "Cancel this request" });
    await userEvent.type(within(dialog).getByLabelText("Reason"), "no longer needed");
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel request" }));

    expect(await within(dialog).findByText(/cannot cancel now/)).toBeInTheDocument();
    expect(screen.queryByText(/Could not resume/)).not.toBeInTheDocument();
  });

  test("closing a failed resume dialog leaves no stale error behind", async () => {
    openRequest(lostBuild(), {
      extra: [
        { on: "POST /requests/req-1/resume", reply: apiErrorResponse(409, "container alive") },
      ],
    });
    await heading("T");
    await userEvent.click(within(resumeCallout()).getByRole("button", { name: "Resume" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Resume" }));
    await within(dialog).findByText(/container alive/);

    await userEvent.click(within(dialog).getByRole("button", { name: "Back" }));
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(screen.queryByText(/container alive/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume" })).toBeEnabled();
  });
});

describe("reading order when a request stops", () => {
  test("cause, then the run's evidence link, then the primary action", async () => {
    openRequest(
      requestWire({
        state: "halted",
        title: "Stopped",
        error: "gh: not logged in",
        ticket_index: 1,
        tickets: [ticketWire({ index: 1, runId: "run-1" })],
      }),
    );
    await heading("Stopped");
    const box = callout();
    const cause = within(box).getByTestId("recovery-cause");
    const evidence = within(box).getByRole("link", { name: /Evidence: run log, diff and gates/ });
    const retry = within(box).getByRole("button", { name: "Retry request" });
    expect(evidence).toHaveAttribute("href", "/runs/run-1");
    const before = Node.DOCUMENT_POSITION_FOLLOWING;
    expect(cause.compareDocumentPosition(evidence) & before).toBeTruthy();
    expect(evidence.compareDocumentPosition(retry) & before).toBeTruthy();
  });

  test("the terminal command and the run-record correction sit in one closed disclosure after the actions", async () => {
    openRequest(
      requestWire({
        state: "quarantined",
        title: "Stopped",
        error: "verify failed after 3 rounds",
        ticket_index: 1,
        tickets: [ticketWire({ index: 1, runId: "run-1" })],
      }),
    );
    await heading("Stopped");
    const box = callout();
    const more = within(box).getByTestId<HTMLDetailsElement>("recovery-more");

    expect(more.open).toBe(false);
    expect(
      within(more).getByRole("heading", { name: "Other ways to resolve this" }),
    ).toBeInTheDocument();
    expect(within(more).getByText("factoryd retry req-1")).toBeInTheDocument();
    expect(within(more).getByText(/Correcting the ticket's own run record/)).toBeInTheDocument();
    expect(
      within(more).getByRole("link", { name: "Review the ticket's run override" }),
    ).toBeInTheDocument();
    // The actions are outside it and first.
    for (const name of ["Retry request", "Cancel request", "Send back to planning"]) {
      const button = within(box).queryByRole("button", { name });
      if (button !== null) {
        expect(more.contains(button)).toBe(false);
        expect(
          button.compareDocumentPosition(more) & Node.DOCUMENT_POSITION_FOLLOWING,
        ).toBeTruthy();
      }
    }
    await userEvent.click(
      within(more).getByRole("heading", { name: "Other ways to resolve this" }),
    );
    expect(more.open).toBe(true);
  });

  test("a long explanation shows its first sentence, with the whole one inside the disclosure", async () => {
    const rest =
      "Then check the branch and merge it yourself if the pull request cannot be opened.";
    openRequest(
      requestWire({
        state: "halted",
        title: "Stopped",
        next_action: `Open the pull request by hand. ${rest}`,
      }),
    );
    await heading("Stopped");

    expect(screen.getByTestId("recovery-explanation")).toHaveTextContent(
      /^Open the pull request by hand\.$/,
    );
    const more = screen.getByTestId("recovery-more");
    expect(within(more).getByText(`Open the pull request by hand. ${rest}`)).toBeInTheDocument();
  });

  test("the cause stays whole, however long", async () => {
    const cause = `ticket 1 was accepted but its pull request could not be opened: ${"gh said no. ".repeat(40)}END`;
    openRequest(requestWire({ state: "halted", title: "Stopped", error: cause }));
    await heading("Stopped");

    expect(screen.getByTestId("recovery-cause")).toHaveTextContent(cause);
  });

  test("no run, no evidence link", async () => {
    openRequest(requestWire({ state: "halted", title: "No run yet" }));
    await heading("No run yet");
    expect(screen.queryByTestId("recovery-evidence")).not.toBeInTheDocument();
  });
});

describe("the request header", () => {
  const longId = "add-subtract-numbers-to-add-py-add-a-sub-20261005-225506";

  test("shows a long id compactly, with the whole id copyable and in the tooltip", async () => {
    openRequest(requestWire({ id: longId, state: "building", title: "Long id" }), { id: longId });
    await heading("Long id");
    const copy = await screen.findByRole("button", { name: "Copy request id" });
    expect(copy).toBeInTheDocument();
    expect(screen.getAllByTitle(longId).length).toBeGreaterThan(0);
    expect(screen.getByText(/^add-subtract.*225506$/, { selector: "[aria-hidden]" })).toBeVisible();
  });

  test("is sticky, so the decision buttons stay reachable while a long plan scrolls", async () => {
    openRequest(requestWire({ state: "spec_review", title: "Reviewing" }));
    const h1 = await heading("Reviewing");
    expect(h1.closest("header")).toHaveAttribute("data-sticky", "true");
  });
});
