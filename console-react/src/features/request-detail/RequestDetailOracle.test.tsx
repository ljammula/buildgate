// The screen halves of the oracle-stage flows: what the request page does
// with the oracle panels (the panels' own behaviour is tested beside them).
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { sha256Hex, sha256HexBytes } from "@/domain/contentHash";
import { bytesResponse, enc, oracleServer, toggle } from "@/shared/oracle/oracleTestKit";
import { type FakeRoute, apiErrorResponse, fakeServer, json, renderApp } from "@/test/render";

import { RequestDetailScreen } from "./RequestDetailScreen";
import { seedOperator, stubRadix } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeAll(stubRadix);
beforeEach(seedOperator);

type OracleFiles = Record<string, string>;

function openOracleReview(
  files: OracleFiles,
  options: {
    draft?: Record<string, unknown>;
    state?: string;
    approve?: Response;
  } = {},
) {
  const state = options.state ?? "oracle_review";
  const oracle = oracleServer(files);
  oracle.state = state;
  oracle.draft = options.draft ?? null;
  const body = requestWire({
    state,
    title: "T",
    ...(options.draft === undefined ? {} : { oracle_draft: options.draft }),
  });
  oracle.server.set("GET /requests/req-1", () => json(body));
  oracle.server.set(
    "POST /requests/req-1/approve",
    () => options.approve ?? json(requestWire({ state: "planning", title: "T" })),
  );
  const view = renderApp(<RequestDetailScreen />, {
    server: oracle.server,
    path: "/requests/req-1",
    pattern: "/requests/:id",
  });
  return { ...view, oracle };
}

const panel = () => screen.getByTestId("oracle-review-panel");
const approve = () => screen.getByRole("button", { name: /^Approve/ });

async function ready() {
  await screen.findByRole("heading", { level: 1, name: "T" });
  await screen.findByRole("button", { name: "Reload files" });
}

describe("oracle_review", () => {
  test("an empty oracle/ approves as a skip with no hashes", async () => {
    const { oracle } = openOracleReview({});
    await ready();
    expect(screen.getByTestId("oracle-empty")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve (skip oracle)" })).toBeInTheDocument();
    await waitFor(() => {
      expect(approve()).toBeEnabled();
    });

    await userEvent.click(approve());
    const dialog = await screen.findByRole("dialog", { name: "Approve this request?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    await waitFor(() => {
      expect(oracle.server.sent("POST /requests/req-1/approve")).toHaveLength(1);
    });
    expect(oracle.server.sent("POST /requests/req-1/approve")[0]?.body).toEqual({ by: "operator" });
  });

  test("approving an empty oracle/ after a failed draft warns in the confirm dialog, and the approval is still allowed", async () => {
    const { oracle } = openOracleReview(
      {},
      { draft: { status: "failed", detail: "oracle drafting failed: boom" } },
    );
    await ready();
    await waitFor(() => {
      expect(approve()).toBeEnabled();
    });
    await userEvent.click(approve());
    const dialog = await screen.findByRole("dialog");

    expect(within(dialog).getByText(/skips the oracle stage/)).toBeInTheDocument();
    expect(within(dialog).getAllByText(/boom/).length).toBeGreaterThan(0);
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));
    await waitFor(() => {
      expect(oracle.server.sent("POST /requests/req-1/approve")).toHaveLength(1);
    });
  });

  test("a very long skip warning does not stop the approval from being confirmed", async () => {
    const { oracle } = openOracleReview(
      {},
      { draft: { status: "failed", detail: "boom ".repeat(4000) } },
    );
    await ready();
    await waitFor(() => {
      expect(approve()).toBeEnabled();
    });
    await userEvent.click(approve());
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));
    await waitFor(() => {
      expect(oracle.server.sent("POST /requests/req-1/approve")).toHaveLength(1);
    });
  });

  test("a none_eligible draft skips quietly: no warning, but the dialog still states what skipping costs", async () => {
    openOracleReview({}, { draft: { status: "none_eligible", detail: "nothing checkable" } });
    await ready();
    await waitFor(() => {
      expect(approve()).toBeEnabled();
    });
    await userEvent.click(approve());
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByText(/skips the oracle stage/)).not.toBeInTheDocument();
    expect(
      within(dialog).getByText(
        "No oracle files: the build will have no request-level acceptance test.",
      ),
    ).toBeInTheDocument();
  });

  test("approving with files sends the hash of every file shown, and Approve waits for them all", async () => {
    const files = { "RUN_COMMAND.txt": "go test ./...\n", "a_oracle_test.go": "package a\n" };
    const { oracle } = openOracleReview(files);
    await ready();
    expect(approve()).toBeDisabled();
    await toggle(panel(), "RUN_COMMAND.txt");
    await toggle(panel(), "a_oracle_test.go");
    await waitFor(() => {
      expect(approve()).toBeEnabled();
    });
    await userEvent.click(approve());
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    await waitFor(() => {
      expect(oracle.server.sent("POST /requests/req-1/approve")).toHaveLength(1);
    });
    expect(oracle.server.sent("POST /requests/req-1/approve")[0]?.body).toEqual({
      by: "operator",
      expected_sha256: {
        "oracle/RUN_COMMAND.txt": sha256Hex("go test ./...\n"),
        "oracle/a_oracle_test.go": sha256Hex("package a\n"),
      },
    });
  });

  test("a refused approval shows the server message without the JSON envelope, and the panel re-lists the files", async () => {
    const { oracle } = openOracleReview(
      { "RUN_COMMAND.txt": "go test ./...\n" },
      {
        approve: apiErrorResponse(
          400,
          "oracle/RUN_COMMAND.txt artifact changed since it was fetched",
        ),
      },
    );
    await ready();
    await toggle(panel(), "RUN_COMMAND.txt");
    await waitFor(() => {
      expect(approve()).toBeEnabled();
    });
    await userEvent.click(approve());
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      /oracle\/RUN_COMMAND.txt artifact changed since it was fetched/,
    );
    expect(screen.queryByText(/Run API request failed/)).not.toBeInTheDocument();

    const before = oracle.server.sent("GET /requests/req-1/oracle").length;
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(oracle.server.sent("GET /requests/req-1/oracle").length).toBeGreaterThan(before);
    });
  });

  test("shows the drafting status, its detail and the proposed command, and keeps Request changes available", async () => {
    openOracleReview(
      { "RUN_COMMAND.txt": "go test ./...\n" },
      {
        draft: {
          status: "failed",
          detail: "model returned no manifest",
          proposed_command: "go test ./internal/a/...",
        },
      },
    );
    await ready();
    expect(
      screen.getByText("Oracle draft: Drafting failed -- model returned no manifest"),
    ).toBeInTheDocument();
    expect(screen.getByTestId("oracle-proposed-command")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Request changes" })).toBeInTheDocument();
  });

  test("Approve is the panel's alone: the page offers no second one", async () => {
    openOracleReview({ "RUN_COMMAND.txt": "go test ./...\n" });
    await ready();
    expect(screen.getAllByRole("button", { name: /^Approve/ })).toHaveLength(1);
  });
});

describe("oracle_drafting", () => {
  test("shows an in-progress notice, and the previous pass's failure when there was one", async () => {
    const body = requestWire({
      state: "oracle_drafting",
      title: "T",
      oracle_draft: { status: "failed", detail: "timeout" },
    });
    renderApp(<RequestDetailScreen />, {
      server: fakeServer([{ on: "GET /requests/req-1", reply: json(body) }]),
      path: "/requests/req-1",
      pattern: "/requests/:id",
    });
    await screen.findByRole("heading", { level: 1, name: "T" });

    expect(screen.getByTestId("oracle-drafting-section")).toBeInTheDocument();
    expect(screen.getByText(/Drafting acceptance-test oracles/)).toBeInTheDocument();
    expect(screen.getByText("Previous pass: Drafting failed -- timeout")).toBeInTheDocument();
    expect(screen.queryByTestId("oracle-review-panel")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Approve/ })).not.toBeInTheDocument();
  });
});

test("the request detail shows the server-recorded oracle skip warning after the approval", async () => {
  renderApp(<RequestDetailScreen />, {
    server: fakeServer([
      {
        on: "GET /requests/req-1",
        reply: json(
          requestWire({
            state: "planning",
            title: "T",
            oracle_draft: { status: "failed", detail: "boom" },
            oracle_skip_warning: "the oracle stage is being skipped: test",
          }),
        ),
      },
    ]),
    path: "/requests/req-1",
    pattern: "/requests/:id",
  });
  await screen.findByRole("heading", { level: 1, name: "T" });
  const warning = screen.getByTestId("oracle-skip-warning");
  expect(warning).toHaveTextContent("oracle stage is being skipped");
  expect(warning).toHaveTextContent("Warning: the oracle stage is being skipped: test (boom)");
});

describe("plan_review ticket oracle files", () => {
  const TICKET1 = "Verify-Command: true\n## Ticket one\n";
  const TICKET2 = "Verify-Command: true\n## Ticket two\n";
  const files: Record<number, Record<string, Uint8Array>> = {
    1: {
      "RUN_COMMAND.txt": enc("go test ./.oracle/...\n"),
      "a_oracle_test.go": enc("package a\n"),
    },
    2: { "RUN_COMMAND.txt": enc("go test ./.oracle/...\n") },
  };

  function openPlan(
    oracle: Record<number, Record<string, Uint8Array>>,
    options: { approve?: Response; listingStatus?: number } = {},
  ) {
    const routes: FakeRoute[] = [
      {
        on: "GET /requests/req-1",
        reply: () =>
          json(
            requestWire({
              state: "plan_review",
              title: "T",
              tickets: [
                ticketWire({
                  index: 1,
                  specPath: "/srv/data/requests/req-1/tickets/001.spec.md",
                  content: TICKET1,
                }),
                ticketWire({
                  index: 2,
                  specPath: "/srv/data/requests/req-1/tickets/002.spec.md",
                  content: TICKET2,
                }),
              ],
            }),
          ),
      },
      {
        on: "POST /requests/req-1/approve",
        reply: () => options.approve ?? json(requestWire({ state: "building", title: "T" })),
      },
    ];
    for (const n of [1, 2]) {
      routes.push({
        on: `GET /requests/req-1/tickets/${n}/oracle`,
        reply: () =>
          options.listingStatus !== undefined
            ? apiErrorResponse(options.listingStatus, "x")
            : json({
                files: Object.entries(oracle[n] ?? {}).map(([name, bytes]) => ({
                  name,
                  size: bytes.length,
                  sha256: sha256HexBytes(bytes),
                })),
                problems: [],
                state: "plan_review",
              }),
      });
      for (const [name, bytes] of Object.entries(oracle[n] ?? {})) {
        routes.push({
          on: `GET /requests/req-1/tickets/${n}/oracle/${encodeURIComponent(name)}`,
          reply: () => bytesResponse(bytes),
        });
      }
    }
    const server = fakeServer(routes);
    const view = renderApp(<RequestDetailScreen />, {
      server,
      path: "/requests/req-1",
      pattern: "/requests/:id",
    });
    return { ...view, server };
  }

  const approveButton = () => screen.getByRole("button", { name: "Approve" });
  const ticketPanel = () => screen.getByTestId("ticket-oracle-panel");

  async function ready() {
    await screen.findByRole("heading", { level: 1, name: "T" });
  }

  test("Approve waits until every ticket oracle file is open, then sends their hashes merged with the spec hashes", async () => {
    const { server } = openPlan(files);
    await ready();
    expect(await screen.findByText("Ticket oracle files")).toBeInTheDocument();
    expect(screen.getByText("Ticket 1 (tickets/001.oracle/)")).toBeInTheDocument();
    expect(screen.getByText("Ticket 2 (tickets/002.oracle/)")).toBeInTheDocument();
    expect(approveButton()).toBeDisabled();

    await toggle(ticketPanel(), "1/RUN_COMMAND.txt");
    await toggle(ticketPanel(), "1/a_oracle_test.go");
    expect(approveButton()).toBeDisabled();
    expect(screen.getByText("2 of 3 shown")).toBeInTheDocument();
    await toggle(ticketPanel(), "2/RUN_COMMAND.txt");
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });

    await userEvent.click(approveButton());
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/approve")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/approve")[0]?.body).toEqual({
      by: "operator",
      expected_sha256: {
        "tickets/001.spec.md": sha256Hex(TICKET1),
        "tickets/002.spec.md": sha256Hex(TICKET2),
        "tickets/001.oracle/RUN_COMMAND.txt": sha256HexBytes(enc("go test ./.oracle/...\n")),
        "tickets/001.oracle/a_oracle_test.go": sha256HexBytes(enc("package a\n")),
        "tickets/002.oracle/RUN_COMMAND.txt": sha256HexBytes(enc("go test ./.oracle/...\n")),
      },
    });
  });

  test("a collapsed ticket oracle file is not shown: Approve disables again", async () => {
    openPlan({ 1: files[1] ?? {} });
    await ready();
    await screen.findByText("Ticket oracle files");
    await toggle(ticketPanel(), "1/RUN_COMMAND.txt");
    await toggle(ticketPanel(), "1/a_oracle_test.go");
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });
    await toggle(ticketPanel(), "1/a_oracle_test.go");
    await waitFor(() => {
      expect(approveButton()).toBeDisabled();
    });
  });

  test("tickets with no oracle files approve immediately with just the spec hashes (no panel)", async () => {
    const { server } = openPlan({});
    await ready();
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });
    expect(screen.queryByText("Ticket oracle files")).not.toBeInTheDocument();
    await userEvent.click(approveButton());
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));
    await waitFor(() => {
      expect(server.sent("POST /requests/req-1/approve")).toHaveLength(1);
    });
    expect(server.sent("POST /requests/req-1/approve")[0]?.body).toEqual({
      by: "operator",
      expected_sha256: {
        "tickets/001.spec.md": sha256Hex(TICKET1),
        "tickets/002.spec.md": sha256Hex(TICKET2),
      },
    });
  });

  test("a server without the ticket oracle route (404) is treated as having no files", async () => {
    openPlan(files, { listingStatus: 404 });
    await ready();
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });
  });

  test("a failed ticket oracle listing keeps Approve disabled", async () => {
    openPlan(files, { listingStatus: 500 });
    await ready();
    expect(await screen.findByTestId("oracle-stale-listing")).toBeInTheDocument();
    expect(approveButton()).toBeDisabled();
  });

  test("a ticket oracle file changed after it was shown is refused by the server and the plain message is shown", async () => {
    openPlan(
      { 1: files[1] ?? {} },
      {
        approve: apiErrorResponse(
          409,
          "request req-1: tickets/001.oracle/a_oracle_test.go artifact changed since it was fetched",
        ),
      },
    );
    await ready();
    await screen.findByText("Ticket oracle files");
    await toggle(ticketPanel(), "1/RUN_COMMAND.txt");
    await toggle(ticketPanel(), "1/a_oracle_test.go");
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });
    await userEvent.click(approveButton());
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      /artifact changed since it was fetched/,
    );
    expect(screen.queryByText(/Run API request failed/)).not.toBeInTheDocument();

    // The panel starts over: nothing counts as shown until re-opened.
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    await waitFor(() => {
      expect(approveButton()).toBeDisabled();
    });
    await screen.findByText("Ticket 1 (tickets/001.oracle/)");
    await toggle(ticketPanel(), "1/RUN_COMMAND.txt");
    await toggle(ticketPanel(), "1/a_oracle_test.go");
    await waitFor(() => {
      expect(approveButton()).toBeEnabled();
    });
  });
});
