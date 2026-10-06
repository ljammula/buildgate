import { screen } from "@testing-library/react";

import { apiErrorResponse, json, renderApp } from "@/test/render";

import { DaemonStatusCard } from "./DaemonStatusCard";

describe("DaemonStatusCard", () => {
  it("shows worker alive when heartbeat is fresh", async () => {
    renderApp(<DaemonStatusCard />, {
      server: [
        {
          on: "GET /queue-run",
          reply: json({ state: "alive", last_heartbeat: "2026-09-10T09:00:00Z" }),
        },
        { on: "GET /daemons", reply: apiErrorResponse(404, "not found") },
      ],
    });

    expect(await screen.findByText("Running")).toBeInTheDocument();
    expect(screen.getByText(/last heartbeat/)).toBeInTheDocument();
    expect(screen.queryByText(/Start the worker/)).not.toBeInTheDocument();
  });

  it("shows worker stale and the start command", async () => {
    renderApp(<DaemonStatusCard />, {
      server: [
        {
          on: "GET /queue-run",
          reply: json({ state: "stale", last_heartbeat: "2026-09-10T08:00:00Z" }),
        },
        { on: "GET /daemons", reply: apiErrorResponse(404, "not found") },
      ],
    });

    expect(await screen.findByText("Stale")).toBeInTheDocument();
    expect(screen.getByText("Start the worker:")).toBeInTheDocument();
    expect(screen.getByText("factoryd worker")).toBeInTheDocument();
  });

  it("shows worker not running and the start command", async () => {
    renderApp(<DaemonStatusCard />, {
      server: [
        { on: "GET /queue-run", reply: json({ state: "absent" }) },
        { on: "GET /daemons", reply: apiErrorResponse(404, "not found") },
      ],
    });

    expect(await screen.findByText("Not running")).toBeInTheDocument();
    expect(screen.getByText("Start the worker:")).toBeInTheDocument();
    expect(screen.getByText("factoryd worker")).toBeInTheDocument();
  });

  it("shows error inside the card when heartbeat route fails", async () => {
    renderApp(<DaemonStatusCard />, {
      server: [
        {
          on: "GET /queue-run",
          reply: apiErrorResponse(500, "internal server error"),
        },
      ],
    });

    expect(await screen.findByText("Request failed (500)")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Daemons" })).toBeVisible();
  });

  it("lists serve-managed daemons when available", async () => {
    renderApp(<DaemonStatusCard />, {
      server: [
        {
          on: "GET /queue-run",
          reply: json({ state: "alive", last_heartbeat: "2026-09-10T09:00:00Z" }),
        },
        {
          on: "GET /daemons",
          reply: json([
            {
              repository: "acme/app",
              state: "running",
              pid: 4242,
              started_at: "2026-09-10T09:00:00Z",
              heartbeat_updated_at: "2026-09-10T09:00:00Z",
            },
          ]),
        },
      ],
    });

    expect(await screen.findByText("Running")).toBeInTheDocument();
    const managed = await screen.findByRole("list", { name: "Started by serve" });
    expect(managed).toHaveTextContent("acme/app");
    expect(managed).toHaveTextContent("running");
    expect(managed).toHaveTextContent("started");
  });

  it("shows nothing for daemons when the route is not authorized", async () => {
    renderApp(<DaemonStatusCard />, {
      server: [
        {
          on: "GET /queue-run",
          reply: json({ state: "alive", last_heartbeat: "2026-09-10T09:00:00Z" }),
        },
        { on: "GET /daemons", reply: apiErrorResponse(403, "forbidden") },
      ],
    });

    expect(await screen.findByText("Running")).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Started by serve" })).not.toBeInTheDocument();
  });
});
