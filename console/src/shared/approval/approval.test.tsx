import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { type RequestSummary, decodeRequestSummary } from "@/domain/request";
import { sha256Hex } from "@/domain/contentHash";
import { getOperatorName, setOperatorName } from "@/platform/operatorIdentity";
import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { nextStateAfterApprove, oracleSkipWarningFor } from "./approvalText";
import { ApproveDialog } from "./ApproveDialog";
import { CancelDialog } from "./CancelDialog";
import type { FlowProps } from "./flowTypes";
import { RejectDialog } from "./RejectDialog";
import { ResumeDialog } from "./ResumeDialog";
import { RetryDialog } from "./RetryDialog";
import { SendBackDialog } from "./SendBackDialog";

beforeEach(() => {
  setOperatorName("operator");
});

function fixture(name: string, patch: Record<string, unknown> = {}): RequestSummary {
  const raw = readFixtureJson(`api/${name}`) as Record<string, unknown>;
  const body = { ...raw, ...patch };
  return decodeRequestSummary(body, `fixture ${name}`);
}

function rawFixture(name: string, patch: Record<string, unknown> = {}): Record<string, unknown> {
  return { ...(readFixtureJson(`api/${name}`) as Record<string, unknown>), ...patch };
}

interface HostProps {
  readonly request: RequestSummary;
  readonly render: (props: FlowProps) => React.ReactNode;
  readonly onDone?: (request: RequestSummary) => void;
}

function Host({ request, render, onDone }: HostProps) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <p>{open ? "dialog open" : "dialog closed"}</p>
      {render({ request, open, onOpenChange: setOpen, ...(onDone ? { onDone } : {}) })}
    </>
  );
}

const specReview = () => fixture("request-spec-review.json");

test("the operator-name dialog helper text allows multiple lines", async () => {
  setOperatorName("");
  renderApp(<Host request={specReview()} render={(props) => <RejectDialog {...props} />} />);
  expect(await screen.findByText("Your name")).toBeInTheDocument();
  expect(screen.getByLabelText("Operator name")).toBeInTheDocument();
  expect(
    screen.getByText("Recorded on every approve/reject you make from here."),
  ).toBeInTheDocument();
});

test("the name prompt remembers the name, then continues to the flow", async () => {
  setOperatorName("");
  const { server } = renderApp(
    <Host request={specReview()} render={(props) => <RejectDialog {...props} />} />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/reject",
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  const user = userEvent.setup();
  expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
  await user.type(screen.getByLabelText("Operator name"), "  jane ");
  await user.click(screen.getByRole("button", { name: "Continue" }));
  await user.type(await screen.findByLabelText("Reason"), "too broad");
  await user.click(screen.getByRole("button", { name: "Request changes" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/reject")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/reject")[0]?.body).toEqual({
    expected_state: "spec_review",
    expected_entered_at: "2026-09-10T09:05:00Z",
    reason: "too broad",
    by: "jane",
  });
  expect(getOperatorName()).toBe("jane");
});

test("cancelling the name prompt closes the flow and sends nothing", async () => {
  setOperatorName("");
  const { server } = renderApp(
    <Host request={specReview()} render={(props) => <RejectDialog {...props} />} />,
  );
  await userEvent.click(await screen.findByRole("button", { name: "Cancel" }));
  expect(screen.getByText("dialog closed")).toBeInTheDocument();
  expect(server.requests).toHaveLength(0);
});

test("a console that cannot write offers no dialog at all", () => {
  renderApp(<Host request={specReview()} render={(props) => <ApproveDialog {...props} />} />, {
    config: { writesEnabled: false },
  });
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

test("approve sends the hashes of the displayed spec, by, and closes", async () => {
  const shown = fixture("request-spec-review.json", { spec: "# the displayed spec\n" });
  const done = vi.fn();
  const { server } = renderApp(
    <Host request={shown} onDone={done} render={(props) => <ApproveDialog {...props} />} />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/approve",
          reply: json(rawFixture("request-spec-review.json", { state: "planning" })),
        },
      ],
    },
  );
  expect(await screen.findByText("Approve this request?")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  await waitFor(() => {
    expect(screen.getByText("dialog closed")).toBeInTheDocument();
  });
  expect(server.sent("POST /requests/req-spec-review/approve")[0]?.body).toEqual({
    by: "operator",
    expected_sha256: { "spec.md": sha256Hex("# the displayed spec\n") },
  });
  expect(server.requests.filter((r) => r.method === "GET")).toHaveLength(0);
  expect(done).toHaveBeenCalledTimes(1);
});

test("a record that changes while the dialog is open is not what gets approved", async () => {
  // The event stream can replace the caller's record (a redraft) after the
  // operator pressed Approve on the version they read.
  function Changing() {
    const [request, setRequest] = useState(() =>
      fixture("request-spec-review.json", { spec: "# the spec the operator read\n" }),
    );
    return (
      <>
        <button
          type="button"
          onClick={() => {
            setRequest(fixture("request-spec-review.json", { spec: "# a redraft nobody read\n" }));
          }}
        >
          redraft arrives
        </button>
        <ApproveDialog request={request} open onOpenChange={() => undefined} />
      </>
    );
  }
  const { server } = renderApp(<Changing />, {
    server: [
      {
        on: "POST /requests/req-spec-review/approve",
        reply: apiErrorResponse(409, "spec.md changed since it was shown"),
      },
    ],
  });
  expect(await screen.findByText("Approve this request?")).toBeInTheDocument();
  // The dialog is modal, so the redraft is delivered programmatically.
  screen.getByText("redraft arrives").click();
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/approve")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/approve")[0]?.body).toEqual({
    by: "operator",
    expected_sha256: { "spec.md": sha256Hex("# the spec the operator read\n") },
  });
  expect(await screen.findByText(/spec.md changed since it was shown/)).toBeInTheDocument();
});

test("approve restates the transition, tickets, usage and prior rejections", async () => {
  renderApp(<Host request={specReview()} render={(props) => <ApproveDialog {...props} />} />);
  expect(await screen.findByText(/Spec review → /)).toBeInTheDocument();
  expect(screen.getByText(/^Tickets: /)).toBeInTheDocument();
  expect(screen.getByText(/^Usage so far: planning · /)).toBeInTheDocument();
  expect(screen.getByText(/^Prior rejections: \d+$/)).toBeInTheDocument();
});

test("approve shows an unknown cost as a dash, or the caller's list-route summary", async () => {
  const bare = fixture("request-spec-review.json", { cost_summary: null });
  renderApp(<Host request={bare} render={(props) => <ApproveDialog {...props} />} />);
  expect(await screen.findByText("Usage so far: —")).toBeInTheDocument();
});

test("a refused approval keeps the dialog open with the server's error", async () => {
  renderApp(<Host request={specReview()} render={(props) => <ApproveDialog {...props} />} />, {
    server: [
      {
        on: "POST /requests/req-spec-review/approve",
        reply: apiErrorResponse(409, "spec.md changed since it was shown"),
      },
    ],
  });
  await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("spec.md changed since it was shown");
  expect(screen.getByText("dialog open")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
});

test("the confirm button is disabled while the write is in flight", async () => {
  let release: () => void = () => undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  renderApp(<Host request={specReview()} render={(props) => <ApproveDialog {...props} />} />, {
    server: [
      {
        on: "POST /requests/req-spec-review/approve",
        reply: async () => {
          await gate;
          return json(rawFixture("request-spec-review.json"));
        },
      },
    ],
  });
  await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Approve" })).toBeDisabled();
  });
  release();
  await waitFor(() => {
    expect(screen.getByText("dialog closed")).toBeInTheDocument();
  });
});

test("an oracle_review approval sends the shown oracle hashes; empty states the consequence", async () => {
  const request = fixture("request-oracle-review.json", {
    oracle_draft: { status: "none_eligible" },
  });
  const { server } = renderApp(
    <Host request={request} render={(props) => <ApproveDialog {...props} expectedSha256={{}} />} />,
    {
      server: [
        {
          on: "POST /requests/req-oracle-review/approve",
          reply: json(rawFixture("request-oracle-review.json")),
        },
      ],
    },
  );
  expect(
    await screen.findByText(
      "No oracle files: the build will have no request-level acceptance test.",
    ),
  ).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-oracle-review/approve")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-oracle-review/approve")[0]?.body).toEqual({
    by: "operator",
  });
});

test("an oracle approval that skips a failed draft shows the warning instead", async () => {
  const request = fixture("request-oracle-review.json", {
    oracle_draft: { status: "failed", detail: "model timed out" },
  });
  renderApp(
    <Host request={request} render={(props) => <ApproveDialog {...props} expectedSha256={{}} />} />,
  );
  expect(
    await screen.findByText(/Approving skips the oracle stage: the draft ended "failed"/),
  ).toHaveTextContent("(model timed out)");
  expect(screen.queryByText(/^No oracle files:/)).not.toBeInTheDocument();
});

test("oracleSkipWarningFor and nextStateAfterApprove", () => {
  const request = fixture("request-oracle-review.json", { oracle_draft: { status: "failed" } });
  expect(oracleSkipWarningFor(request, { "oracle/a_test.go": "x" })).toBeNull();
  expect(oracleSkipWarningFor(request, null)).toBeNull();
  expect(oracleSkipWarningFor(specReview(), {})).toBeNull();
  expect(oracleSkipWarningFor(request, {})).toContain('ended "failed"');
  expect(nextStateAfterApprove("plan_review")).toBe("building");
  expect(nextStateAfterApprove("oracle_review")).toBe("planning");
  expect(nextStateAfterApprove("building")).toBeNull();
});

test("reject needs a reason, then sends reason and by", async () => {
  const { server } = renderApp(
    <Host request={specReview()} render={(props) => <RejectDialog {...props} />} />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/reject",
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  const confirm = await screen.findByRole("button", { name: "Request changes" });
  expect(confirm).toBeDisabled();
  expect(server.requests).toHaveLength(0);
  await userEvent.type(screen.getByLabelText("Reason"), "scope is too broad");
  await userEvent.click(confirm);
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/reject")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/reject")[0]?.body).toEqual({
    expected_state: "spec_review",
    expected_entered_at: "2026-09-10T09:05:00Z",
    reason: "scope is too broad",
    by: "operator",
  });
});

test.each([
  ["retry", "Retry this request", "Retry", RetryDialog],
  ["cancel", "Cancel this request", "Cancel request", CancelDialog],
])("%s posts its reason and by", async (verb, title, confirm, Dialog) => {
  const { server } = renderApp(
    <Host request={specReview()} render={(props) => <Dialog {...props} />} />,
    {
      server: [
        {
          on: `POST /requests/req-spec-review/${verb}`,
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  expect(await screen.findByText(title)).toBeInTheDocument();
  await userEvent.type(screen.getByLabelText("Reason"), "because");
  await userEvent.click(screen.getByRole("button", { name: confirm }));
  await waitFor(() => {
    expect(server.sent(`POST /requests/req-spec-review/${verb}`)).toHaveLength(1);
  });
  expect(server.sent(`POST /requests/req-spec-review/${verb}`)[0]?.body).toEqual({
    reason: "because",
    by: "operator",
  });
});

test("retry offers a rebuild from scratch, off by default, and sends it when ticked", async () => {
  const { server } = renderApp(
    <Host request={specReview()} render={(props) => <RetryDialog {...props} />} />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/retry",
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  const box = await screen.findByRole("checkbox", { name: "Retry from scratch" });
  expect(box).not.toBeChecked();
  expect(box).toHaveAccessibleDescription(
    "Rebuild the ticket from the base commit instead of continuing from the failed attempt's commit.",
  );
  await userEvent.type(screen.getByLabelText("Reason"), "the attempt went the wrong way");
  await userEvent.click(box);
  await userEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/retry")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/retry")[0]?.body).toEqual({
    reason: "the attempt went the wrong way",
    by: "operator",
    from: "scratch",
  });
});

test("a failed cancel shows the server's message and keeps the reason", async () => {
  renderApp(<Host request={specReview()} render={(props) => <CancelDialog {...props} />} />, {
    server: [
      {
        on: "POST /requests/req-spec-review/cancel",
        reply: apiErrorResponse(409, "cannot cancel now"),
      },
    ],
  });
  await userEvent.type(await screen.findByLabelText("Reason"), "nope");
  await userEvent.click(screen.getByRole("button", { name: "Cancel request" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("cannot cancel now");
  expect(screen.queryByText("(temporary: try again)")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Reason")).toHaveValue("nope");
});

test("the cancel dialog states its consequence", async () => {
  renderApp(<Host request={specReview()} render={(props) => <CancelDialog {...props} />} />);
  expect(
    await screen.findByText(
      "The request is marked cancelled for good. A build already running is not stopped; its result is discarded.",
    ),
  ).toBeInTheDocument();
});

test("a 503 on a write is marked temporary, a 409 is not", async () => {
  renderApp(<Host request={specReview()} render={(props) => <CancelDialog {...props} />} />, {
    server: [
      {
        on: "POST /requests/req-spec-review/cancel",
        reply: apiErrorResponse(503, "could not check the build"),
      },
    ],
  });
  await userEvent.type(await screen.findByLabelText("Reason"), "nope");
  await userEvent.click(screen.getByRole("button", { name: "Cancel request" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("could not check the build");
  expect(screen.getByText("(temporary: try again)")).toBeInTheDocument();
});

test("send back defaults to plan and sends the target", async () => {
  const request = fixture("request-spec-review.json", { can_send_back_to_plan: true });
  const { server } = renderApp(
    <Host request={request} render={(props) => <SendBackDialog {...props} />} />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/reject",
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  expect(await screen.findByRole("radio", { name: "To planning" })).toBeChecked();
  await userEvent.type(screen.getByLabelText("Reason"), "diff_scope: allow the contract test");
  await userEvent.click(screen.getByRole("button", { name: "Send back" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/reject")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/reject")[0]?.body).toEqual({
    expected_state: "spec_review",
    expected_entered_at: "2026-09-10T09:05:00Z",
    reason: "diff_scope: allow the contract test",
    by: "operator",
    to: "plan",
  });
});

test("Send back defaults to spec when the server disallows the plan target (can_send_back_to_plan false)", async () => {
  const request = fixture("request-spec-review.json", { can_send_back_to_plan: false });
  const { server } = renderApp(
    <Host request={request} render={(props) => <SendBackDialog {...props} />} />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/reject",
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  expect(await screen.findByRole("radio", { name: "To planning" })).toBeDisabled();
  expect(screen.getByRole("radio", { name: "To spec" })).toBeChecked();
  await userEvent.type(screen.getByLabelText("Reason"), "reword");
  await userEvent.click(screen.getByRole("button", { name: "Send back" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/reject")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/reject")[0]?.body).toMatchObject({
    to: "spec",
  });
});

test("a spec_conformity quarantine starts on spec even when plan is allowed", async () => {
  const request = fixture("request-spec-review.json", {
    can_send_back_to_plan: true,
    quarantine_check: "spec_conformity",
  });
  renderApp(<Host request={request} render={(props) => <SendBackDialog {...props} />} />);
  expect(await screen.findByRole("radio", { name: "To spec" })).toBeChecked();
  expect(screen.getByRole("radio", { name: "To planning" })).toBeEnabled();
});

function lost(step: string): RequestSummary {
  return fixture("request-spec-review.json", {
    resume: { from_state: step, ticket: 1, refusals: [] },
  });
}

test.each([
  [
    "round",
    "building",
    "Resume this request",
    "Continue the lost step where the worker stopped.",
    "Resume",
  ],
  ["scratch", "building", "Rebuild from scratch", "paid run", "Rebuild"],
  [
    "round",
    "spec_drafting",
    "Rerun this step",
    "Run the lost Spec drafting step again.",
    "Rerun step",
  ],
])("resume from=%s on a lost %s step", async (from, step, title, body, confirm) => {
  const { server } = renderApp(
    <Host
      request={lost(step)}
      render={(props) => <ResumeDialog {...props} from={from as "round" | "scratch"} />}
    />,
    {
      server: [
        {
          on: "POST /requests/req-spec-review/resume",
          reply: json(rawFixture("request-spec-review.json")),
        },
      ],
    },
  );
  expect(await screen.findByText(title)).toBeInTheDocument();
  expect(screen.getByText(new RegExp(body))).toBeInTheDocument();
  expect(server.requests).toHaveLength(0);
  await userEvent.click(screen.getByRole("button", { name: confirm }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-spec-review/resume")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-spec-review/resume")[0]?.body).toEqual({
    from,
    by: "operator",
  });
});

test("backing out of the resume confirmation sends nothing", async () => {
  const { server } = renderApp(
    <Host
      request={lost("building")}
      render={(props) => <ResumeDialog {...props} from="round" />}
    />,
  );
  await userEvent.click(await screen.findByRole("button", { name: "Back" }));
  expect(screen.getByText("dialog closed")).toBeInTheDocument();
  expect(server.requests).toHaveLength(0);
});

describe("Request changes with notes on specific places", () => {
  const rejectRoute = {
    on: "POST /requests/req-spec-review/reject",
    reply: json(rawFixture("request-spec-review.json")),
  };

  async function addNote(user: ReturnType<typeof userEvent.setup>, place: string, note: string) {
    await user.selectOptions(await screen.findByLabelText("Place"), place);
    await user.type(screen.getByLabelText("Note on this place"), note);
    await user.click(screen.getByRole("button", { name: "Add note" }));
  }

  test("the places offered are the shown spec's sections and criteria", async () => {
    setOperatorName("jane");
    renderApp(<Host request={specReview()} render={(props) => <RejectDialog {...props} />} />);

    const options = within(await screen.findByLabelText("Place")).getAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([
      "spec.md · the whole file",
      "spec.md · Problem",
      "spec.md · Acceptance criteria",
      "spec.md · Acceptance criteria · 1. A repeated request with the same key returns the first response.",
      "spec.md · Acceptance criteria · 2. A key is scoped to one account.",
    ]);
  });

  test("notes alone are enough: they are sent as anchors, with an empty reason", async () => {
    setOperatorName("jane");
    const { server } = renderApp(
      <Host request={specReview()} render={(props) => <RejectDialog {...props} />} />,
      { server: [rejectRoute] },
    );
    const user = userEvent.setup();
    const confirm = await screen.findByRole("button", { name: "Request changes" });
    expect(confirm).toBeDisabled();

    await addNote(
      user,
      "spec.md · Acceptance criteria · 2. A key is scoped to one account.",
      "which account?",
    );
    await addNote(user, "spec.md · Problem", "say who is charged{Enter}");

    expect(
      within(screen.getByRole("list", { name: "Anchored notes" }))
        .getAllByRole("listitem")
        .map((item) => item.textContent),
    ).toEqual([
      "spec.md · Acceptance criteria · number 2: which account?",
      "spec.md · Problem: say who is charged",
    ]);
    expect(confirm).toBeEnabled();
    await user.click(confirm);

    await waitFor(() => {
      expect(server.sent("POST /requests/req-spec-review/reject")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-spec-review/reject")[0]?.body).toEqual({
      expected_state: "spec_review",
      expected_entered_at: "2026-09-10T09:05:00Z",
      reason: "",
      by: "jane",
      anchors: [
        { path: "spec.md", section: "## Acceptance criteria", item: 2, note: "which account?" },
        { path: "spec.md", section: "## Problem", note: "say who is charged" },
      ],
    });
  });

  test("a removed note is not sent, and the reason is required again", async () => {
    setOperatorName("jane");
    const { server } = renderApp(
      <Host request={specReview()} render={(props) => <RejectDialog {...props} />} />,
      { server: [rejectRoute] },
    );
    const user = userEvent.setup();
    await addNote(user, "spec.md · the whole file", "start over");
    await user.click(screen.getByRole("button", { name: "Remove note 1" }));

    const confirm = screen.getByRole("button", { name: "Request changes" });
    expect(confirm).toBeDisabled();
    await user.type(screen.getByLabelText("Reason"), "too broad");
    await user.click(confirm);

    await waitFor(() => {
      expect(server.sent("POST /requests/req-spec-review/reject")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-spec-review/reject")[0]?.body).toEqual({
      expected_state: "spec_review",
      expected_entered_at: "2026-09-10T09:05:00Z",
      reason: "too broad",
      by: "jane",
    });
  });

  test("a state with no document sections offers no places, only the reason", async () => {
    setOperatorName("jane");
    renderApp(
      <Host
        request={{ ...specReview(), state: "oracle_review" }}
        render={(props) => <RejectDialog {...props} />}
      />,
    );
    await screen.findByLabelText("Reason");
    expect(screen.queryByLabelText("Place")).not.toBeInTheDocument();
  });
});

describe("the stage a rejection is of", () => {
  const stage = { expected_state: "spec_review", expected_entered_at: "2026-09-10T09:05:00Z" };
  const reply = {
    on: "POST /requests/req-spec-review/reject",
    reply: json(rawFixture("request-spec-review.json", { state: "spec_drafting" })),
  };

  // The caller's record changes while the dialog is open, as a page kept
  // live by the event stream does.
  function Changing({
    render,
    next,
  }: {
    readonly render: (props: FlowProps) => React.ReactNode;
    readonly next: Record<string, unknown>;
  }) {
    const [request, setRequest] = useState(() => fixture("request-spec-review.json"));
    return (
      <>
        <button
          type="button"
          onClick={() => {
            setRequest(fixture("request-spec-review.json", next));
          }}
        >
          record changes
        </button>
        <Host request={request} render={render} />
      </>
    );
  }
  const change = () => {
    // Behind a modal dialog the page takes no pointer: the change comes from outside it.
    screen.getByRole("button", { name: "record changes", hidden: true }).click();
  };

  test.each([
    ["the request moved to another state", { state: "plan_review" }],
    ["the request was redrafted into the same state", { entered_at: "2026-09-10T11:00:00Z" }],
  ])(
    "Request changes sends the stage shown when it opened, though %s underneath",
    async (_, next) => {
      const { server } = renderApp(
        <Changing render={(props) => <RejectDialog {...props} />} next={next} />,
        { server: [reply] },
      );
      const dialog = await screen.findByRole("dialog", { name: "Request changes" });
      await userEvent.type(within(dialog).getByLabelText("Reason"), "too broad");
      await waitFor(change);
      await userEvent.click(within(dialog).getByRole("button", { name: "Request changes" }));
      await waitFor(() => {
        expect(server.sent(reply.on)).toHaveLength(1);
      });
      expect(server.sent(reply.on)[0]?.body).toEqual({
        reason: "too broad",
        by: "operator",
        ...stage,
      });
    },
  );

  test.each([
    ["the request moved to another state", { state: "building" }],
    ["the request re-entered the state", { entered_at: "2026-09-10T11:00:00Z" }],
  ])("Send back sends the stage shown when it opened, though %s underneath", async (_, next) => {
    const { server } = renderApp(
      <Changing render={(props) => <SendBackDialog {...props} />} next={next} />,
      { server: [reply] },
    );
    const dialog = await screen.findByRole("dialog", { name: "Send back" });
    await userEvent.type(within(dialog).getByLabelText("Reason"), "replan it");
    await waitFor(change);
    await userEvent.click(within(dialog).getByRole("button", { name: "Send back" }));
    await waitFor(() => {
      expect(server.sent(reply.on)).toHaveLength(1);
    });
    expect(server.sent(reply.on)[0]?.body).toMatchObject({ reason: "replan it", ...stage });
  });

  test.each([
    ["Request changes", RejectDialog],
    ["Send back", SendBackDialog],
  ] as const)(
    "%s: a 409 keeps the dialog and the typed text and shows the server's own sentence",
    async (name, Dialog) => {
      const message =
        "request req-spec-review is in plan_review now, not the spec_review you were shown";
      const { server } = renderApp(
        <Host request={specReview()} render={(props) => <Dialog {...props} />} />,
        {
          server: [
            { on: reply.on, reply: () => apiErrorResponse(409, message) },
            {
              on: "GET /requests/req-spec-review",
              reply: json(rawFixture("request-spec-review.json", { state: "plan_review" })),
            },
          ],
        },
      );
      const dialog = await screen.findByRole("dialog", { name });
      await userEvent.type(within(dialog).getByLabelText("Reason"), "too broad");
      await userEvent.click(within(dialog).getByRole("button", { name }));
      expect(await within(dialog).findByRole("alert")).toHaveTextContent(message);
      expect(screen.getByText("dialog open")).toBeInTheDocument();
      expect(within(dialog).getByLabelText("Reason")).toHaveValue("too broad");
      expect(server.sent(reply.on)).toHaveLength(1);
    },
  );
});
