import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { sha256Hex } from "@/domain/contentHash";
import { json } from "@/test/render";

import { type OpenOptions, openRequest, seedOperator } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const SPEC =
  "# Spec\n\n## Problem\n\np\n\n## Acceptance criteria\n\n1. It adds.\n2. It subtracts\n   exactly.\n3. It multiplies.\n\n## Risks\n\nr\n";

const TICKET =
  "Verify-Command: go test ./...\nAllowed-Files: a.go\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n" +
  "### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n" +
  "### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nnone\n";

describe("a spec's criteria as a list", () => {
  const specReview = () => requestWire({ state: "spec_review", title: "Calculator", spec: SPEC });
  const panel = () => screen.getByRole("region", { name: "Spec" });
  const raw = () => within(panel()).getByRole("textbox", { name: "Edit spec.md" });
  const list = () => within(panel()).getByRole("region", { name: "Acceptance criteria" });
  const criterion = (n: number) => within(list()).getByRole("textbox", { name: `Criterion ${n}` });

  async function edit(extra: OpenOptions = {}) {
    const opened = openRequest(specReview(), extra);
    await screen.findByRole("heading", { level: 1, name: "Calculator" });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    });
    await userEvent.click(within(panel()).getByRole("button", { name: "Edit" }));
    return opened;
  }

  test("each criterion has its own field, continuation lines included", async () => {
    await edit();

    expect(within(list()).getByRole("heading", { name: "Acceptance criteria (3)" })).toBeVisible();
    expect(criterion(1)).toHaveValue("It adds.");
    expect(criterion(2)).toHaveValue("It subtracts\n   exactly.");
    expect(raw()).toHaveValue(SPEC);
    expect(within(panel()).queryByRole("region", { name: "Your changes" })).toBeNull();
  });

  test("editing a field rewrites that criterion in the text and nothing else", async () => {
    await edit();

    await userEvent.type(criterion(1), " Twice.");

    expect(raw()).toHaveValue(SPEC.replace("1. It adds.", "1. It adds. Twice."));
  });

  test("an emptied field is held, not written: the text keeps the criterion until there is text again", async () => {
    await edit();

    await userEvent.clear(criterion(3));
    expect(criterion(3)).toHaveValue("");
    expect(raw()).toHaveValue(SPEC);

    await userEvent.type(criterion(3), "It divides.");
    expect(raw()).toHaveValue(SPEC.replace("3. It multiplies.", "3. It divides."));
  });

  test("add, remove and reorder keep the numbers in step", async () => {
    await edit();

    await userEvent.type(
      within(list()).getByRole("textbox", { name: "New criterion" }),
      "It divides.{Enter}",
    );
    expect(raw()).toHaveValue(
      SPEC.replace("3. It multiplies.\n", "3. It multiplies.\n4. It divides.\n"),
    );
    expect(within(list()).getByRole("textbox", { name: "New criterion" })).toHaveValue("");

    await userEvent.click(within(list()).getByRole("button", { name: "Remove criterion 1" }));
    expect(raw()).toHaveValue(
      SPEC.replace(
        "1. It adds.\n2. It subtracts\n   exactly.\n3. It multiplies.\n",
        "1. It subtracts\n   exactly.\n2. It multiplies.\n3. It divides.\n",
      ),
    );

    await userEvent.click(within(list()).getByRole("button", { name: "Move criterion 3 up" }));
    expect(criterion(2)).toHaveValue("It divides.");
    expect(criterion(3)).toHaveValue("It multiplies.");
    expect(within(list()).getByRole("button", { name: "Move criterion 1 up" })).toBeDisabled();
    expect(within(list()).getByRole("button", { name: "Move criterion 3 down" })).toBeDisabled();
  });

  test("Alt+Down moves the criterion and the focus goes with it", async () => {
    await edit();

    criterion(1).focus();
    await userEvent.keyboard("{Alt>}{ArrowDown}{/Alt}");

    expect(criterion(2)).toHaveValue("It adds.");
    expect(criterion(2)).toHaveFocus();
  });

  test("typing in the text is read back into the list", async () => {
    await edit();

    fireEvent.change(raw(), { target: { value: SPEC.replace("1. It adds.\n", "") } });

    expect(within(list()).getByRole("heading", { name: "Acceptance criteria (2)" })).toBeVisible();
    expect(criterion(1)).toHaveValue("It subtracts\n   exactly.");
  });

  test("Save sends the text shown, against the hash of the text opened, and the changes are shown first", async () => {
    const saved = SPEC.replace("3. It multiplies.\n", "");
    const { server } = await edit({
      extra: [
        {
          on: "PUT /requests/req-1/spec",
          reply: json(requestWire({ state: "spec_review", title: "Calculator", spec: saved })),
        },
      ],
    });

    await userEvent.click(within(list()).getByRole("button", { name: "Remove criterion 3" }));
    const changes = within(panel()).getByRole("region", { name: "Your changes" });
    expect(changes).toHaveTextContent("3. It multiplies.");
    const shown = (raw() as HTMLTextAreaElement).value;
    await userEvent.click(within(panel()).getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
    });
    expect(server.sent("PUT /requests/req-1/spec")[0]?.body).toEqual({
      content: shown,
      base_sha256: sha256Hex(SPEC),
      by: "operator",
    });
    expect(shown).toBe(saved);
  });

  test("without the heading the list says how to get one, and the text is still editable", async () => {
    openRequest(requestWire({ state: "spec_review", title: "Calculator", spec: "# Spec\n" }));
    await screen.findByRole("heading", { level: 1, name: "Calculator" });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    });
    await userEvent.click(within(panel()).getByRole("button", { name: "Edit" }));

    expect(
      within(panel()).getByText(
        'Add a "## Acceptance criteria" heading to edit the criteria as a list.',
      ),
    ).toBeVisible();
    expect(raw()).toHaveValue("# Spec\n");
  });
});

describe("a ticket's header lines and covered criteria as fields", () => {
  const planReview = (content = TICKET) =>
    requestWire({
      state: "plan_review",
      title: "Calculator",
      spec: SPEC,
      tickets: [ticketWire({ index: 1, specPath: "tickets/001.spec.md", content })],
    });
  const panel = () => screen.getByRole("region", { name: "Ticket 1 plan" });
  const raw = () => within(panel()).getByRole("textbox", { name: "Edit tickets/001.spec.md" });
  const fields = () => within(panel()).getByRole("region", { name: "Ticket fields" });

  async function edit(content = TICKET, extra: OpenOptions = {}) {
    const opened = openRequest(planReview(content), extra);
    await screen.findByRole("heading", { level: 1, name: "Calculator" });
    await userEvent.click(within(panel()).getByRole("button", { name: "Edit" }));
    return opened;
  }

  test("the fields show the header values and the spec's criteria, the covered ones ticked", async () => {
    await edit();

    expect(within(fields()).getByRole("textbox", { name: "Verify command" })).toHaveValue(
      "go test ./...",
    );
    expect(within(fields()).getByRole("textbox", { name: "Allowed files" })).toHaveValue("a.go");
    expect(
      within(fields())
        .getAllByRole("checkbox")
        .map((box) => [box.parentElement?.textContent, (box as HTMLInputElement).checked]),
    ).toEqual([
      ["1. It adds.", true],
      ["2. It subtracts exactly.", false],
      ["3. It multiplies.", false],
    ]);
  });

  test("typing a header value rewrites its line; a list keeps the comma and space being typed", async () => {
    await edit();

    await userEvent.type(
      within(fields()).getByRole("textbox", { name: "Allowed files" }),
      ", a_test.go",
    );

    expect(within(fields()).getByRole("textbox", { name: "Allowed files" })).toHaveValue(
      "a.go, a_test.go",
    );
    expect(raw()).toHaveValue(
      TICKET.replace("Allowed-Files: a.go", "Allowed-Files: a.go, a_test.go"),
    );
    expect(within(fields()).getByText("2 paths, comma-separated")).toBeVisible();
  });

  test("ticking a criterion rewrites the covered list in order", async () => {
    await edit();

    await userEvent.click(within(fields()).getByRole("checkbox", { name: "3. It multiplies." }));
    expect(raw()).toHaveValue(TICKET.replace("- 1\n", "- 1\n- 3\n"));

    await userEvent.click(within(fields()).getByRole("checkbox", { name: "1. It adds." }));
    expect(raw()).toHaveValue(TICKET.replace("- 1\n", "- 3\n"));
  });

  test("a claim the spec has no criterion for is shown, so it can be unticked", async () => {
    await edit(TICKET.replace("- 1\n", "- 1\n- 9\n"));

    const stray = within(fields()).getByRole("checkbox", { name: "9 (not in the spec)" });
    expect(stray).toBeChecked();
    await userEvent.click(stray);

    expect(raw()).toHaveValue(TICKET);
  });

  test("a missing header line is said, and typing adds it", async () => {
    await edit(TICKET.replace("Allowed-Files: a.go\n", ""));

    expect(
      within(fields()).getByText("No Allowed-Files: line yet: typing here adds it."),
    ).toBeVisible();
    await userEvent.type(within(fields()).getByRole("textbox", { name: "Allowed files" }), "a.go");

    expect(raw()).toHaveValue(
      TICKET.replace(
        "Allowed-Files: a.go\nRequired-Changed-Files: a.go\n",
        "Required-Changed-Files: a.go\nAllowed-Files: a.go\n",
      ),
    );
  });

  test("Save sends the text shown against the hash of the text opened", async () => {
    const { server } = await edit(TICKET, {
      extra: [{ on: "PUT /requests/req-1/tickets/1", reply: json(planReview()) }],
    });

    await userEvent.clear(within(fields()).getByRole("textbox", { name: "Verify command" }));
    await userEvent.type(
      within(fields()).getByRole("textbox", { name: "Verify command" }),
      "make verify",
    );
    const shown = (raw() as HTMLTextAreaElement).value;
    await userEvent.click(within(panel()).getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(server.sent("PUT /requests/req-1/tickets/1")).toHaveLength(1);
    });
    expect(server.sent("PUT /requests/req-1/tickets/1")[0]?.body).toEqual({
      content: shown,
      base_sha256: sha256Hex(TICKET),
      by: "operator",
    });
    expect(shown).toBe(TICKET.replace("go test ./...", "make verify"));
  });

  test("ticket list editors rewrite the shared draft and Save sends untouched bytes", async () => {
    const content = TICKET.replace(
      "- a.go\n\n### Steps\n\n1. s\n",
      "- a.go\n* b.go\n\n### Steps\n\n1. s\n2) second\n",
    );
    const { server } = await edit(content, {
      extra: [{ on: "PUT /requests/req-1/tickets/1", reply: json(planReview(content)) }],
    });
    const panelNode = panel();
    const steps = within(panelNode).getByRole("region", { name: "Steps" });
    const files = within(panelNode).getByRole("region", { name: "Files to touch" });

    await userEvent.type(within(steps).getByRole("textbox", { name: "Step 1" }), " updated");
    await userEvent.type(within(files).getByRole("textbox", { name: "New file" }), "c.go{Enter}");
    await userEvent.click(within(steps).getByRole("button", { name: "Remove step 2" }));
    await userEvent.click(within(files).getByRole("button", { name: "Move file 2 up" }));

    const expected =
      "Verify-Command: go test ./...\n" +
      "Allowed-Files: a.go\n" +
      "Required-Changed-Files: a.go\n\n" +
      "## Goal\n\n" +
      "g\n\n" +
      "## Plan\n\n" +
      "### Files to touch\n\n" +
      "* b.go\n" +
      "- a.go\n" +
      "- c.go\n\n" +
      "### Steps\n\n" +
      "1. s updated\n\n" +
      "### Tests to add\n\n" +
      "- t\n\n" +
      "### Acceptance criteria covered\n\n" +
      "- 1\n\n" +
      "## Out of scope\n\n" +
      "none\n";
    await userEvent.click(within(panelNode).getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(server.sent("PUT /requests/req-1/tickets/1")).toHaveLength(1);
    });
    expect(server.sent("PUT /requests/req-1/tickets/1")[0]?.body).toEqual({
      content: expected,
      base_sha256: sha256Hex(content),
      by: "operator",
    });
  });

  test("a step line that reads as a required heading is refused, and the text keeps its sections", async () => {
    await edit(TICKET);
    const panelNode = panel();
    const steps = within(panelNode).getByRole("region", { name: "Steps" });
    const raw = within(panelNode).getByRole("textbox", { name: /^Edit / });

    await userEvent.type(
      within(steps).getByRole("textbox", { name: "Step 1" }),
      "\n## Out of scope",
    );

    expect(within(steps).getByText(/would add or move a section heading/)).toBeVisible();
    // Everything typed before the heading line was written; the heading line was not.
    expect(raw).toHaveValue(TICKET.replace("1. s\n", "1. s\n## Out of scop\n"));
  });
});
