import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { readFixtureJson } from "@/test/fixtures";
import { json, renderApp } from "@/test/render";

import { ProjectListScreen } from "./ProjectListScreen";

test("project list renders known projects and prefills a run", async () => {
  const { location } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () =>
          json([
            {
              project_path: "/home/kanna/workspaces/app",
              project: "app",
              workspace_path: "/workspaces/app",
              spec_path: "/specs/app-latest.md",
              repository: "app-repo",
              run_count: 3,
              last_run_at: "2026-08-26T12:00:00Z",
            },
          ]),
      },
    ],
  });

  const row = await screen.findByRole("link", { name: "…/workspaces/app" });
  // Each heading is true: the first column is a path, the second is the project id.
  expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
    "Workspace",
    "Project",
    "Runs",
    "Last run",
    "Details",
  ]);
  expect(row).toHaveAttribute("title", "/home/kanna/workspaces/app");
  expect(screen.getByText("app")).toBeInTheDocument();
  expect(screen.getByText("3 runs")).toBeInTheDocument();

  await userEvent.click(row);

  // The new-run form reads these three from its query string.
  expect(await screen.findByText(/Navigated to \/app\/runs\/new\?/)).toBeInTheDocument();
  const url = new URL(location(), "http://console.test");
  expect(url.pathname).toBe("/app/runs/new");
  expect(Object.fromEntries(url.searchParams)).toEqual({
    workspace: "/workspaces/app",
    spec: "/specs/app-latest.md",
    repository: "app-repo",
  });
});

test("empty project list still offers a custom run", async () => {
  const { location } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [{ on: "GET /projects", reply: () => json([]) }],
  });

  expect(await screen.findByText(/No projects yet/)).toBeInTheDocument();

  await userEvent.click(screen.getByRole("link", { name: "New run" }));

  expect(await screen.findByText("Navigated to /app/runs/new")).toBeInTheDocument();
  expect(location()).toBe("/app/runs/new");
});

test("collapsed project rows do not fetch stats or release data", async () => {
  const { server } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () =>
          json([
            {
              project_path: "/workspaces/app",
              project: "app",
              workspace_path: "/workspaces/app",
              spec_path: "/specs/app-latest.md",
              repository: "app-repo",
              run_count: 3,
              last_run_at: "2026-08-26T12:00:00Z",
            },
          ]),
      },
    ],
  });

  await screen.findByText("app");

  // Verify that stats and release endpoints were not called while row is collapsed.
  expect(server.sent("GET /projects/app/stats")).toHaveLength(0);
  expect(server.sent("GET /projects/app/release")).toHaveLength(0);
});

test("expanding a project row shows the stats tab", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () =>
          json([
            {
              project_path: "/workspaces/app",
              project: "app",
              workspace_path: "/workspaces/app",
              spec_path: "/specs/app-latest.md",
              repository: "app-repo",
              run_count: 3,
              last_run_at: "2026-08-26T12:00:00Z",
            },
          ]),
      },
      {
        on: "GET /projects/app/stats",
        reply: () =>
          json({
            project: "app",
            total_runs: 3,
            accepted: 2,
            halted: 0,
            override_rate_percent: 50,
            accepted_via_override: 1,
            median_accepted_tokens: 1000,
            quarantined_by_cause: {},
          }),
      },
    ],
  });

  await screen.findByText("app");

  const expandButton = screen.getByRole("button", { name: "Show details for /workspaces/app" });
  await userEvent.click(expandButton);

  // Verify that stats content is shown after expanding.
  expect(await screen.findByText("Total runs")).toBeInTheDocument();
  expect(screen.getByText("3")).toBeInTheDocument();
  expect(screen.getByText("Accepted")).toBeInTheDocument();
});

test("switching to release tab shows release content", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () =>
          json([
            {
              project_path: "/workspaces/app",
              project: "app",
              workspace_path: "/workspaces/app",
              spec_path: "/specs/app-latest.md",
              repository: "app-repo",
              run_count: 3,
              last_run_at: "2026-08-26T12:00:00Z",
            },
          ]),
      },
      {
        on: "GET /projects/app/release",
        reply: () =>
          json({
            project: "app",
            kill_switch: {
              project: "app",
              engaged: false,
              history: [
                {
                  engaged: true,
                  by: "operator@example.com",
                  at: "2026-08-25T10:00:00Z",
                  reason: "incident 42",
                },
                {
                  engaged: false,
                  by: "operator@example.com",
                  at: "2026-08-26T12:00:00Z",
                  reason: "resolved",
                },
              ],
            },
          }),
      },
    ],
  });

  await screen.findByText("app");

  const expandButton = screen.getByRole("button", { name: "Show details for /workspaces/app" });
  await userEvent.click(expandButton);

  const releaseTab = screen.getByRole("tab", { name: "Release" });
  await userEvent.click(releaseTab);

  // Verify that release content is shown after switching tabs.
  expect(await screen.findByText("State")).toBeInTheDocument();
  expect(screen.getByText("incident 42")).toBeInTheDocument();
});

test("the observations tab shows what the project's runs showed", async () => {
  const { server } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () =>
          json([
            {
              project_path: "/workspaces/app",
              project: "app",
              workspace_path: "/workspaces/app",
              spec_path: "/specs/app-latest.md",
              repository: "app-repo",
              run_count: 3,
              last_run_at: "2026-08-26T12:00:00Z",
            },
          ]),
      },
      {
        on: "GET /projects/app/observations",
        reply: () => json(readFixtureJson("api/project-observations.json")),
      },
    ],
  });

  await screen.findByText("app");
  await userEvent.click(screen.getByRole("button", { name: "Show details for /workspaces/app" }));
  // Nothing is read for a tab that is not the selected one.
  expect(server.sent("GET /projects/app/observations")).toHaveLength(0);

  await userEvent.click(screen.getByRole("tab", { name: "Observations" }));
  expect(
    await screen.findByText("Rounds 2 and 3 ended without changing any file."),
  ).toBeInTheDocument();
  expect(server.sent("GET /projects/app/observations")).toHaveLength(1);
});
