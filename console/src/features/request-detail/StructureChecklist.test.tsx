import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { apiErrorResponse } from "@/test/render";

import { StructureChecklist } from "./StructureChecklist";
import { type OpenOptions, openRequest, seedOperator, stubRadix } from "./testHarness";
import { requestWire } from "./testRequests";

beforeAll(stubRadix);
beforeEach(seedOperator);

const SPEC =
  "# Spec\n\n## Problem\n\np\n\n## Scope\n\ns\n\n## Non-goals\n\nn\n\n" +
  "## Affected services and packages\n\na\n\n## Acceptance criteria\n\n1. It adds.\n2. It subtracts.\n\n" +
  "## Risks\n\nr\n\n## Open questions\n\nnone\n";

const checklist = () => screen.getByRole("region", { name: "Structure checklist" });
const failing = () =>
  within(checklist())
    .getAllByRole("listitem")
    .filter((item) => item.dataset.ok === "false")
    .map((item) => item.textContent);

test("a complete spec ticks every row and says the server checks again", () => {
  render(<StructureChecklist kind="spec" text={SPEC} />);

  expect(within(checklist()).getAllByRole("listitem")).toHaveLength(9);
  expect(failing()).toEqual([]);
  expect(within(checklist()).getAllByRole("listitem").at(-1)).toHaveTextContent(
    "OK: Numbered criteria (2 parsed)",
  );
  expect(within(checklist()).getByRole("status")).toHaveTextContent(
    "Structure is complete. The server checks the saved text again.",
  );
});

test("a ticket is checked against the ticket rules, header lines first", () => {
  render(<StructureChecklist kind="ticket" text={"## Goal\n\ng\n"} />);

  expect(failing().slice(0, 4)).toEqual([
    "Problem: Verify-Command: (missing)",
    "Problem: Allowed-Files: (missing)",
    "Problem: Required-Changed-Files: (missing)",
    "Problem: ## Plan (missing, or out of order)",
  ]);
  expect(within(checklist()).getByRole("status")).toHaveTextContent(
    "10 to fix. The server refuses a save without them.",
  );
});

test("agent-written text in a note is shown as text", () => {
  const ticket =
    "Verify-Command: true\nAllowed-Files: a\nRequired-Changed-Files: a\n## Goal\ng\n## Plan\n" +
    "### Files to touch\n- a\n### Steps\n1. s\n### Tests to add\n- t\n" +
    "### Acceptance criteria covered\n- <img src=x onerror=alert(1)>\n## Out of scope\nnone\n";
  render(<StructureChecklist kind="ticket" text={ticket} />);

  expect(failing()).toEqual([
    'Problem: Criteria covered ("- <img src=x onerror=alert(1)>" is not "- N")',
  ]);
  expect(checklist().querySelector("img")).toBeNull();
});

describe("in the spec editor", () => {
  const specReview = () =>
    requestWire({ state: "spec_review", title: "Add idempotency keys", spec: SPEC });
  const specPanel = () => screen.getByRole("region", { name: "Spec" });
  const editor = () => within(specPanel()).getByRole("textbox", { name: "Edit spec.md" });

  async function openEditor(routes: OpenOptions = {}) {
    const opened = openRequest(specReview(), routes);
    await screen.findByRole("heading", { level: 1, name: "Add idempotency keys" });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    });
    await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
    return opened;
  }

  test("the checklist is absent until Edit, then follows the text as it is typed", async () => {
    openRequest(specReview());
    await screen.findByRole("heading", { level: 1, name: "Add idempotency keys" });
    expect(screen.queryByRole("region", { name: "Structure checklist" })).not.toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    });
    await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
    expect(failing()).toEqual([]);

    await userEvent.clear(editor());
    await userEvent.type(editor(), "# Spec{Enter}## Problem{Enter}");

    expect(failing()).toHaveLength(7);
    expect(failing()[0]).toBe("Problem: ## Scope (missing, or out of order)");
  });

  test("a failing checklist does not disable Save, and the server's refusal still shows", async () => {
    const { server } = await openEditor({
      extra: [
        {
          on: "PUT /requests/req-1/spec",
          reply: () => apiErrorResponse(422, 'spec is missing required heading "## Scope"'),
        },
      ],
    });
    await userEvent.clear(editor());
    await userEvent.type(editor(), "# Spec");
    expect(failing()).not.toEqual([]);

    const save = within(specPanel()).getByRole("button", { name: "Save" });
    expect(save).toBeEnabled();
    await userEvent.click(save);

    expect(await within(specPanel()).findByRole("alert")).toHaveTextContent(
      'Could not save: spec is missing required heading "## Scope"',
    );
    expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
    expect(editor()).toHaveValue("# Spec");
  });
});
