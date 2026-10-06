import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

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

  const row = await screen.findByRole("link", { name: "/workspaces/app" });
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

  await userEvent.click(screen.getByRole("link", { name: "Custom run" }));

  expect(await screen.findByText("Navigated to /app/runs/new")).toBeInTheDocument();
  expect(location()).toBe("/app/runs/new");
});
