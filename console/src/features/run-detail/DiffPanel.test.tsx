import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { DiffPanel } from "@/features/run-detail/DiffPanel";
import { apiErrorResponse, json, renderApp } from "@/test/render";

const route = "GET /runs/run-accepted/diff";

const twoFiles =
  "diff --git a/lib/a.dart b/lib/a.dart\n" +
  "--- a/lib/a.dart\n" +
  "+++ b/lib/a.dart\n" +
  "@@ -1,2 +1,2 @@\n" +
  "-old line\n" +
  "+new line\n" +
  "diff --git a/lib/b.dart b/lib/b.dart\n" +
  "--- a/lib/b.dart\n" +
  "+++ b/lib/b.dart\n" +
  "@@ -1,1 +1,1 @@\n" +
  "-old b\n" +
  "+new b\n";

const scrolled: Element[] = [];

beforeAll(() => {
  Object.assign(Element.prototype, {
    hasPointerCapture: () => false,
    scrollIntoView(this: Element) {
      scrolled.push(this);
    },
  });
});

beforeEach(() => {
  scrolled.length = 0;
});

function renderDiff(reply: Response | (() => Response | Promise<Response>)) {
  return renderApp(<DiffPanel runId="run-accepted" />, {
    server: [{ on: route, reply }],
  });
}

test("shows a spinner while the diff loads, then the diff", async () => {
  let release = () => {};
  const hold = new Promise<void>((resolve) => {
    release = resolve;
  });
  renderDiff(async () => {
    await hold;
    return json({ diff: "+added line\n-removed line\n", truncated: false });
  });

  expect(screen.getByRole("status", { name: "Loading diff" })).toBeInTheDocument();
  release();
  expect(await screen.findByText("+added line")).toBeInTheDocument();
  expect(screen.getByText("-removed line")).toBeInTheDocument();
  expect(screen.queryByRole("status", { name: "Loading diff" })).not.toBeInTheDocument();
});

test("a diff that is not collected is reported, not shown as empty", async () => {
  renderDiff(apiErrorResponse(404, "diff not found"));

  expect(await screen.findByText("Not found")).toBeInTheDocument();
  expect(screen.queryByText("No changes.")).not.toBeInTheDocument();
});

test("truncated diff shows a warning banner", async () => {
  renderDiff(json({ diff: "+added line\n", truncated: true }));

  expect(await screen.findByText(/truncated/)).toBeInTheDocument();
  expect(screen.getByText("+added line")).toBeInTheDocument();
});

test("shows 'No changes.' for an empty diff", async () => {
  renderDiff(json({ diff: "", truncated: false }));

  expect(await screen.findByText("No changes.")).toBeInTheDocument();
});

// A chip per changed file, read from the diff's own headers, scrolls the diff
// to that file's first line.
test("offers a jump-to-file chip list", async () => {
  renderDiff(json({ diff: twoFiles, truncated: false }));

  const list = await screen.findByRole("list", { name: "Changed files" });
  expect(list).toHaveTextContent("lib/a.dartlib/b.dart");

  await userEvent.click(screen.getByRole("button", { name: "lib/b.dart" }));
  await waitFor(() => {
    expect(scrolled).toHaveLength(1);
  });
  expect(scrolled[0]).toHaveTextContent("diff --git a/lib/b.dart b/lib/b.dart");
});
