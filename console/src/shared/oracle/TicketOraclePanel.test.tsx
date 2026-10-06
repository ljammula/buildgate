import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { sha256HexBytes } from "@/domain/contentHash";
import { bytesResponse, enc, requestSummary, toggle } from "@/shared/oracle/oracleTestKit";
import { type TicketOracleShown, TicketOraclePanel } from "@/shared/oracle/TicketOraclePanel";
import { type FakeRoute, apiErrorResponse, fakeServer, json, renderApp } from "@/test/render";

type Files = Record<number, Record<string, Uint8Array>>;

const files: Files = {
  1: { "RUN_COMMAND.txt": enc("go test ./.oracle/...\n"), "a_oracle_test.go": enc("package a\n") },
  2: { "RUN_COMMAND.txt": enc("go test ./.oracle/...\n") },
};

interface PlanServer {
  oracle: Files;
  status: number | null;
  problems: Record<number, string[]>;
}

function planServer(oracle: Files) {
  const state: PlanServer = { oracle, status: null, problems: {} };
  const routes: FakeRoute[] = [];
  for (const n of [1, 2]) {
    routes.push({
      on: `GET /requests/req-1/tickets/${n}/oracle`,
      reply: () =>
        state.status !== null
          ? apiErrorResponse(state.status, "x")
          : json({
              files: Object.entries(state.oracle[n] ?? {}).map(([name, bytes]) => ({
                name,
                size: bytes.length,
                sha256: sha256HexBytes(bytes),
              })),
              problems: state.problems[n] ?? [],
              state: "plan_review",
            }),
    });
    for (const name of new Set(Object.values(oracle).flatMap((f) => Object.keys(f)))) {
      routes.push({
        on: `GET /requests/req-1/tickets/${n}/oracle/${encodeURIComponent(name)}`,
        reply: () => bytesResponse(state.oracle[n]?.[name]),
      });
    }
  }
  return { state, server: fakeServer(routes) };
}

const request = requestSummary({ state: "plan_review" }, [
  { index: 1, specPath: "/srv/data/requests/req-1/tickets/001.spec.md" },
  { index: 2, specPath: "/srv/data/requests/req-1/tickets/002.spec.md" },
]);

// What the plan's Approve does with the panel's report.
function Host({ onApprove }: { onApprove: (hashes: Record<string, string>) => void }) {
  const [shown, setShown] = useState<TicketOracleShown>({
    hashes: {},
    complete: false,
    remaining: null,
  });
  return (
    <>
      <TicketOraclePanel request={request} onChanged={setShown} />
      <button
        type="button"
        disabled={!shown.complete}
        onClick={() => {
          onApprove({ ...shown.hashes });
        }}
      >
        Approve
      </button>
    </>
  );
}

function setup(oracle: Files) {
  const plan = planServer(oracle);
  const onApprove = vi.fn();
  const view = renderApp(<Host onApprove={onApprove} />, { server: plan.server });
  return { ...plan, onApprove, ...view };
}

const approve = () => screen.getByRole("button", { name: "Approve" });
const container = () => document.body;
const contentOf = (keyId: string) =>
  screen.getByTestId(`oracle-content-${keyId}`).querySelector("pre")?.textContent;

test("groups each ticket's oracle files, keeps Approve disabled until every one is open, then reports their hashes", async () => {
  const { onApprove } = setup(files);

  expect(await screen.findByText("Ticket oracle files")).toBeInTheDocument();
  expect(screen.getByText("Ticket 1 (tickets/001.oracle/)")).toBeInTheDocument();
  expect(screen.getByText("Ticket 2 (tickets/002.oracle/)")).toBeInTheDocument();
  expect(approve()).toBeDisabled();

  await toggle(container(), "1/RUN_COMMAND.txt");
  await toggle(container(), "1/a_oracle_test.go");
  await screen.findByText("2 of 3 shown");
  expect(approve()).toBeDisabled();
  await toggle(container(), "2/RUN_COMMAND.txt");
  await waitFor(() => {
    expect(approve()).toBeEnabled();
  });

  await userEvent.click(approve());
  // The screen merges these with the spec hashes (tickets/NNN.spec.md).
  expect(onApprove).toHaveBeenCalledWith({
    "tickets/001.oracle/RUN_COMMAND.txt": sha256HexBytes(
      files[1]?.["RUN_COMMAND.txt"] ?? new Uint8Array(),
    ),
    "tickets/001.oracle/a_oracle_test.go": sha256HexBytes(enc("package a\n")),
    "tickets/002.oracle/RUN_COMMAND.txt": sha256HexBytes(enc("go test ./.oracle/...\n")),
  });
});

test("a collapsed ticket oracle file is not shown: Approve disables again", async () => {
  setup({ 1: files[1] ?? {} });
  await screen.findByText("Ticket oracle files");
  await toggle(container(), "1/RUN_COMMAND.txt");
  await toggle(container(), "1/a_oracle_test.go");
  await waitFor(() => {
    expect(approve()).toBeEnabled();
  });
  await toggle(container(), "1/a_oracle_test.go");
  await waitFor(() => {
    expect(approve()).toBeDisabled();
  });
});

test("tickets with no oracle files approve immediately with just the spec hashes (no panel)", async () => {
  const { onApprove } = setup({});
  await waitFor(() => {
    expect(approve()).toBeEnabled();
  });
  expect(screen.queryByText("Ticket oracle files")).toBeNull();
  await userEvent.click(approve());
  // The screen adds the spec hashes; the panel contributes none.
  expect(onApprove).toHaveBeenCalledWith({});
});

test("a server without the ticket oracle route (404) is treated as having no files", async () => {
  const plan = planServer(files);
  plan.state.status = 404;
  const onApprove = vi.fn();
  renderApp(<Host onApprove={onApprove} />, { server: plan.server });
  await waitFor(() => {
    expect(approve()).toBeEnabled();
  });
  expect(screen.queryByText("Ticket oracle files")).toBeNull();
});

test("a failed ticket oracle listing shows an error, keeps Approve disabled, and Reload recovers", async () => {
  const plan = planServer(files);
  plan.state.status = 500;
  renderApp(<Host onApprove={vi.fn()} />, { server: plan.server });
  expect(await screen.findByTestId("oracle-stale-listing")).toBeInTheDocument();
  expect(approve()).toBeDisabled();

  plan.state.status = null;
  await userEvent.click(screen.getByRole("button", { name: "Reload oracle files" }));
  await waitFor(() => {
    expect(screen.queryByTestId("oracle-stale-listing")).toBeNull();
  });
  expect(screen.getByText("Ticket 1 (tickets/001.oracle/)")).toBeInTheDocument();
  expect(approve()).toBeDisabled();
});

test("an existing but empty ticket oracle directory shows the approval-blocking problem and keeps Approve disabled", async () => {
  const plan = planServer({});
  plan.state.problems = {
    1: ["no RUN_COMMAND.txt -- add it, or remove the directory to skip"],
  };
  renderApp(<Host onApprove={vi.fn()} />, { server: plan.server });

  expect(await screen.findByTestId("oracle-problems")).toBeInTheDocument();
  expect(screen.getByText(/no RUN_COMMAND.txt/)).toBeInTheDocument();
  expect(approve()).toBeDisabled();
});

test("ticket oracle content shows hidden characters as escapes", async () => {
  setup({
    1: {
      "RUN_COMMAND.txt": enc("go test‮ ./...\n"),
      "b_oracle_test.go": new Uint8Array([0x70, 0x80]),
    },
  });
  await screen.findByText("Ticket oracle files");
  await toggle(container(), "1/RUN_COMMAND.txt");
  await toggle(container(), "1/b_oracle_test.go");
  await waitFor(() => {
    expect(contentOf("1/RUN_COMMAND.txt")).toBe("go test\\u{202E} ./...\n");
  });
  await waitFor(() => {
    expect(contentOf("1/b_oracle_test.go")).toBe("p\\x80");
  });
});
