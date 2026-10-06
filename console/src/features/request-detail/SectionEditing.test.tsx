import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { sha256Hex } from "@/domain/contentHash";
import { json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire } from "./testRequests";

beforeEach(seedOperator);

// A textarea shows CRLF as LF; the text state (and the PUT) keep the CR.
const shown = (t: string) => t.replace(/\r\n/g, "\n");

const SPEC =
  "# Spec  \n\n## Problem\n\n  odd <b>spacing</b>  \n\n\n## Scope\r\nkept\t\r\n\n## Non-goals\nn\n\n## Affected services and packages\na\n\n## Acceptance criteria\n\n1. It adds.\n\n## Risks\nr\n\n## Open questions\nq";

const specReview = (spec = SPEC) => requestWire({ state: "spec_review", title: "Calc", spec });
const panel = () => screen.getByRole("region", { name: "Spec" });

async function edit(spec = SPEC, extra: Parameters<typeof openRequest>[1] = {}) {
  const opened = openRequest(specReview(spec), extra);
  await screen.findByRole("heading", { level: 1, name: "Calc" });
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
  });
  await userEvent.click(within(panel()).getByRole("button", { name: "Edit" }));
  return opened;
}

test("editing one section sends the whole file with every other byte unchanged, via the usual PUT", async () => {
  const { server } = await edit(SPEC, {
    extra: [{ on: "PUT /requests/req-1/spec", reply: json(specReview("x")) }],
  });
  const raw = within(panel()).getByRole("textbox", { name: "Edit spec.md" });

  await userEvent.click(within(panel()).getByRole("button", { name: "Edit section Risks" }));
  const field = within(panel()).getByRole("textbox", { name: "Section Risks" });
  expect(field).toHaveValue("r");
  await userEvent.clear(field);
  await userEvent.type(field, "new risk");
  expect(raw).toHaveValue(shown(SPEC));
  expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(0);
  // Save sends the text below, not an open field: it waits for Apply or Cancel.
  expect(within(panel()).getByRole("button", { name: "Save" })).toBeDisabled();
  expect(within(panel()).getByTestId("edit-section-pending")).toHaveTextContent(
    "A section is open: Apply or Cancel it before saving.",
  );
  await userEvent.click(within(panel()).getByRole("button", { name: "Apply section Risks" }));
  expect(within(panel()).getByRole("button", { name: "Save" })).toBeEnabled();

  const expected = SPEC.replace("## Risks\nr\n", "## Risks\nnew risk\n");
  expect(raw).toHaveValue(shown(expected));
  expect(within(panel()).getByRole("region", { name: "Your changes" })).toBeVisible();
  await userEvent.click(within(panel()).getByRole("button", { name: "Save" }));

  await waitFor(() => {
    expect(server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
  });
  expect(server.sent("PUT /requests/req-1/spec")[0]?.body).toEqual({
    content: expected,
    base_sha256: sha256Hex(SPEC),
    by: "operator",
  });
});

test("a body with a required heading line cannot be applied, and Cancel discards the draft", async () => {
  await edit();
  await userEvent.click(within(panel()).getByRole("button", { name: "Edit section Problem" }));
  const field = within(panel()).getByRole("textbox", { name: "Section Problem" });
  expect(field).toHaveValue("\n  odd <b>spacing</b>  ");
  await userEvent.type(field, "\n## Scope");

  expect(within(panel()).getByRole("button", { name: "Apply section Problem" })).toBeDisabled();
  expect(within(panel()).getByText(/would add or move a section heading/)).toBeVisible();
  await userEvent.click(within(panel()).getByRole("button", { name: "Cancel section Problem" }));
  expect(within(panel()).queryByRole("textbox", { name: "Section Problem" })).toBeNull();
  expect(within(panel()).getByRole("textbox", { name: "Edit spec.md" })).toHaveValue(shown(SPEC));
});

test("a missing section says so and has no Edit", async () => {
  await edit("# Spec\n\n## Problem\np\n");
  expect(within(panel()).queryByRole("button", { name: "Edit section Scope" })).toBeNull();
  expect(within(panel()).getAllByText("missing").length).toBeGreaterThan(0);
  expect(within(panel()).getByRole("button", { name: "Edit section Problem" })).toBeVisible();
});

test("outside editing the spec is shown whole and no section controls exist", async () => {
  openRequest(specReview());
  await screen.findByRole("heading", { level: 1, name: "Calc" });
  expect(screen.queryByRole("button", { name: /Edit section/ })).toBeNull();
});
