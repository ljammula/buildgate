// The commands to look at the work, on the request page: where they show and
// what they say.
import { screen, within } from "@testing-library/react";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const builtInspect = {
  tickets: [
    {
      ticket: 1,
      run_id: "run-1",
      worktree: "~/buildgate/data/workspaces/req-1-abc",
      base: "1111111",
      result: "2222222",
      commands: [
        { label: "The files changed", command: "git -C ~/code/app diff --stat 1111111..2222222" },
        {
          label: "Your own copy, to try or change",
          command: "git -C ~/code/app worktree add ~/review/run-1 2222222",
        },
      ],
    },
  ],
  note: "These commands only read. To try or change the work, use your own copy.",
};

test("a quarantined request shows each ticket's commands, its worktree and the note", async () => {
  openRequest(
    requestWire({
      state: "quarantined",
      tickets: [ticketWire({ index: 1, runId: "run-1" })],
      inspect: builtInspect,
    }),
  );
  const panel = await screen.findByRole("region", { name: "Commands to look at the work" });
  expect(
    within(panel).getByText("Run these in a shell on the machine that runs the factory."),
  ).toBeInTheDocument();
  const ticket = within(panel).getByRole("region", { name: "Ticket 001" });
  expect(within(ticket).getByText("~/buildgate/data/workspaces/req-1-abc")).toBeInTheDocument();
  expect(
    within(ticket).getByText("git -C ~/code/app diff --stat 1111111..2222222"),
  ).toBeInTheDocument();
  expect(within(ticket).getAllByRole("button", { name: "Copy command" })).toHaveLength(2);
  expect(within(panel).getByText(/use your own copy/)).toBeInTheDocument();
});

test("a spec review shows the commands that open the drafted files", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      inspect: {
        commands: [{ label: "The spec", command: "less ~/buildgate/data/requests/req-1/spec.md" }],
        note: "Nothing is built yet.",
      },
    }),
  );
  const panel = await screen.findByRole("region", { name: "Commands to look at the work" });
  expect(
    within(panel).getByText("less ~/buildgate/data/requests/req-1/spec.md"),
  ).toBeInTheDocument();
  expect(within(panel).queryByRole("region", { name: /Ticket/ })).toBeNull();
});

test("no panel when the server sends no commands", async () => {
  openRequest(requestWire({ state: "building" }));
  await screen.findByRole("heading", { level: 1 });
  expect(screen.queryByRole("region", { name: "Commands to look at the work" })).toBeNull();
});
