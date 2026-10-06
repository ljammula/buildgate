import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectReleaseScreen } from "./ProjectReleaseScreen";

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
  return renderApp(<ProjectReleaseScreen />, {
    path: `/projects/${project}/release`,
    pattern: "/projects/:project/release",
    server: [{ on: `GET /projects/${project}/release`, reply }],
    ...(startToken === undefined ? {} : { tokens: { startToken } }),
  });
}

test("entering a project id loads its kill-switch state and history", async () => {
  const { server } = renderRelease("checkouts", () => json(projectRelease), "control-token");

  expect(await screen.findByText("Kill switch engaged")).toBeInTheDocument();
  const request = server.sent("GET /projects/checkouts/release")[0];
  expect(request?.headers.Authorization).toBe("Bearer control-token");
  expect(screen.getByText("operator@example.com")).toBeInTheDocument();
  expect(screen.getByText("incident 42")).toBeInTheDocument();
});

test("the screen exposes no kill-switch control", async () => {
  renderRelease("checkouts", () => json(projectRelease));
  await screen.findByText("Kill switch engaged");

  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  expect(screen.getByText(/deliberately not a console action/)).toBeInTheDocument();
});

test("a failed lookup for a different project clears the prior project's data", async () => {
  const { server } = renderRelease("checkouts", () => json(projectRelease));
  server.set("GET /projects/other-project/release", () =>
    apiErrorResponse(500, "kill switch is unreadable"),
  );
  expect(await screen.findByText("Kill switch engaged")).toBeInTheDocument();

  const input = screen.getByLabelText("Project id");
  await userEvent.clear(input);
  await userEvent.type(input, "other-project");
  await userEvent.click(screen.getByRole("button", { name: "Load" }));

  expect(await screen.findByText("Request failed (500)")).toBeInTheDocument();
  expect(screen.queryByText("Kill switch engaged")).not.toBeInTheDocument();
  expect(screen.queryByText("Kill switch clear")).not.toBeInTheDocument();
  expect(screen.queryByText("checkouts")).not.toBeInTheDocument();
});

test("a failed lookup is reported, not shown as disengaged", async () => {
  renderRelease("..%2Fetc", () => apiErrorResponse(400, "project must be a single path component"));
  expect(await screen.findByText("Request failed (400)")).toBeInTheDocument();
  expect(screen.queryByText("Kill switch clear")).not.toBeInTheDocument();
});

test("a failed refresh keeps the last state under a warning, and Retry waits for the refresh", async () => {
  const { server } = renderRelease("checkouts", () => json(projectRelease));
  expect(await screen.findByText("Kill switch engaged")).toBeInTheDocument();

  let release: (() => void) | undefined;
  server.set("GET /projects/checkouts/release", async () => {
    await new Promise<void>((resolve) => {
      release = resolve;
    });
    return apiErrorResponse(500, "kill switch is unreadable");
  });
  await userEvent.click(screen.getByRole("button", { name: "Load" }));
  expect(screen.getByRole("button", { name: "Load" })).toBeDisabled();
  release?.();

  expect(await screen.findByText(/refresh failed: kill switch is unreadable/)).toBeInTheDocument();
  expect(screen.getByText("Kill switch engaged")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
});
