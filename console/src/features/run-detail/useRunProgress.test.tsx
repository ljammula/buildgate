import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { renderApp, sseResponse } from "@/test/render";

import { useRunProgress } from "./useRunProgress";

function event(stage: string) {
  return { ts: "2026-09-17T10:00:00.000Z", source: "factory", stage, event: "start" };
}

function Probe() {
  const [id, setId] = useState("run-a");
  const { events } = useRunProgress(id);
  return (
    <>
      <button
        onClick={() => {
          setId("run-b");
        }}
      >
        Switch
      </button>
      <p>{id}</p>
      <ul>
        {events.map((e) => (
          <li key={e.stage}>{e.stage}</li>
        ))}
      </ul>
    </>
  );
}

describe("useRunProgress", () => {
  // Proves the feed belongs to its run: run A's events must not be shown
  // under run B, not even for the render before B's first event arrives.
  it("shows no event of the first run after the id changes", async () => {
    renderApp(<Probe />, {
      server: [
        { on: "GET /runs/run-a/progress", reply: sseResponse("progress", [event("stage_of_a")]) },
        { on: "GET /runs/run-b/progress", reply: sseResponse("progress", []) },
      ],
    });
    expect(await screen.findByText("stage_of_a")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Switch" }));

    expect(screen.getByText("run-b")).toBeInTheDocument();
    expect(screen.queryByText("stage_of_a")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByText("stage_of_a")).not.toBeInTheDocument();
    });
  });
});
