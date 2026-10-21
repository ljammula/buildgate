import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { clearAllPendingCommentsForTest } from "@/platform/pendingComments";
import { json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire } from "./testRequests";

beforeEach(seedOperator);

afterEach(() => {
  clearAllPendingCommentsForTest();
});

const SPEC =
  "# Spec\n\n## Problem\n\nRefunds double-process.\n\n## Scope\n\nThe refund path.\n\n## Non-goals\n\nn\n\n## Affected services and packages\n\na\n\n## Acceptance criteria\n\n1. A repeat is refused.\n2. The first is kept.\n\n## Risks\n\nr\n\n## Open questions\n\nq\n";

const specReview = () => requestWire({ state: "spec_review", title: "Calc", spec: SPEC });
const panel = () => screen.getByRole("region", { name: "Spec" });

/** Selects `text` inside the raw view the way a reader dragging over it would. */
function select(text: string) {
  const box = within(panel()).getByTestId("markdown-raw-content");
  const walker = document.createTreeWalker(box, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    const at = node.textContent?.indexOf(text) ?? -1;
    if (at === -1) continue;
    const range = document.createRange();
    range.setStart(node, at);
    range.setEnd(node, at + text.length);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    act(() => {
      document.dispatchEvent(new Event("selectionchange"));
    });
    return;
  }
  throw new Error(`no text ${text} in the raw view`);
}

async function open() {
  const opened = openRequest(specReview(), {
    extra: [{ on: "POST /requests/req-1/reject", reply: json(specReview()) }],
  });
  await screen.findByRole("heading", { level: 1, name: "Calc" });
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
  });
  return opened;
}

async function comment(user: ReturnType<typeof userEvent.setup>, text: string, note: string) {
  select(text);
  await user.click(await within(panel()).findByRole("button", { name: "Comment on selection" }));
  await user.type(within(panel()).getByLabelText("Comment on this passage"), note);
  await user.click(within(panel()).getByRole("button", { name: "Add comment" }));
}

test("passages are commented on as a batch, then sent together with the passage of each", async () => {
  const { server } = await open();
  const user = userEvent.setup();

  await comment(user, "The first is kept.", "kept for how long?");
  await comment(user, "The refund path.", "name the endpoint");

  const batch = screen.getByRole("region", { name: "Comments to send (2)" });
  expect(
    within(batch)
      .getAllByRole("listitem")
      .map((item) => item.textContent),
  ).toEqual([
    "spec.md · Acceptance criteria · number 2The first is kept.: kept for how long?",
    "spec.md · ScopeThe refund path.: name the endpoint",
  ]);
  await user.click(screen.getByRole("button", { name: "Request changes (2)" }));
  expect(await screen.findByText(/Only the commented sections will be redrafted/)).toBeVisible();
  await user.click(
    within(screen.getByRole("dialog")).getByRole("button", { name: "Request changes" }),
  );

  await waitFor(() => {
    expect(server.sent("POST /requests/req-1/reject")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-1/reject")[0]?.body).toMatchObject({
    reason: "",
    anchors: [
      {
        path: "spec.md",
        section: "## Acceptance criteria",
        item: 2,
        quote: "The first is kept.",
        note: "kept for how long?",
      },
      {
        path: "spec.md",
        section: "## Scope",
        quote: "The refund path.",
        note: "name the endpoint",
      },
    ],
  });
  await waitFor(() => {
    expect(screen.queryByRole("region", { name: /Comments to send/ })).not.toBeInTheDocument();
  });
});

test("a pending comment can be removed before sending", async () => {
  await open();
  const user = userEvent.setup();
  await comment(user, "The refund path.", "name the endpoint");
  await user.click(screen.getByRole("button", { name: "Remove comment 1" }));
  expect(screen.queryByRole("region", { name: /Comments to send/ })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Request changes" })).toBeEnabled();
});

test("a passage that occurs twice is placed where it was selected", async () => {
  const dup = SPEC.replace("Refunds double-process.", "The refund path.").replace(
    "The refund path.\n\n## Non-goals",
    "The refund path.\n\n## Non-goals",
  );
  const opened = openRequest(requestWire({ state: "spec_review", title: "Calc", spec: dup }), {
    extra: [{ on: "POST /requests/req-1/reject", reply: json(specReview()) }],
  });
  await screen.findByRole("heading", { level: 1, name: "Calc" });
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
  });
  const user = userEvent.setup();
  // The second occurrence, the one under Scope.
  const box = within(panel()).getByTestId("markdown-raw-content");
  const node = [...box.querySelectorAll("span")]
    .map((e) => e.firstChild)
    .find((n) => n?.textContent?.includes("The refund path.")) as Text;
  const second = node.textContent.lastIndexOf("The refund path.");
  const range = document.createRange();
  range.setStart(node, second);
  range.setEnd(node, second + "The refund path.".length);
  window.getSelection()?.removeAllRanges();
  window.getSelection()?.addRange(range);
  act(() => {
    document.dispatchEvent(new Event("selectionchange"));
  });
  await user.click(await within(panel()).findByRole("button", { name: "Comment on selection" }));
  await user.type(within(panel()).getByLabelText("Comment on this passage"), "which endpoint?");
  await user.click(within(panel()).getByRole("button", { name: "Add comment" }));
  expect(screen.getByRole("region", { name: "Comments to send (1)" })).toHaveTextContent(
    "spec.md · Scope",
  );
  expect(opened.server).toBeDefined();
});
