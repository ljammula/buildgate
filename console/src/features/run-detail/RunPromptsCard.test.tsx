import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { apiErrorResponse, json, renderApp } from "@/test/render";
import { readFixtureJson, readFixtureText } from "@/test/fixtures";

import { RunPromptsCard } from "./RunPromptsCard";

test("the prompts a run was sent are listed collapsed, and a prompt is read only when opened", async () => {
  const { server } = renderApp(<RunPromptsCard runId="run-quarantined" />, {
    server: [
      {
        on: "GET /runs/run-quarantined/prompts",
        reply: () => json(readFixtureJson("api/run-prompts.json")),
      },
      {
        on: "GET /runs/run-quarantined/prompts/build-1/build-round-1",
        reply: () =>
          new Response(readFixtureText("api/run-prompt.txt"), {
            headers: { "Content-Type": "text/plain" },
          }),
      },
    ],
  });
  const section = await screen.findByRole("heading", { name: "Prompts sent" });
  expect(section).toBeInTheDocument();
  expect(screen.getByText(/2 prompts the factory sent to a model/)).toBeInTheDocument();
  expect(screen.getAllByTestId("run-prompt")).toHaveLength(2);
  // Nothing is read until one is opened.
  expect(server.sent("GET /runs/run-quarantined/prompts/build-1/build-round-1")).toHaveLength(0);

  await userEvent.click(section);
  await userEvent.click(screen.getByRole("button", { name: /^build-round-1/ }));
  const text = await screen.findByRole("region", { name: "Prompt build-round-1" });
  expect(text).toHaveTextContent("Fix the idempotency key scope.");
  expect(server.sent("GET /runs/run-quarantined/prompts/build-1/build-round-1")).toHaveLength(1);
  expect(server.sent("GET /runs/run-quarantined/prompts/build-1/build-round-2")).toHaveLength(0);
});

test("a prompt is shown as text, never as HTML", async () => {
  renderApp(<RunPromptsCard runId="run-quarantined" />, {
    server: [
      {
        on: "GET /runs/run-quarantined/prompts",
        reply: () => json(readFixtureJson("api/run-prompts.json")),
      },
      {
        on: "GET /runs/run-quarantined/prompts/build-1/build-round-1",
        reply: () =>
          new Response('<img src=x onerror="alert(1)">', {
            headers: { "Content-Type": "text/plain" },
          }),
      },
    ],
  });
  await userEvent.click(await screen.findByRole("heading", { name: "Prompts sent" }));
  await userEvent.click(await screen.findByRole("button", { name: /^build-round-1/ }));
  const text = await screen.findByRole("region", { name: "Prompt build-round-1" });
  expect(text.querySelector("img")).toBeNull();
  expect(text).toHaveTextContent('<img src=x onerror="alert(1)">');
});

test("a run with no prompts, or whose list cannot be read, shows nothing", async () => {
  const none = renderApp(<RunPromptsCard runId="run-accepted" />, {
    server: [{ on: "GET /runs/run-accepted/prompts", reply: () => json({ prompts: [] }) }],
  });
  await waitFor(() => {
    expect(none.server.sent("GET /runs/run-accepted/prompts")).toHaveLength(1);
  });
  expect(screen.queryByTestId("run-prompts")).not.toBeInTheDocument();
  none.unmount();

  renderApp(<RunPromptsCard runId="run-gone" />, {
    server: [
      { on: "GET /runs/run-gone/prompts", reply: () => apiErrorResponse(404, "run not found") },
    ],
  });
  await waitFor(() => {
    expect(screen.queryByTestId("run-prompts")).not.toBeInTheDocument();
  });
});
