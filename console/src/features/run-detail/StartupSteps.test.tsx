import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { decodeRun } from "@/domain/run";
import { renderApp } from "@/test/render";
import { readFixtureJson } from "@/test/fixtures";

import { StartupSteps } from "./StartupSteps";

const steps = (extra: unknown[]) =>
  decodeRun(
    { ...(readFixtureJson("api/run-running.json") as object), startup_steps: extra },
    "test",
  ).startupSteps;

const sample = steps([
  { name: "image", status: "done", detail: "worker@sha256:aaaa (configured)" },
  { name: "workspace", status: "done" },
  { name: "install", status: "skipped" },
  { name: "sandbox", status: "done", log: "launch.log" },
  { name: "verify", status: "running", wait: "queued behind run-a" },
  { name: "agent", status: "pending" },
]);

test("the six start-up steps are listed in order with their detail and wait", () => {
  renderApp(<StartupSteps runId="run-running" steps={sample} live />);
  const items = screen.getAllByTestId(/^startup-step-/).map((li) => li.getAttribute("data-testid"));
  expect(items).toEqual([
    "startup-step-image",
    "startup-step-workspace",
    "startup-step-install",
    "startup-step-sandbox",
    "startup-step-verify",
    "startup-step-agent",
  ]);
  expect(screen.getByTestId("startup-step-image")).toHaveTextContent(
    "worker@sha256:aaaa (configured)",
  );
  expect(screen.getByTestId("startup-step-install")).toHaveTextContent("Skipped");
  expect(screen.getByTestId("startup-step-verify")).toHaveTextContent(
    "Waiting: queued behind run-a",
  );
  expect(screen.getByTestId("startup-step-agent")).toHaveTextContent(/Pending/i);
});

test("only a step with a log offers it, and the log is read when opened, as text", async () => {
  const { server } = renderApp(<StartupSteps runId="run-running" steps={sample} live />, {
    server: [
      {
        on: "GET /runs/run-running/log?step=sandbox",
        reply: () =>
          new Response('<img src=x onerror="alert(1)">', {
            headers: { "Content-Type": "text/plain" },
          }),
      },
    ],
  });
  expect(screen.getAllByRole("button", { name: "Open log" })).toHaveLength(1);
  expect(server.sent("GET /runs/run-running/log?step=sandbox")).toHaveLength(0);
  await userEvent.click(screen.getByRole("button", { name: "Open log" }));
  const text = await screen.findByRole("region", { name: "Log of sandbox" });
  expect(text.querySelector("img")).toBeNull();
  expect(text).toHaveTextContent('<img src=x onerror="alert(1)">');
  expect(server.sent("GET /runs/run-running/log?step=sandbox")).toHaveLength(1);
});

test("a run whose server sent no steps shows no start-up section", () => {
  renderApp(<StartupSteps runId="run-running" steps={[]} live />);
  expect(screen.queryByText("Start-up")).not.toBeInTheDocument();
});
