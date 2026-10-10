import { screen } from "@testing-library/react";

import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectReleasePanel } from "./ProjectReleasePanel";

const projectRelease = {
  project: "checkouts",
  kill_switch: {
    project: "checkouts",
    engaged: true,
    history: [
      {
        engaged: true,
        by: "operator@example.com",
        reason: "incident 42",
        at: "2026-09-03T09:00:00Z",
      },
    ],
  },
};

function renderRelease(project: string, reply: () => Response, startToken?: string) {
  return renderApp(<ProjectReleasePanel project={project} />, {
    server: [{ on: `GET /projects/${project}/release`, reply }],
    ...(startToken === undefined ? {} : { tokens: { startToken } }),
  });
}

test("a project's kill-switch state and history load with the start token", async () => {
  const { server } = renderRelease("checkouts", () => json(projectRelease), "control-token");

  expect(await screen.findByText("Kill switch engaged")).toBeInTheDocument();
  const request = server.sent("GET /projects/checkouts/release")[0];
  expect(request?.headers.Authorization).toBe("Bearer control-token");
  expect(screen.getByText("operator@example.com")).toBeInTheDocument();
  expect(screen.getByText("incident 42")).toBeInTheDocument();
});

test("the panel exposes no kill-switch control", async () => {
  renderRelease("checkouts", () => json(projectRelease));
  await screen.findByText("Kill switch engaged");

  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  expect(screen.getByText(/deliberately not a console action/)).toBeInTheDocument();
});

test("a failed lookup is reported, not shown as disengaged", async () => {
  renderApp(<ProjectReleasePanel project="../etc" />, {
    server: [
      {
        on: "GET /projects/..%2Fetc/release",
        reply: () => apiErrorResponse(400, "project must be a single path component"),
      },
    ],
  });
  expect(await screen.findByText("Request failed (400)")).toBeInTheDocument();
  expect(screen.queryByText("Kill switch clear")).not.toBeInTheDocument();
});

test("a failed refresh keeps the last state under a warning, and Retry is offered", async () => {
  const { server, queryClient } = renderRelease("checkouts", () => json(projectRelease));
  expect(await screen.findByText("Kill switch engaged")).toBeInTheDocument();

  server.set("GET /projects/checkouts/release", () =>
    apiErrorResponse(500, "kill switch is unreadable"),
  );
  void queryClient.refetchQueries();

  expect(await screen.findByText(/refresh failed: kill switch is unreadable/)).toBeInTheDocument();
  expect(screen.getByText("Kill switch engaged")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
});
