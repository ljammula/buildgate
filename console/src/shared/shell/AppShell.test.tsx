import { screen, waitFor, within } from "@testing-library/react";

import { decodeRequestSummary } from "@/domain/request";
import { needsYouRequests } from "@/shared/request/needsYou";
import { AppShell } from "@/shared/shell/AppShell";
import { requestJson } from "@/test/requestFixtures";
import { type FakeRoute, json, renderApp } from "@/test/render";

function shell(list: Parameters<typeof requestJson>[0][]): FakeRoute[] {
  return [{ on: "GET /requests", reply: () => json(list.map(requestJson)) }];
}

test("the sidebar counts the requests that wait on the operator, on Triage only", async () => {
  const { server } = renderApp(
    <AppShell>
      <p>page</p>
    </AppShell>,
    {
      server: shell([
        { id: "a", state: "spec_review" },
        { id: "b", state: "plan_review" },
        { id: "c", state: "building" },
        { id: "d", state: "done" },
      ]),
    },
  );
  await waitFor(() => {
    expect(screen.getAllByTestId("nav-needs-you-count")).toHaveLength(1);
  });
  expect(screen.getByTestId("nav-needs-you-count")).toHaveTextContent("2");
  // The count sits on Triage, the screen for deciding, and not on Mission Control.
  expect(
    within(screen.getByRole("link", { name: "Triage" })).getByTestId("nav-needs-you-count"),
  ).toBeInTheDocument();
  expect(
    within(screen.getByRole("link", { name: "Mission Control" })).queryByTestId(
      "nav-needs-you-count",
    ),
  ).not.toBeInTheDocument();
  // The pill is for the eye; the links keep their names and the count is announced once.
  expect(screen.getByRole("link", { name: "Mission Control" })).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Triage" })).toBeInTheDocument();
  expect(screen.getByRole("status")).toHaveTextContent("2 requests need you.");
  // No event stream is opened just to count.
  expect(server.sent("GET /requests/events")).toHaveLength(0);
});

test("no pill when nothing waits, and none when the list cannot be read", async () => {
  const idle = renderApp(<AppShell>x</AppShell>, {
    server: shell([{ id: "c", state: "building" }]),
  });
  await waitFor(() => {
    expect(screen.getByRole("status")).toHaveTextContent("Nothing is waiting on you.");
  });
  expect(screen.queryByTestId("nav-needs-you-count")).not.toBeInTheDocument();
  idle.unmount();

  renderApp(<AppShell>x</AppShell>, { server: [] });
  await waitFor(() => {
    expect(screen.getByRole("link", { name: "Mission Control" })).toBeInTheDocument();
  });
  expect(screen.queryByTestId("nav-needs-you-count")).not.toBeInTheDocument();
  expect(screen.getByRole("status")).toHaveTextContent("");
});

test("the Triage count is the number of rows Triage lists: every state that needs the operator", async () => {
  const list = [
    { id: "a", state: "spec_review" },
    { id: "b", state: "oracle_review" },
    { id: "c", state: "halted" },
    { id: "d", state: "quarantined" },
    { id: "e", state: "resume_review" },
    { id: "f", state: "building" },
    { id: "g", state: "done" },
  ];
  renderApp(<AppShell>x</AppShell>, { server: shell(list) });
  await waitFor(() => {
    expect(screen.getAllByTestId("nav-needs-you-count")).toHaveLength(1);
  });
  expect(screen.getByTestId("nav-needs-you-count")).toHaveTextContent("5");
  // The same function lists the rows: it returns exactly those five.
  const summaries = list.map((o) => decodeRequestSummary(requestJson(o), "test"));
  expect(
    needsYouRequests(summaries)
      .map((r) => r.id)
      .sort(),
  ).toEqual(["a", "b", "c", "d", "e"]);
});

test("Projects is the current item on the Projects screen", async () => {
  renderApp(<AppShell>x</AppShell>, {
    server: shell([]),
    path: "/app/projects?project=app&tab=release",
    pattern: "/app/projects",
  });
  const projects = await screen.findByRole("link", { name: "Projects" });
  expect(projects).toHaveAttribute("aria-current", "page");
  expect(screen.getByRole("link", { name: "Runs" })).not.toHaveAttribute("aria-current");
  expect(screen.getByRole("link", { name: "Mission Control" })).not.toHaveAttribute("aria-current");
});
