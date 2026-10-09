import { screen, waitFor, within } from "@testing-library/react";

import { asObject } from "@/domain/decode";
import { decodeRun } from "@/domain/run";
import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { RunHandoffCard } from "./RunHandoffCard";

function fixtureRun(name: string, over: Record<string, unknown> = {}) {
  const at = "GET /runs/{id}";
  return decodeRun({ ...asObject(readFixtureJson(`api/${name}.json`), at), ...over }, at);
}

test("a stopped run's card lists each failed check, its finding and how it is sorted", async () => {
  const run = fixtureRun("run-quarantined");
  const { server } = renderApp(<RunHandoffCard run={run} />, {
    server: [
      {
        on: "GET /runs/run-quarantined/handoff",
        reply: () => json(readFixtureJson("api/run-handoff.json")),
      },
    ],
  });

  expect(
    await screen.findByText(
      "Every failed check is of a kind a build can fix when it is told what failed.",
    ),
  ).toBeInTheDocument();
  const checks = screen.getAllByTestId("run-handoff-check");
  expect(checks).toHaveLength(2);
  expect(within(checks[0]!).getByText("canonical_verify")).toBeInTheDocument();
  expect(
    within(checks[0]!).getByText("Of a kind a build can fix when told what failed"),
  ).toBeInTheDocument();
  expect(
    within(checks[0]!).getByText("Failed (exit 1); the factory has no more to say about it."),
  ).toBeInTheDocument();
  // The attempt committed nothing, so the check on its diff was not judged.
  expect(within(checks[1]!).getByText("tests_added: no test file changed")).toBeInTheDocument();
  expect(
    within(checks[1]!).getByText(
      "Not judged: the attempt committed nothing, so there was no diff to check",
    ),
  ).toBeInTheDocument();
  expect(within(checks[1]!).queryByText("Of a kind a build is never told about")).toBeNull();
  expect(server.sent("GET /runs/run-quarantined/handoff")).toHaveLength(1);
});

test("a run that recorded no handoff shows no card and asks the server nothing", () => {
  const { server } = renderApp(<RunHandoffCard run={fixtureRun("run-accepted")} />, {
    server: [{ on: "GET /runs/run-accepted/handoff", reply: () => json({}) }],
  });
  expect(screen.queryByTestId("run-handoff")).not.toBeInTheDocument();
  expect(server.sent("GET /runs/run-accepted/handoff")).toHaveLength(0);
});

test("a finding is rendered as text", async () => {
  renderApp(<RunHandoffCard run={fixtureRun("run-quarantined")} />, {
    server: [
      {
        on: "GET /runs/run-quarantined/handoff",
        reply: () =>
          json({
            run_id: "run-quarantined",
            state: "quarantined",
            next: "corrective",
            checks: [
              {
                check: "lint",
                bin: "corrective",
                exit_code: 2,
                finding: 'lint failed: the log says "<img src=x onerror=alert(1)>"',
                output: ["error: <script>alert(1)</script>", "error: second line"],
              },
            ],
          }),
      },
    ],
  });
  const check = await screen.findByTestId("run-handoff-check");
  expect(check).toHaveTextContent("<img src=x onerror=alert(1)>");
  expect(check.querySelector("img, script")).toBeNull();
  expect(screen.getByRole("region", { name: "Output of lint" })).toHaveTextContent(
    "error: <script>alert(1)</script> error: second line",
  );
});

test("a handoff the server will not vouch for shows nothing, and another failure shows its error", async () => {
  const refused = renderApp(<RunHandoffCard run={fixtureRun("run-quarantined")} />, {
    server: [
      {
        on: "GET /runs/run-quarantined/handoff",
        reply: () => apiErrorResponse(409, "run's handoff cannot be used"),
      },
    ],
  });
  await waitFor(() => {
    expect(refused.server.sent("GET /runs/run-quarantined/handoff")).toHaveLength(1);
  });
  await waitFor(() => expect(screen.queryByTestId("run-handoff")).not.toBeInTheDocument());
  refused.unmount();

  renderApp(<RunHandoffCard run={fixtureRun("run-quarantined")} />, {
    server: [
      {
        on: "GET /runs/run-quarantined/handoff",
        reply: () => apiErrorResponse(500, "load run"),
      },
    ],
  });
  expect(await screen.findByText(/load run/)).toBeInTheDocument();
});

// An operator has just overridden the run: its record still names the old
// handoff until the next read, and the old record must not be shown for it.
test("a handoff that describes another state than the run is in shows nothing", async () => {
  const run = fixtureRun("run-quarantined", { state: "accepted" });
  const { server } = renderApp(<RunHandoffCard run={run} />, {
    server: [
      {
        on: "GET /runs/run-quarantined/handoff",
        reply: () => json(readFixtureJson("api/run-handoff.json")),
      },
    ],
  });
  await waitFor(() => {
    expect(server.sent("GET /runs/run-quarantined/handoff")).toHaveLength(1);
  });
  await waitFor(() => expect(screen.queryByTestId("run-handoff")).not.toBeInTheDocument());
});
