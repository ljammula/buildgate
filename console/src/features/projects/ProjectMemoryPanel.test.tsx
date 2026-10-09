import { screen, within } from "@testing-library/react";

import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectMemoryPanel } from "./ProjectMemoryPanel";

function renderMemory(reply: () => Response) {
  return renderApp(<ProjectMemoryPanel project="app" />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [{ on: "GET /projects/app/memory", reply }],
  });
}

const memory = {
  project: "app",
  on: true,
  budget_lines: 40,
  budget_chars: 3000,
  used_lines: 1,
  used_chars: 15,
  in_force: ["- Use go 1.26."],
  candidates: [
    {
      id: "0123456789abcdef",
      line: "- Run `make gen` before <img src=x onerror=alert(1)>.",
      source: "agent",
      state: "proposed",
      seen: 2,
      request_id: "memory-app-1",
    },
    {
      id: "fedcba9876543210",
      line: "- A dropped line.",
      source: "operator",
      state: "dropped",
      seen: 1,
    },
  ],
};

test("the panel shows the switch, the budget, the lines in force and the candidates as text", async () => {
  renderMemory(() => json(memory));
  expect(await screen.findByText("On")).toBeInTheDocument();
  expect(screen.getByText("1 of 40 lines, 15 of 3000 characters")).toBeInTheDocument();
  expect(screen.getAllByTestId("memory-line").map((li) => li.textContent)).toEqual([
    "- Use go 1.26.",
  ]);
  const candidates = screen.getAllByTestId("memory-candidate");
  expect(candidates).toHaveLength(2);
  expect(candidates[0]).toHaveTextContent("- Run `make gen` before <img src=x onerror=alert(1)>.");
  expect(candidates[0]!.querySelector("img")).toBeNull();
  expect(candidates[0]).toHaveTextContent("seen in 2 runs");
  expect(within(candidates[0]!).getByRole("link", { name: "memory-app-1" })).toHaveAttribute(
    "href",
    "/requests/memory-app-1",
  );
  expect(candidates[1]).toHaveTextContent("seen in 1 run");
  // Read only: nothing on the panel acts.
  expect(screen.queryByRole("button")).toBeNull();
});

test("the contract fixture renders", async () => {
  renderMemory(() => json(readFixtureJson("api/project-memory.json")));
  expect(await screen.findByText("every-field line")).toBeInTheDocument();
  expect(screen.getByText(/every-field section_error/)).toBeInTheDocument();
});

test("memory that is off says which switch, and an empty memory says how a line gets there", async () => {
  renderMemory(() =>
    json({
      ...memory,
      on: false,
      off_reason: "this repository is not listed under memory.repositories in the session config",
      in_force: [],
      candidates: [],
    }),
  );
  expect(
    await screen.findByText(
      "Off: this repository is not listed under memory.repositories in the session config",
    ),
  ).toBeInTheDocument();
  expect(screen.getByText("No line in force")).toBeInTheDocument();
  expect(screen.getByText("No candidate")).toBeInTheDocument();
});

test("a failed load shows the server's error", async () => {
  renderMemory(() => apiErrorResponse(404, "no request was submitted for this project"));
  expect(await screen.findByText(/no request was submitted for this project/)).toBeInTheDocument();
});
