import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { type FakeRoute, apiErrorResponse, json, renderApp } from "@/test/render";

import { NewRunScreen } from "./NewRunScreen";

const acceptedRun = {
  id: "run-accepted",
  ticket: "ticket-accepted",
  project_path: "/projects/app",
  workspace_path: "/workspaces/run-accepted",
  spec_path: "/specs/ticket-accepted.md",
  spec_sha256: "spec-accepted",
  state: "accepted",
  base_sha: "base-accepted",
  committed_by_factoryd: true,
  created_at: "2026-08-26T11:00:00Z",
  updated_at: "2026-08-26T11:04:00Z",
};

function renderForm(server: readonly FakeRoute[], startToken?: string) {
  return renderApp(<NewRunScreen />, {
    path: "/app/runs/new",
    pattern: "/app/runs/new",
    server,
    ...(startToken === undefined ? {} : { tokens: { startToken } }),
  });
}

async function type(label: string, value: string) {
  await userEvent.type(screen.getByLabelText(label), value);
}

async function fillAll() {
  await type("Ticket", "ticket-new");
  await type("Workspace path", "/repo/workspace");
  await type("Spec path", "/repo/spec/spec.md");
  await type("Repository", "/projects/app");
  await type("Temporal address", "localhost:7233");
}

test("new-run intake posts its fields and opens the run detail", async () => {
  const { server } = renderForm(
    [{ on: "POST /runs", reply: () => json(acceptedRun, 202) }],
    "start-token",
  );
  await type("Ticket", "ticket-new");
  await type("Workspace path", "/workspaces/ticket-new");
  await type("Spec path", "/specs/ticket-new.md");
  await type("Repository", "/projects/app");
  await type("Temporal address", "localhost:7233");

  await userEvent.click(screen.getByRole("button", { name: "Start run" }));

  expect(await screen.findByText("Navigated to /runs/run-accepted")).toBeInTheDocument();
  const posted = server.sent("POST /runs")[0];
  expect(posted?.headers.Authorization).toBe("Bearer start-token");
  expect(posted?.body).toEqual({
    ticket: "ticket-new",
    workspace: "/workspaces/ticket-new",
    spec: "/specs/ticket-new.md",
    repository: "/projects/app",
    temporal_address: "localhost:7233",
  });
});

test("new-run intake reports required fields without posting", async () => {
  const { server } = renderForm([]);
  await userEvent.click(screen.getByRole("button", { name: "Start run" }));

  expect(server.requests).toHaveLength(0);
  for (const message of [
    "Ticket is required",
    "Workspace path is required",
    "Spec path is required",
    "Repository is required",
    "Temporal address is required",
  ]) {
    expect(screen.getByText(message)).toBeInTheDocument();
  }
});

test("a multi-part project-bootstrap failure renders as one bullet per check", async () => {
  renderForm([
    {
      on: "POST /runs",
      reply: () =>
        apiErrorResponse(
          400,
          "project-bootstrap preflight failed, run not started: " +
            "product_spec_frozen (spec/spec.md): could not read artifact | " +
            "ticket_structure (spec/tickets/012.md): could not read artifact",
        ),
    },
  ]);
  await fillAll();
  await userEvent.click(screen.getByRole("button", { name: "Start run" }));

  const alert = await screen.findByRole("alert");
  expect(alert).toHaveTextContent("Could not start run:");
  const bullets = screen.getAllByRole("listitem").map((item) => item.textContent);
  expect(bullets).toEqual([
    "project-bootstrap preflight failed, run not started: product_spec_frozen (spec/spec.md): could not read artifact",
    "ticket_structure (spec/tickets/012.md): could not read artifact",
  ]);
});

test('"Check project setup" reports a passing project without starting a run', async () => {
  const { server } = renderForm(
    [
      {
        on: "POST /projects/check",
        reply: () =>
          json({
            passed: true,
            checks: [{ check: "product_spec_frozen", path: "/repo/spec/spec.md", passed: true }],
          }),
      },
    ],
    "start-token",
  );
  await type("Workspace path", "/repo/workspace");
  await type("Repository", "/projects/app");

  await userEvent.click(screen.getByRole("button", { name: "Check project setup" }));

  expect(await screen.findByText("Project setup looks ready.")).toBeInTheDocument();
  expect(server.sent("POST /projects/check")[0]?.body).toEqual({
    workspace: "/repo/workspace",
    repository: "/projects/app",
  });
  expect(server.sent("POST /runs")).toHaveLength(0);
  expect(screen.getByText(/product_spec_frozen \(\/repo\/spec\/spec\.md\)/)).toBeInTheDocument();
});

test('"Check project setup" reports every failing artifact with its reason', async () => {
  renderForm([
    {
      on: "POST /projects/check",
      reply: () =>
        json({
          passed: false,
          checks: [
            {
              check: "product_spec_frozen",
              path: "/repo/spec/spec.md",
              passed: false,
              reasons: ["could not read artifact: no such file"],
            },
          ],
        }),
    },
  ]);
  await type("Workspace path", "/repo/workspace");

  await userEvent.click(screen.getByRole("button", { name: "Check project setup" }));

  expect(await screen.findByText("Project setup is not ready yet:")).toBeInTheDocument();
  expect(
    screen.getByText(
      "product_spec_frozen (/repo/spec/spec.md): could not read artifact: no such file",
    ),
  ).toBeInTheDocument();
});

test('"Check project setup" without a workspace reports the requirement locally, without a request', async () => {
  const { server } = renderForm([]);

  await userEvent.click(screen.getByRole("button", { name: "Check project setup" }));

  expect(server.requests).toHaveLength(0);
  expect(screen.getByText("Something went wrong")).toBeInTheDocument();
  await userEvent.click(screen.getByText("Details"));
  expect(screen.getByText("Workspace path is required to check a project")).toBeInTheDocument();
});

test("a project's quick-fill values arrive in the query string", () => {
  renderApp(<NewRunScreen />, {
    path: "/app/runs/new?workspace=%2Fworkspaces%2Fapp&spec=%2Fspecs%2Fapp-latest.md&repository=app-repo",
    pattern: "/app/runs/new",
  });
  expect(screen.getByLabelText("Workspace path")).toHaveValue("/workspaces/app");
  expect(screen.getByLabelText("Spec path")).toHaveValue("/specs/app-latest.md");
  expect(screen.getByLabelText("Repository")).toHaveValue("app-repo");
});
