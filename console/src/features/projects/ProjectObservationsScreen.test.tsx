import { screen, within } from "@testing-library/react";

import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectObservationsScreen } from "./ProjectObservationsScreen";

function renderObservations(project: string, reply: () => Response) {
  return renderApp(<ProjectObservationsScreen />, {
    path: `/projects/${project}/observations`,
    pattern: "/projects/:project/observations",
    server: [{ on: `GET /projects/${project}/observations`, reply }],
  });
}

test("a project's observations list what its runs showed, with the output as text", async () => {
  renderObservations("app", () => json(readFixtureJson("api/project-observations.json")));

  expect(
    await screen.findByText(
      "Rounds 2 and 3 failed the same way (no changes made to the workspace; canonical verification failed).",
    ),
  ).toBeInTheDocument();
  expect(screen.getByText("Rounds 2 and 3 ended without changing any file.")).toBeInTheDocument();
  expect(screen.getAllByTestId("observation")).toHaveLength(4);
  // The counts: one row per kind that happened.
  expect(screen.getByText("Finished runs read")).toBeInTheDocument();
  expect(screen.getAllByText("Same failure twice running").length).toBeGreaterThan(0);
  expect(screen.queryByText("The run halted")).not.toBeInTheDocument();

  const first = screen.getAllByTestId("observation")[0]!;
  expect(within(first).getByRole("link", { name: "run-quarantined" })).toHaveAttribute(
    "href",
    "/runs/run-quarantined",
  );
  const output = within(first).getByRole("region", {
    name: "Output of run run-quarantined: round-logs/round-3/verify.log",
  });
  expect(output).toHaveTextContent("idempotency_test.go:41: key reused across accounts");
});

test("agent-reported text is rendered as text, never as markup", async () => {
  renderObservations("app", () =>
    json({
      project: "app",
      runs: 1,
      accepted_first_round: 0,
      counts: { fixed_after_failure: 1 },
      observations: [
        {
          kind: "fixed_after_failure",
          run_id: "r1",
          ticket: "t",
          at: "2026-10-08T10:00:00Z",
          what: "Round 1 failed (<img src=x onerror=alert(1)>); round 2 passed after changing a.go.",
          log: "round-logs/round-1/verify.log",
          excerpt: "<script>alert(1)</script> FAIL",
        },
      ],
    }),
  );
  const item = await screen.findByTestId("observation");
  expect(item).toHaveTextContent("<img src=x onerror=alert(1)>");
  expect(item.querySelector("img, script")).toBeNull();
});

test("a project with nothing to report says why", async () => {
  renderObservations("quiet", () =>
    json({ project: "quiet", runs: 4, accepted_first_round: 4, counts: {}, observations: [] }),
  );
  expect(
    await screen.findByText(
      "No finished run of this project recorded a failed round, a failed check or a halt.",
    ),
  ).toBeInTheDocument();
});

test("a project with no finished run says so, and a longer history says it was cut", async () => {
  renderObservations("empty", () =>
    json({ project: "empty", runs: 0, accepted_first_round: 0, counts: {}, observations: [] }),
  );
  expect(
    await screen.findByText("No finished run of this project is in this data directory."),
  ).toBeInTheDocument();
});

test("a cut list says the counts include the rest", async () => {
  renderObservations("busy", () =>
    json({
      project: "busy",
      runs: 300,
      accepted_first_round: 0,
      counts: { run_halted: 300 },
      truncated: true,
      observations: [
        { kind: "run_halted", run_id: "h1", what: "The run halted: context canceled." },
      ],
    }),
  );
  expect(
    await screen.findByText("Showing the 1 newest; the counts above include the rest."),
  ).toBeInTheDocument();
});

test("a failed load shows the server's error", async () => {
  renderObservations("app", () => apiErrorResponse(403, "read endpoint is not authorized"));
  expect(await screen.findByText(/read endpoint is not authorized/)).toBeInTheDocument();
});
