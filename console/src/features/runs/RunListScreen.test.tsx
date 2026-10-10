import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { apiErrorResponse, json, renderApp } from "@/test/render";

import { RunListScreen } from "./RunListScreen";

function runJson(overrides: Record<string, unknown> = {}) {
  return {
    id: "run-accepted",
    ticket: "ticket-accepted",
    project_path: "/projects/app",
    workspace_path: "/workspaces/run-accepted",
    spec_path: "/specs/ticket-accepted.md",
    spec_sha256: "spec-accepted",
    state: "accepted",
    base_sha: "base-accepted",
    committed_by_factoryd: true,
    halt_confirmed: true,
    created_at: "2026-08-26T11:00:00Z",
    updated_at: "2026-08-26T11:04:00Z",
    ...overrides,
  };
}

const inProgress = runJson({
  id: "run-progress",
  ticket: "ticket-progress",
  state: "slice_running",
  halt_confirmed: false,
  created_at: "2026-08-26T12:00:00Z",
  updated_at: "2026-08-26T12:01:00Z",
  stalled: true,
  stalled_since_seconds: 999999,
});

/** A fresh, non-terminal run: now-relative so "not stalled" never rots. */
function waitingRun(extra: Record<string, unknown> = {}) {
  const now = Date.now();
  return runJson({
    id: "run-waiting",
    ticket: "ticket-waiting",
    state: "slice_running",
    halt_confirmed: false,
    created_at: new Date(now - 60_000).toISOString(),
    updated_at: new Date(now - 60_000).toISOString(),
    last_progress_at: new Date(now).toISOString(),
    current_stage: "build",
    ...extra,
  });
}

const noRequests = { on: "GET /requests", reply: () => json([]) };

test("run list row shows a stalled chip for a quiet run", async () => {
  renderApp(<RunListScreen />, {
    server: [{ on: "GET /runs", reply: () => json([inProgress]) }, noRequests],
  });
  expect(await screen.findByTestId("stalled-chip")).toBeInTheDocument();
});

test("run list row shows a waiting chip for a queued run", async () => {
  renderApp(<RunListScreen />, {
    server: [
      {
        on: "GET /runs",
        reply: () => json([waitingRun({ waiting_reason: "behind 1 run(s) on foo/bar" })]),
      },
      noRequests,
    ],
  });
  expect(await screen.findByTestId("waiting-chip")).toBeInTheDocument();
  expect(screen.queryByTestId("stalled-chip")).not.toBeInTheDocument();
  expect(screen.getByText(/Waiting: behind 1 run\(s\)/)).toBeInTheDocument();
});

test("run list row shows current stage and last activity", async () => {
  renderApp(<RunListScreen />, {
    server: [{ on: "GET /runs", reply: () => json([waitingRun()]) }, noRequests],
  });
  expect(await screen.findByText(/Build · last activity/)).toBeInTheDocument();
  expect(screen.queryByTestId("stalled-chip")).not.toBeInTheDocument();
  expect(screen.queryByTestId("waiting-chip")).not.toBeInTheDocument();
});

test("run list renders terminal and in-progress states", async () => {
  renderApp(<RunListScreen />, {
    server: [
      {
        on: "GET /runs",
        reply: () =>
          json([
            runJson(),
            runJson({ id: "run-halted", ticket: "ticket-halted", state: "halted" }),
            runJson({ id: "run-q", ticket: "ticket-q", state: "quarantined" }),
            inProgress,
          ]),
      },
      noRequests,
    ],
  });
  expect(await screen.findByText("ticket-accepted")).toBeInTheDocument();
  // The shared vocabulary (domain/status) names slice_running "Building".
  const tones: [string, string][] = [
    ["Accepted", "success"],
    ["Halted", "warning"],
    ["Quarantined", "danger"],
    ["Building", "info"],
  ];
  for (const [label, tone] of tones) {
    expect(screen.getByText(label)).toHaveAttribute("data-tone", tone);
  }
});

test("the toolbar has New run and Refresh, and no project lookups or Ops link", async () => {
  renderApp(<RunListScreen />, {
    server: [{ on: "GET /runs", reply: () => json([]) }, noRequests],
    path: "/runs",
    pattern: "/runs",
  });
  await screen.findByText("No runs found.");
  const banner = within(screen.getByRole("banner"));
  const controls = [...banner.queryAllByRole("link"), ...banner.queryAllByRole("button")].map(
    (el) => el.getAttribute("aria-label") ?? el.textContent,
  );
  expect(controls.sort()).toEqual(["New run", "Refresh"]);
});

test("run list row shows project, elapsed time, and run id", async () => {
  renderApp(<RunListScreen />, {
    server: [{ on: "GET /runs", reply: () => json([runJson()]) }, noRequests],
  });
  expect(await screen.findByText("/projects/app")).toBeInTheDocument();
  // A terminal run's elapsed is its fixed created-to-updated span, not "now".
  expect(screen.getByText("04:00")).toBeInTheDocument();
  expect(screen.getByText("run-accepted")).toBeInTheDocument();
});

test("a refresh failure surfaces a stale-data warning, not silence", async () => {
  let runsFetches = 0;
  renderApp(<RunListScreen />, {
    server: [
      {
        on: "GET /runs",
        reply: () => {
          runsFetches += 1;
          return runsFetches === 1
            ? json([runJson()])
            : apiErrorResponse(500, "factory is unreachable");
        },
      },
      noRequests,
    ],
  });
  expect(await screen.findByText("ticket-accepted")).toBeInTheDocument();
  expect(screen.queryByTestId("run-list-stale-banner")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));

  await waitFor(() => {
    expect(runsFetches).toBe(2);
    expect(screen.getByTestId("run-list-stale-banner")).toBeInTheDocument();
  });
  // A failed refresh never blanks data that was already loaded.
  expect(screen.getByText("ticket-accepted")).toBeInTheDocument();
  expect(screen.getByText(/refresh failed/)).toBeInTheDocument();
});

test("run list row shows the request title when requestId resolves", async () => {
  renderApp(<RunListScreen />, {
    server: [
      { on: "GET /runs", reply: () => json([runJson({ request_id: "request-1" })]) },
      {
        on: "GET /requests",
        reply: () =>
          json([
            {
              id: "request-1",
              workspace: "/workspaces/request-1",
              project: "app",
              state: "done",
              submitted_at: "2026-08-26T10:00:00Z",
              updated_at: "2026-08-26T11:04:00Z",
              title: "Add the widget",
            },
          ]),
      },
    ],
  });
  expect(await screen.findByText("Add the widget")).toBeInTheDocument();
  expect(screen.getByText("Ticket ticket-accepted")).toBeInTheDocument();
});

test("the Created column reads a relative age, with the exact local time on hover", async () => {
  const createdAt = new Date(Date.now() - 2 * 60 * 60_000 - 5 * 60_000).toISOString();
  renderApp(<RunListScreen />, {
    server: [
      { on: "GET /runs", reply: () => json([runJson({ created_at: createdAt })]) },
      noRequests,
    ],
  });

  const when = await screen.findByText("2h 05m ago");
  expect(when.tagName).toBe("TIME");
  expect(when).toHaveAttribute("dateTime", createdAt);
  expect(when.getAttribute("title")).toMatch(/^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d [+-]\d\d:\d\d$/);
});

test("columns read Run, Run ID, State, Activity, Elapsed, Created, Project, with Elapsed right-aligned", async () => {
  renderApp(<RunListScreen />, {
    server: [{ on: "GET /runs", reply: () => json([runJson()]) }, noRequests],
  });
  await screen.findByText("ticket-accepted");
  const headers = screen.getAllByRole("columnheader");
  expect(headers.map((h) => h.textContent)).toEqual([
    "Run",
    "Run ID",
    "State",
    "Activity",
    "Elapsed",
    "Created",
    "Project",
  ]);
  expect(headers[4]).toHaveClass("text-right", "tabular-nums");
  expect(screen.getByText("04:00").closest("td")).toHaveClass("text-right");
});

test("a long project path is shortened to its last two segments, whole on hover", async () => {
  renderApp(<RunListScreen />, {
    server: [
      {
        on: "GET /runs",
        reply: () => json([runJson({ project_path: "/home/op/buildgate/walk/workspace" })]),
      },
      noRequests,
    ],
  });
  const path = await screen.findByText("…/walk/workspace");
  expect(path).toHaveAttribute("title", "/home/op/buildgate/walk/workspace");
});
