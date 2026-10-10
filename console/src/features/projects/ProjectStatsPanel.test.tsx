import { screen } from "@testing-library/react";

import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectStatsPanel } from "./ProjectStatsPanel";

function renderStats(project: string, reply: () => Response, startToken?: string) {
  return renderApp(<ProjectStatsPanel project={project} />, {
    server: [{ on: `GET /projects/${project}/stats`, reply }],
    ...(startToken === undefined ? {} : { tokens: { startToken } }),
  });
}

test("a project's acceptance-rate figures load with the start token", async () => {
  const { server } = renderStats(
    "checkouts",
    () =>
      json({
        project: "checkouts",
        total_runs: 5,
        accepted: 4,
        accepted_via_override: 1,
        override_rate_percent: 25,
        quarantined_by_cause: { canonical_verify: 1 },
        halted: 0,
        median_accepted_cost_micro_usd: 250000,
        median_accepted_tokens: 478300,
      }),
    "control-token",
  );

  expect(await screen.findByText("25% (1 of 4)")).toBeInTheDocument();
  const request = server.sent("GET /projects/checkouts/stats")[0];
  expect(request?.headers.Authorization).toBe("Bearer control-token");
  expect(screen.getByText("478.3k tokens")).toBeInTheDocument();
  expect(screen.getByText("canonical_verify")).toBeInTheDocument();
});

test("renders the median accepted tokens even when the server also carries dollar fields", async () => {
  renderStats("checkouts", () =>
    json({
      project: "checkouts",
      total_runs: 2,
      accepted: 2,
      accepted_via_override: 0,
      override_rate_percent: 0,
      halted: 0,
      median_accepted_cost_micro_usd: 250000,
      median_accepted_cost_subscription_billed: true,
      median_accepted_tokens: 12300,
    }),
  );
  expect(await screen.findByText("12.3k tokens")).toBeInTheDocument();
  expect(screen.queryByText(/\$/)).not.toBeInTheDocument();
});

test("a project with no accepted runs shows no data, not 0%/$0", async () => {
  renderStats("brand-new", () =>
    json({
      project: "brand-new",
      total_runs: 1,
      accepted: 0,
      accepted_via_override: 0,
      halted: 0,
    }),
  );
  expect(await screen.findAllByText("No accepted runs yet")).toHaveLength(2);
  expect(screen.queryByText(/%/)).not.toBeInTheDocument();
});

test("a failed lookup is reported, not shown as a stale success", async () => {
  renderStats("unknown-project", () => apiErrorResponse(404, "not found"));
  expect(await screen.findByRole("alert")).toHaveTextContent("Not found");
  expect(screen.queryByText("Override rate (accepted)")).not.toBeInTheDocument();
});

test("a failed refresh keeps the last figures under a warning, and Retry is offered", async () => {
  let calls = 0;
  const { server, queryClient } = renderStats("checkouts", () => {
    calls += 1;
    return json({
      project: "checkouts",
      total_runs: 3,
      accepted: 0,
      accepted_via_override: 0,
      halted: 1,
    });
  });
  expect(await screen.findByText("Override rate (accepted)")).toBeInTheDocument();

  server.set("GET /projects/checkouts/stats", () => apiErrorResponse(500, "stats unreadable"));
  void queryClient.refetchQueries();

  expect(await screen.findByText(/refresh failed: stats unreadable/)).toBeInTheDocument();
  expect(screen.getByText("Override rate (accepted)")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
  expect(calls).toBe(1);
});

test("blocks with nothing in them are absent: no Halted row at zero, no empty quarantine section", async () => {
  renderStats("checkouts", () =>
    json({
      project: "checkouts",
      total_runs: 2,
      accepted: 2,
      accepted_via_override: 0,
      override_rate_percent: 0,
      halted: 0,
      median_accepted_tokens: 12300,
    }),
  );
  await screen.findByText("Override rate (accepted)");

  expect(screen.queryByText("Halted")).not.toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Quarantined by cause" })).not.toBeInTheDocument();
  expect(screen.queryByText("No quarantined runs recorded.")).not.toBeInTheDocument();
});

test("a halted run and a quarantine cause each bring their block back", async () => {
  renderStats("checkouts", () =>
    json({
      project: "checkouts",
      total_runs: 3,
      accepted: 1,
      accepted_via_override: 0,
      halted: 2,
      quarantined_by_cause: { canonical_verify: 1 },
    }),
  );

  expect(await screen.findByText("Halted")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Quarantined by cause" })).toBeInTheDocument();
});

test("the facts sit in a card", async () => {
  renderStats("checkouts", () =>
    json({
      project: "checkouts",
      total_runs: 5,
      accepted: 4,
      accepted_via_override: 1,
      override_rate_percent: 25,
      quarantined_by_cause: {},
      halted: 0,
      median_accepted_tokens: null,
    }),
  );
  const facts = (await screen.findByText("Total runs")).closest("dl");
  expect(facts?.parentElement).toHaveClass("border", "rounded-lg");
  // The panel sits in its project's own row: there is no other project to load.
  expect(screen.queryByLabelText("Project id")).not.toBeInTheDocument();
});
