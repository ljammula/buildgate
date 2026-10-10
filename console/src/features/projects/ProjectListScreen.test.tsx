import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectListScreen } from "./ProjectListScreen";

const appProject = {
  project_path: "/workspaces/app",
  project: "app",
  workspace_path: "/workspaces/app",
  spec_path: "/specs/app-latest.md",
  repository: "app-repo",
  run_count: 3,
  last_run_at: "2026-08-26T12:00:00Z",
};

const appStats = {
  project: "app",
  total_runs: 4,
  accepted: 2,
  halted: 0,
  override_rate_percent: 50,
  accepted_via_override: 1,
  median_accepted_tokens: 478300,
  quarantined_by_cause: { canonical_verify: 1 },
};

const appRelease = (engaged: boolean) => ({
  project: "app",
  kill_switch: { project: "app", engaged, history: [] },
});

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
    "Project",
    "Workspace",
    "Runs",
    "Accepted",
    "Kill switch",
    "Last run",
    "Details",
  ]);
  expect(row).toHaveAttribute("title", "/home/kanna/workspaces/app");
  expect(screen.getByText("app")).toBeInTheDocument();
  // The count is a number under its heading; the stats were not served, so these read as unavailable.
  expect(screen.getByText("3")).toBeInTheDocument();

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

test("a closed row reads its stats and release for the list, and nothing from the other tabs", async () => {
  const { server } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      { on: "GET /projects", reply: () => json([appProject]) },
      { on: "GET /projects/app/stats", reply: () => json(appStats) },
      { on: "GET /projects/app/release", reply: () => json(appRelease(false)) },
    ],
  });

  await screen.findByText("2 / 4 · 50% via override");
  expect(server.sent("GET /projects/app/stats")).toHaveLength(1);
  expect(server.sent("GET /projects/app/release")).toHaveLength(1);
  expect(server.sent("GET /projects/app/observations")).toHaveLength(0);
  expect(server.sent("GET /projects/app/trend")).toHaveLength(0);
  expect(server.sent("GET /projects/app/memory")).toHaveLength(0);
});

test("a row shows its runs, accepted share and kill switch", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      { on: "GET /projects", reply: () => json([appProject]) },
      { on: "GET /projects/app/stats", reply: () => json(appStats) },
      { on: "GET /projects/app/release", reply: () => json(appRelease(true)) },
    ],
  });

  expect(await screen.findByText("2 / 4 · 50% via override")).toBeInTheDocument();
  expect(screen.getByText("Kill switch engaged")).toBeInTheDocument();
  // Causes and tokens are in the Stats tab, not repeated in the row.
  expect(screen.queryByText(/canonical_verify/)).not.toBeInTheDocument();
});

test("a failed release read is unknown, never clear, and a failed stats read stays on its row", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () =>
          json([appProject, { ...appProject, project_path: "/workspaces/ok", project: "ok" }]),
      },
      { on: "GET /projects/app/stats", reply: () => apiErrorResponse(403, "forbidden") },
      { on: "GET /projects/app/release", reply: () => apiErrorResponse(403, "forbidden") },
      {
        on: "GET /projects/ok/stats",
        reply: () =>
          json({
            ...appStats,
            project: "ok",
            total_runs: 9,
            accepted: 9,
            override_rate_percent: null,
          }),
      },
      {
        on: "GET /projects/ok/release",
        reply: () => json({ ...appRelease(false), project: "ok" }),
      },
    ],
  });

  expect(await screen.findByText("Kill switch unknown")).toBeInTheDocument();
  expect(screen.queryAllByText("Kill switch unknown")).toHaveLength(1);
  expect(screen.getByText("9 / 9")).toBeInTheDocument();
  expect(screen.getByText("Kill switch clear")).toBeInTheDocument();
  expect(screen.getByText("–")).toBeInTheDocument();
  // One callout, however many projects failed on the start-token routes.
  expect(screen.getAllByText(/could not be read \(401\/403\)/)).toHaveLength(1);
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
  expect(screen.getByRole("tab", { name: "Stats" })).toHaveAttribute("aria-selected", "true");
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

const urlServer = [
  { on: "GET /projects", reply: () => json([appProject]) },
  { on: "GET /projects/app/stats", reply: () => json(appStats) },
  { on: "GET /projects/app/release", reply: () => json(appRelease(true)) },
] as const;

test("the URL opens a project's row on the tab it names", async () => {
  const { server } = renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=app&tab=release",
    pattern: "/app/projects",
    server: urlServer,
  });

  expect(await screen.findByRole("tab", { name: "Release" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  expect(screen.getByRole("button", { name: "Hide details for /workspaces/app" })).toBeVisible();
  expect(server.sent("GET /projects/app/observations")).toHaveLength(0);
});

test("a project with no tab opens on Stats", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=app",
    pattern: "/app/projects",
    server: urlServer,
  });

  expect(await screen.findByRole("tab", { name: "Stats" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
});

test("an unknown tab is ignored", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=app&tab=ops",
    pattern: "/app/projects",
    server: urlServer,
  });
  expect(await screen.findByRole("tab", { name: "Stats" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
});

test("a project the list does not hold opens in a panel, and its Release tab reads its own kill switch", async () => {
  const { server } = renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=never-ran&tab=release",
    pattern: "/app/projects",
    server: [
      ...urlServer,
      {
        on: "GET /projects/never-ran/release",
        reply: () =>
          json({
            project: "never-ran",
            kill_switch: { project: "never-ran", engaged: true, history: [] },
          }),
      },
    ],
  });

  expect(await screen.findByRole("heading", { name: "never-ran" })).toBeInTheDocument();
  expect(screen.getByText("No runs recorded for this project.")).toBeInTheDocument();
  expect(screen.getByRole("tab", { name: "Release" })).toHaveAttribute("aria-selected", "true");
  // The row for app stays closed: the panel is for the id the URL names.
  expect(screen.getByRole("button", { name: "Show details for /workspaces/app" })).toBeVisible();
  const panelChip = await screen.findAllByText("Kill switch engaged");
  expect(panelChip.length).toBeGreaterThan(1);
  expect(server.sent("GET /projects/never-ran/release")).toHaveLength(1);
});

test("an unknown project's failed read shows its error in the panel", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=bad&tab=release",
    pattern: "/app/projects",
    server: [
      ...urlServer,
      { on: "GET /projects/bad/release", reply: () => apiErrorResponse(400, "invalid project id") },
    ],
  });
  expect(await screen.findByText(/invalid project id/)).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "bad" })).toBeInTheDocument();
});

test("Close clears the query", async () => {
  const { location } = renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=never-ran&tab=release",
    pattern: "/app/projects",
    server: urlServer,
  });
  await userEvent.click(await screen.findByRole("button", { name: "Close" }));
  expect(location()).toBe("/app/projects");
  expect(screen.queryByText("No runs recorded for this project.")).not.toBeInTheDocument();
});

test("the lookup form opens a project by id on its Release tab, and does nothing when empty", async () => {
  const { location } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: urlServer,
  });
  await screen.findByText("2 / 4 · 50% via override");

  await userEvent.click(screen.getByRole("button", { name: "Open" }));
  expect(location()).toBe("/app/projects");

  await userEvent.type(screen.getByLabelText("Open a project by id"), "never-ran{Enter}");
  expect(location()).toBe("/app/projects?project=never-ran&tab=release");
  expect(await screen.findByText("No runs recorded for this project.")).toBeInTheDocument();
});

test("two rows with one id open only the first", async () => {
  renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=app",
    pattern: "/app/projects",
    server: [
      {
        on: "GET /projects",
        reply: () => json([appProject, { ...appProject, project_path: "/workspaces/other" }]),
      },
      { on: "GET /projects/app/stats", reply: () => json(appStats) },
      { on: "GET /projects/app/release", reply: () => json(appRelease(false)) },
    ],
  });
  expect(
    await screen.findByRole("button", { name: "Hide details for /workspaces/app" }),
  ).toBeVisible();
  expect(screen.getByRole("button", { name: "Show details for /workspaces/other" })).toBeVisible();
  expect(screen.getAllByRole("tab", { name: "Stats" })).toHaveLength(1);
});

test("opening project B after project A never shows A's figures in B's panel", async () => {
  const other = { ...appProject, project_path: "/workspaces/b", project: "b" };
  renderApp(<ProjectListScreen />, {
    path: "/app/projects?project=app",
    pattern: "/app/projects",
    server: [
      { on: "GET /projects", reply: () => json([appProject, other]) },
      { on: "GET /projects/app/stats", reply: () => json(appStats) },
      { on: "GET /projects/app/release", reply: () => json(appRelease(false)) },
      { on: "GET /projects/b/stats", reply: () => apiErrorResponse(500, "b stats unreadable") },
      { on: "GET /projects/b/release", reply: () => json({ ...appRelease(false), project: "b" }) },
    ],
  });
  expect(await screen.findByText("Total runs")).toBeInTheDocument();
  expect(screen.getByText("4")).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Show details for /workspaces/b" }));

  expect(await screen.findByText(/b stats unreadable/)).toBeInTheDocument();
  expect(screen.queryByText("Total runs")).not.toBeInTheDocument();
});

test.each([
  ["a&b", "a%26b"],
  ["50%", "50%25"],
  ["my repo", "my%20repo"],
])("an id %j is read and written through the URL intact", async (id, encoded) => {
  const { location, server } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [
      { on: "GET /projects", reply: () => json([]) },
      {
        on: `GET /projects/${encoded}/release`,
        reply: () =>
          json({ project: id, kill_switch: { project: id, engaged: true, history: [] } }),
      },
    ],
  });
  await screen.findByText(/No projects yet/);
  await userEvent.type(screen.getByLabelText("Open a project by id"), `${id}{Enter}`);

  expect(await screen.findByRole("heading", { name: id })).toBeInTheDocument();
  expect(new URLSearchParams(location().split("?")[1]).get("project")).toBe(id);
  expect(await screen.findByText("Kill switch engaged")).toBeInTheDocument();
  expect(server.sent(`GET /projects/${encoded}/release`)).toHaveLength(1);
});

test("a release read that fails after one succeeded reads unknown, never clear", async () => {
  const { server, queryClient } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: urlServer.map((route) =>
      route.on === "GET /projects/app/release"
        ? { on: route.on, reply: () => json(appRelease(false)) }
        : route,
    ),
  });
  expect(await screen.findByText("Kill switch clear")).toBeInTheDocument();

  server.set("GET /projects/app/release", () => apiErrorResponse(500, "unreadable"));
  server.set("GET /projects/app/stats", () => apiErrorResponse(500, "unreadable"));
  void queryClient.refetchQueries();

  const chip = await screen.findByText("Kill switch unknown");
  expect(screen.queryByText("Kill switch clear")).not.toBeInTheDocument();
  expect(chip.closest("[title]")).toHaveAttribute(
    "title",
    "Last read failed; showing nothing rather than an old state",
  );
  // The stale figures are gone too, and the dash says why.
  expect(screen.queryByText("2 / 4 · 50% via override")).not.toBeInTheDocument();
  expect(screen.getByTitle("Stats could not be read")).toHaveTextContent("–");
});

test("opening a row, changing tab and closing it are written to the query string", async () => {
  const { location } = renderApp(<ProjectListScreen />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: urlServer,
  });

  await userEvent.click(
    await screen.findByRole("button", { name: "Show details for /workspaces/app" }),
  );
  expect(location()).toBe("/app/projects?project=app");

  await userEvent.click(screen.getByRole("tab", { name: "Observations" }));
  expect(location()).toBe("/app/projects?project=app&tab=observations");

  await userEvent.click(screen.getByRole("button", { name: "Hide details for /workspaces/app" }));
  expect(location()).toBe("/app/projects");
});
