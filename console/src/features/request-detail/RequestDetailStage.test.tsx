import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { setOperatorName } from "@/platform/operatorIdentity";
import { json } from "@/test/render";

import { openRequest } from "./testHarness";
import { type Wire, requestWire } from "./testRequests";

// No stored name: the "Your name" prompt comes up between pressing the button
// and the dialog itself, and the request can change while it is up.
beforeEach(() => {
  setOperatorName("");
});

const pressedAt = "2026-09-10T09:05:00Z";

/** The page on a request whose record the test can change, and the reject route. */
function open(first: Wire) {
  let body = first;
  const view = openRequest(() => body, {
    extra: [
      {
        on: "POST /requests/req-1/reject",
        reply: json(requestWire({ state: "spec_drafting", title: "T" })),
      },
    ],
  });
  return {
    ...view,
    /** The request changes on the server, and the page reads it again (as its event stream makes it). */
    becomes: async (next: Wire) => {
      body = next;
      await act(async () => {
        await view.queryClient.invalidateQueries();
      });
    },
  };
}

async function enterName() {
  const prompt = await screen.findByRole("dialog", { name: "Your name" });
  await userEvent.type(within(prompt).getByLabelText("Operator name"), "jane");
  await userEvent.click(within(prompt).getByRole("button", { name: "Continue" }));
}

describe("Request changes pressed, then the request changes while the name prompt is up", () => {
  const spec = "# Spec\n\n## Acceptance criteria\n\n1. One charge.\n";
  const shown = requestWire({ state: "spec_review", title: "T", spec });
  const press = async () => {
    const view = open(shown);
    await screen.findByRole("heading", { level: 1, name: "T" });
    await userEvent.click(await screen.findByRole("button", { name: "Request changes" }));
    await screen.findByRole("dialog", { name: "Your name" });
    return view;
  };
  const dialog = () => screen.findByRole("dialog", { name: "Request changes" });

  test("nothing changes: the rejection names the stage that was on the page", async () => {
    const { server } = await press();
    await enterName();
    const flow = await dialog();
    expect(within(flow).queryByTestId("flow-blocked")).not.toBeInTheDocument();
    await userEvent.type(within(flow).getByLabelText("Reason"), "too broad");
    await userEvent.click(within(flow).getByRole("button", { name: "Request changes" }));
    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/reject")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/reject")[0]?.body).toEqual({
      reason: "too broad",
      by: "jane",
      expected_state: "spec_review",
      expected_entered_at: pressedAt,
    });
  });

  test("it moves to another state: the dialog says so, keeps the text, and cannot send", async () => {
    const { server, becomes } = await press();
    await becomes(
      requestWire({ state: "plan_review", title: "T", entered_at: "2026-09-10T09:30:00Z" }),
    );
    await enterName();
    const flow = await dialog();
    expect(within(flow).getByTestId("flow-blocked")).toHaveTextContent(
      "This request has moved on to Plan review. Nothing can be sent from this dialog.",
    );
    // It still works from the record that was on the page: the spec's places, not the plan's.
    expect(within(flow).getByLabelText("Place")).toHaveTextContent("spec.md");
    await userEvent.type(within(flow).getByLabelText("Reason"), "too broad");
    const send = within(flow).getByRole("button", { name: "Request changes" });
    expect(send).toBeDisabled();
    await userEvent.click(send);
    expect(within(flow).getByLabelText("Reason")).toHaveValue("too broad");
    expect(server.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });

  test("it is redrafted into the same state: the dialog says so and cannot send", async () => {
    const { server, becomes } = await press();
    await becomes({ ...shown, entered_at: "2026-09-10T11:00:00Z", spec: "# Spec\n\nRedrafted.\n" });
    await enterName();
    const flow = await dialog();
    expect(within(flow).getByTestId("flow-blocked")).toHaveTextContent(
      "it was redrafted and is in Spec review again",
    );
    await userEvent.type(within(flow).getByLabelText("Reason"), "too broad");
    expect(within(flow).getByRole("button", { name: "Request changes" })).toBeDisabled();
    expect(server.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });

  test("it changes after the dialog is up: the dialog stops sending then, with the text kept", async () => {
    const { server, becomes } = await press();
    await enterName();
    const flow = await dialog();
    await userEvent.type(within(flow).getByLabelText("Reason"), "too broad");
    expect(within(flow).getByRole("button", { name: "Request changes" })).toBeEnabled();
    await becomes({ ...shown, entered_at: "2026-09-10T11:00:00Z" });
    expect(await within(flow).findByTestId("flow-blocked")).toBeInTheDocument();
    expect(within(flow).getByRole("button", { name: "Request changes" })).toBeDisabled();
    expect(within(flow).getByLabelText("Reason")).toHaveValue("too broad");
    expect(server.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });
});

describe("Send back pressed, then the request changes while the name prompt is up", () => {
  const shown = requestWire({
    state: "quarantined",
    title: "T",
    can_send_back: true,
    can_send_back_to_plan: true,
  });
  const press = async () => {
    const view = open(shown);
    await screen.findByRole("heading", { level: 1, name: "T" });
    await userEvent.click(await screen.findByRole("button", { name: "Send back to planning" }));
    await screen.findByRole("dialog", { name: "Your name" });
    return view;
  };
  const dialog = () => screen.findByRole("dialog", { name: "Send back" });

  test("nothing changes: the send-back names the stage that was on the page", async () => {
    const { server } = await press();
    await enterName();
    const flow = await dialog();
    await userEvent.type(within(flow).getByLabelText("Reason"), "replan");
    await userEvent.click(within(flow).getByRole("button", { name: "Send back" }));
    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/reject")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/reject")[0]?.body).toEqual({
      reason: "replan",
      by: "jane",
      to: "plan",
      expected_state: "quarantined",
      expected_entered_at: pressedAt,
    });
  });

  test.each([
    [
      "moves to another state",
      requestWire({ state: "building", title: "T", entered_at: "2026-09-10T09:30:00Z" }),
      "This request has moved on to Building.",
    ],
    [
      "is quarantined again",
      { ...shown, entered_at: "2026-09-10T11:00:00Z" },
      "is in Quarantined again",
    ],
  ])("it %s: the dialog says so and cannot send", async (_, next, notice) => {
    const { server, becomes } = await press();
    await becomes(next);
    await enterName();
    const flow = await dialog();
    expect(within(flow).getByTestId("flow-blocked")).toHaveTextContent(notice);
    await userEvent.type(within(flow).getByLabelText("Reason"), "replan");
    expect(within(flow).getByRole("button", { name: "Send back" })).toBeDisabled();
    expect(within(flow).getByLabelText("Reason")).toHaveValue("replan");
    expect(server.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });
});
