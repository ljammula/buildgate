import { screen } from "@testing-library/react";

import { apiErrorResponse, json, renderApp } from "@/test/render";

import { OpsScreen } from "./OpsScreen";

const project = {
  project_path: "/repo/checkouts",
  project: "checkouts",
  workspace_path: "/repo/checkouts",
  spec_path: "/repo/spec/spec.md",
  run_count: 5,
  last_run_at: "2026-09-08T10:00:00Z",
};

describe("OpsScreen", () => {
  // Proves the ops view aggregates every project's own stats and kill-switch
  // state on one screen, from GET /projects plus a stats and a release call
  // per project.
  it("lists every project with its quarantine breakdown and kill-switch state", async () => {
    renderApp(<OpsScreen />, {
      server: [
        { on: "GET /projects", reply: json([project]) },
        {
          on: "GET /projects/checkouts/stats",
          reply: json({
            project: "checkouts",
            total_runs: 5,
            accepted: 4,
            accepted_via_override: 1,
            override_rate_percent: 25,
            quarantined_by_cause: { canonical_verify: 1 },
            halted: 0,
            median_accepted_cost_micro_usd: 250000,
            median_accepted_cost_subscription_billed: true,
            median_accepted_tokens: 478300,
          }),
        },
        {
          on: "GET /projects/checkouts/release",
          reply: json({
            project: "checkouts",
            kill_switch: { project: "checkouts", engaged: true, history: [] },
          }),
        },
      ],
    });

    expect(await screen.findByText("checkouts")).toBeInTheDocument();
    expect(await screen.findByText(/Accepted 4 \/ 5 runs/)).toBeInTheDocument();
    expect(screen.getByText(/canonical_verify \(1\)/)).toBeInTheDocument();
    expect(screen.getByText(/Median accepted: 478.3k tokens/)).toBeInTheDocument();
    expect(screen.getByText("Kill switch engaged")).toBeInTheDocument();
  });

  it("a project whose stats/release calls fail still renders, marked unavailable", async () => {
    renderApp(<OpsScreen />, {
      server: [
        { on: "GET /projects", reply: json([{ ...project, project: "broken" }]) },
        { on: "GET /projects/broken/stats", reply: apiErrorResponse(403, "forbidden") },
        { on: "GET /projects/broken/release", reply: apiErrorResponse(403, "forbidden") },
      ],
    });

    expect(await screen.findByText("broken")).toBeInTheDocument();
    expect(screen.getByText("Stats unavailable for this project.")).toBeInTheDocument();
    // A failed release fetch must read as unknown, never as clear.
    expect(screen.getByText("Kill switch unknown")).toBeInTheDocument();
    expect(screen.queryByText("Kill switch clear")).not.toBeInTheDocument();
    expect(
      screen.getByText(/One or more projects' stats\/release could not be read/),
    ).toBeVisible();
  });

  it("no projects recorded yet is reported plainly", async () => {
    renderApp(<OpsScreen />, { server: [{ on: "GET /projects", reply: json([]) }] });

    expect(await screen.findByText("No projects recorded yet.")).toBeInTheDocument();
  });
});
