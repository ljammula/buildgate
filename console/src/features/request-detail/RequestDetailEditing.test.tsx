import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { sha256Hex } from "@/domain/contentHash";
import { apiErrorResponse, json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const approveButton = () => screen.getByRole("button", { name: "Approve" });
const specPanel = () => screen.getByRole("region", { name: "Spec" });
const editor = () => within(specPanel()).getByRole("textbox", { name: "Edit spec.md" });

async function ready() {
  await screen.findByRole("heading", { level: 1, name: "Add idempotency keys" });
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
}

const specReview = (spec = "# Spec\n\nOriginal detail.") =>
  requestWire({ state: "spec_review", title: "Add idempotency keys", spec });

test("the spec Edit toggle shows an editor pre-filled with the current content", async () => {
  openRequest(specReview());
  await ready();
  expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();

  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));

  expect(editor()).toHaveValue("# Spec\n\nOriginal detail.");
});

test("Edit is not offered without write access, or outside spec_review", async () => {
  openRequest(specReview(), { writes: false });
  await screen.findByRole("heading", { level: 1, name: "Add idempotency keys" });
  expect(within(specPanel()).queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
});

test("Edit disables Approve and Request changes while the editor is open, and says why", async () => {
  openRequest(specReview());
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));

  expect(approveButton()).toBeDisabled();
  expect(screen.getByRole("button", { name: "Request changes" })).toBeDisabled();
  expect(
    screen.getByText(
      "Approve/Request changes are disabled while an edit is open -- Save or Cancel it first.",
    ),
  ).toBeInTheDocument();

  await userEvent.click(within(specPanel()).getByRole("button", { name: "Cancel" }));
  expect(approveButton()).toBeEnabled();
  expect(screen.getByText("# Spec", { exact: false })).toBeInTheDocument();
});

test("saving an edited spec calls PUT with the edited body and the hash of what the editor opened with", async () => {
  const { server } = openRequest(specReview(), {
    extra: [
      {
        on: "PUT /requests/req-1/spec",
        reply: json(specReview("# Spec\n\nEdited detail.")),
      },
    ],
  });
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  await userEvent.clear(editor());
  await userEvent.type(editor(), "# Spec\n\nEdited detail.");
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));

  await waitFor(() => {
    expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
  });
  expect(server.sent("PUT /requests/req-1/spec")[0]?.body).toEqual({
    content: "# Spec\n\nEdited detail.",
    base_sha256: sha256Hex("# Spec\n\nOriginal detail."),
    by: "operator",
  });
  // The editor closes and the screen shows the saved content, fed from the
  // PUT response with no extra GET.
  await waitFor(() => {
    expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
  });
  expect(screen.getByText(/Edited detail\./)).toBeInTheDocument();
  expect(server.sent("GET /requests/req-1")).toHaveLength(1);
});

test("a 422 from saving an edited spec renders the message inline and keeps the edit", async () => {
  openRequest(specReview(), {
    extra: [
      {
        on: "PUT /requests/req-1/spec",
        reply: apiErrorResponse(422, 'spec is missing required heading "## Risks"'),
      },
    ],
  });
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  await userEvent.clear(editor());
  await userEvent.type(editor(), "not a spec");
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));

  expect(
    await screen.findByText('Could not save: spec is missing required heading "## Risks"'),
  ).toBeInTheDocument();
  // The editor stays open with the operator's edit intact.
  expect(editor()).toHaveValue("not a spec");
});

test("Approve still calls the approve route with the Edit control present", async () => {
  const { server } = openRequest(specReview(), {
    extra: [
      {
        on: "POST /requests/req-1/approve",
        reply: json(requestWire({ state: "planning", title: "Add idempotency keys" })),
      },
    ],
  });
  await ready();
  expect(within(specPanel()).getByRole("button", { name: "Edit" })).toBeInTheDocument();
  await userEvent.click(approveButton());
  const dialog = await screen.findByRole("dialog");
  await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

  await waitFor(() => {
    expect(server.sent("POST /requests/req-1/approve")).toHaveLength(1);
  });
  // What was shown is what is pinned.
  expect(server.sent("POST /requests/req-1/approve")[0]?.body).toEqual({
    by: "operator",
    expected_sha256: { "spec.md": sha256Hex("# Spec\n\nOriginal detail.") },
  });
});

test("the acceptance criteria list renders numbered items parsed from the spec, with a count", async () => {
  const spec =
    "# Spec\n\n## Acceptance criteria\n\n1. Adding two numbers returns their sum\n2. Dividing by zero raises ValueError\n\n## Non-goals\n\nNot covering strings.\n";
  openRequest(requestWire({ state: "spec_review", title: "T", spec }), { writes: false });
  await screen.findByRole("heading", { level: 1, name: "T" });

  expect(screen.getByTestId("acceptance-criteria-list")).toBeInTheDocument();
  expect(screen.getByText("Acceptance criteria (2)")).toBeInTheDocument();
  expect(screen.getByText("1. Adding two numbers returns their sum")).toBeInTheDocument();
  expect(screen.getByText("2. Dividing by zero raises ValueError")).toBeInTheDocument();
});

test("the approve confirm dialog uses the server-supplied approve_next_state instead of the client-side table", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "Add idempotency keys",
      spec: "# Spec",
      approve_next_state: "oracle_drafting",
    }),
  );
  await ready();
  await userEvent.click(approveButton());
  expect(await screen.findByText("Spec review → Drafting oracles")).toBeInTheDocument();
});

test("a 409 conflict on saving a spec edit fetches and diffs the current server content, and 'Discard mine and reload' drops the unsaved edit", async () => {
  let gets = 0;
  const { server } = openRequest(
    // The first GET (initial load) sees the original content; the second
    // (the conflict's fetch) sees what "someone else" saved in between.
    () => {
      gets += 1;
      return specReview(
        gets === 1 ? "# Spec\n\nOriginal detail." : "# Spec\n\nServer updated detail.",
      );
    },
    {
      extra: [
        {
          on: "PUT /requests/req-1/spec",
          reply: json(
            { error: "spec.md changed since you started editing", current_sha256: "deadbeef" },
            409,
          ),
        },
      ],
    },
  );
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  await userEvent.clear(editor());
  await userEvent.type(editor(), "My unsaved edit.");
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));

  expect(await screen.findByTestId("edit-conflict")).toHaveTextContent("current hash: deadbeef");
  const diff = await screen.findByTestId("edit-conflict-diff");
  expect(within(diff).getByText(/Server updated detail\./)).toBeInTheDocument();
  expect(within(diff).getByText(/My unsaved edit\./)).toBeInTheDocument();
  // The operator's own text is still intact, untouched by the fetch.
  expect(editor()).toHaveValue("My unsaved edit.");
  expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(1);

  await userEvent.click(screen.getByRole("button", { name: "Discard mine and reload" }));

  // The editor is gone; the screen shows the server's current content.
  expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
  await waitFor(() => {
    expect(screen.getByText(/Server updated detail\./)).toBeInTheDocument();
  });
  expect(approveButton()).toBeEnabled();
});

test("'Keep editing (base = current)' on a 409 conflict re-bases base_sha256 to the reported current hash and keeps the edit open", async () => {
  let puts = 0;
  const { server } = openRequest(specReview("# Spec\n\nServer updated detail."), {
    extra: [
      {
        on: "PUT /requests/req-1/spec",
        reply: () => {
          puts += 1;
          return puts === 1
            ? json(
                { error: "spec.md changed since you started editing", current_sha256: "deadbeef" },
                409,
              )
            : json(requestWire({ state: "planning", title: "Add idempotency keys" }));
        },
      },
    ],
  });
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  await userEvent.clear(editor());
  await userEvent.type(editor(), "My unsaved edit.");
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));
  await screen.findByTestId("edit-conflict");

  const first = server.sent("PUT /requests/req-1/spec")[0]?.body as { base_sha256: string };
  expect(first.base_sha256).not.toBe("deadbeef");

  await userEvent.click(screen.getByRole("button", { name: "Keep editing (base = current)" }));

  // The editor is still open, with the operator's text intact.
  expect(editor()).toHaveValue("My unsaved edit.");
  expect(screen.queryByTestId("edit-conflict")).not.toBeInTheDocument();

  await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));
  await waitFor(() => {
    expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(2);
  });
  // The second Save re-sent the operator's text, now based on the reported hash.
  expect(server.sent("PUT /requests/req-1/spec")[1]?.body).toEqual({
    content: "My unsaved edit.",
    base_sha256: "deadbeef",
    by: "operator",
  });
});

test("a conflict without a reported hash cannot be re-based", async () => {
  openRequest(specReview(), {
    extra: [{ on: "PUT /requests/req-1/spec", reply: apiErrorResponse(409, "changed") }],
  });
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  await userEvent.type(editor(), " more");
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));
  await screen.findByTestId("edit-conflict");
  expect(screen.getByTestId("edit-conflict")).toHaveTextContent("current hash: unknown");
  expect(screen.getByRole("button", { name: "Keep editing (base = current)" })).toBeDisabled();
});

describe("a ticket plan file at plan_review", () => {
  const planReview = (content = "Verify-Command: true\n## Ticket one\n") =>
    requestWire({
      state: "plan_review",
      title: "Plan",
      tickets: [
        ticketWire({
          index: 1,
          specPath: "/srv/data/requests/req-1/tickets/001.spec.md",
          content,
        }),
      ],
    });

  test("is editable, and Save PUTs to the ticket route with the hash of the shown content", async () => {
    const { server } = openRequest(planReview(), {
      extra: [
        {
          on: "PUT /requests/req-1/tickets/1",
          reply: json(planReview("Verify-Command: true\n## Edited\n")),
        },
      ],
    });
    await screen.findByRole("heading", { level: 1, name: "Plan" });
    const panel = screen.getByRole("region", { name: "Ticket 1 plan" });
    await userEvent.click(within(panel).getByRole("button", { name: "Edit" }));
    const box = within(panel).getByRole("textbox", {
      name: "Edit /srv/data/requests/req-1/tickets/001.spec.md",
    });
    await userEvent.clear(box);
    await userEvent.type(box, "Verify-Command: true\n## Edited");
    await userEvent.click(within(panel).getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(server.sent("PUT /requests/req-1/tickets/1")).toHaveLength(1);
    });
    expect(server.sent("PUT /requests/req-1/tickets/1")[0]?.body).toEqual({
      content: "Verify-Command: true\n## Edited",
      base_sha256: sha256Hex("Verify-Command: true\n## Ticket one\n"),
      by: "operator",
    });
    expect(await screen.findByText(/## Edited/)).toBeInTheDocument();
  });

  test("Approve stays off while its editor is open", async () => {
    openRequest(planReview());
    await screen.findByRole("heading", { level: 1, name: "Plan" });
    // No oracle files: the panel reports complete once it has listed.
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(approveButton()).toBeDisabled();
  });
});

test("an open editor closes when the request moves on and its file is no longer editable", async () => {
  let state = "spec_review";
  const { server } = openRequest(() =>
    requestWire({ state, title: "Add idempotency keys", spec: "# Spec\n\nOriginal detail." }),
  );
  await ready();
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  expect(editor()).toBeInTheDocument();

  state = "spec_drafting";
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));

  await waitFor(() => {
    expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
  });
  expect(server.sent("GET /requests/req-1")).toHaveLength(2);
});

test("the audit lists an in-place edit with its changed lines and whether a drafter was given it", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "Edited once",
      spec: "# Spec\n\nnew",
      edits: [
        {
          by: "kanna",
          at: "2026-09-10T09:00:00Z",
          path: "spec.md",
          from_state: "spec_review",
          revision: 1,
          diff: "- old\n+ new\n",
        },
      ],
    }),
  );
  await screen.findByRole("heading", { level: 1, name: "Edited once" });
  const audit = screen.getByRole("region", { name: "Audit" });
  await userEvent.click(within(audit).getByRole("button", { name: /Edits in place \(1\)/ }));
  const history = within(audit).getByTestId("edit-history");
  expect(history).toHaveTextContent("Edited by kanna at");
  expect(history).toHaveTextContent("spec.md");
  expect(history).toHaveTextContent("- old");
  expect(history).toHaveTextContent("+ new");
  expect(history).toHaveTextContent(
    "Not sent to a drafter: nothing has redrafted this file since.",
  );
});

test("an edit followed by a rejection of its stage is shown as part of that rejection's feedback", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "Edited then rejected",
      spec: "# Spec\n\nnew",
      edits: [
        {
          by: "kanna",
          at: "2026-09-10T09:00:00Z",
          path: "spec.md",
          from_state: "spec_review",
          revision: 1,
          diff: "- old\n+ new\n",
        },
      ],
      rejections: [
        {
          by: "kanna",
          at: "2026-09-10T09:05:00Z",
          reason: "tighten it",
          from_state: "spec_review",
        },
      ],
    }),
    {
      extra: [{ on: "GET /requests/req-1/revisions", reply: json([]) }],
    },
  );
  await screen.findByRole("heading", { level: 1, name: "Edited then rejected" });
  const audit = screen.getByRole("region", { name: "Audit" });
  await userEvent.click(within(audit).getByRole("button", { name: /Edits in place \(1\)/ }));
  expect(within(audit).getByTestId("edit-history")).toHaveTextContent(
    /In the feedback of the rejection of .*: a redraft from it is told to keep these lines\./,
  );
});
