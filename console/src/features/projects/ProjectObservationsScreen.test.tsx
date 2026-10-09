import { screen, within } from "@testing-library/react";

import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectObservationsScreen } from "./ProjectObservationsScreen";

function renderObservations(project: string, reply: () => Response) {
  return renderApp(<ProjectObservationsScreen />, {
    path: `/projects/${project}/observations`,
    pattern: "/projects/:project/observations",
    server: [{ on: `GET /projects/${project}/observations`, reply }],
  });
}

test("a project's observations list what its runs showed, with the output as text", async () => {
  renderObservations("app", () => json(readFixtureJson("api/project-observations.json")));

  expect(
    await screen.findByText(
      "Rounds 2 and 3 failed the same way (no changes made to the workspace; canonical verification failed).",
    ),
  ).toBeInTheDocument();
  expect(screen.getByText("Rounds 2 and 3 ended without changing any file.")).toBeInTheDocument();
  expect(screen.getAllByTestId("observation")).toHaveLength(5);
  // The counts: one row per kind that happened.
  expect(screen.getByText("Finished runs read")).toBeInTheDocument();
  expect(screen.getAllByText("Same failure twice running").length).toBeGreaterThan(0);
  expect(screen.queryByText("The run halted")).not.toBeInTheDocument();

  const first = screen.getAllByTestId("observation")[0]!;
  expect(within(first).getByRole("link", { name: "run-quarantined" })).toHaveAttribute(
    "href",
    "/runs/run-quarantined",
  );
  const output = within(first).getByRole("region", {
    name: "Output of run run-quarantined: round-logs/round-3/verify.log",
  });
  expect(output).toHaveTextContent("idempotency_test.go:41: key reused across accounts");
});

test("agent-reported text is rendered as text, never as markup", async () => {
  renderObservations("app", () =>
    json({
      project: "app",
      runs: 1,
      accepted_first_round: 0,
      counts: { fixed_after_failure: 1 },
      observations: [
        {
          kind: "fixed_after_failure",
          run_id: "r1",
          ticket: "t",
          at: "2026-10-08T10:00:00Z",
          what: "Round 1 failed (<img src=x onerror=alert(1)>); round 2 passed after changing a.go.",
          log: "round-logs/round-1/verify.log",
          excerpt: "<script>alert(1)</script> FAIL",
        },
      ],
    }),
  );
  const item = await screen.findByTestId("observation");
  expect(item).toHaveTextContent("<img src=x onerror=alert(1)>");
  expect(item.querySelector("img, script")).toBeNull();
});

test("a project with nothing to report says why", async () => {
  renderObservations("quiet", () =>
    json({ project: "quiet", runs: 4, accepted_first_round: 4, counts: {}, observations: [] }),
  );
  expect(
    await screen.findByText(
      "No finished run of this project recorded a failed round, a failed check or a halt.",
    ),
  ).toBeInTheDocument();
});

test("a project with no finished run says so, and a longer history says it was cut", async () => {
  renderObservations("empty", () =>
    json({ project: "empty", runs: 0, accepted_first_round: 0, counts: {}, observations: [] }),
  );
  expect(
    await screen.findByText("No finished run of this project is in this data directory."),
  ).toBeInTheDocument();
});

test("a cut list says the counts include the rest", async () => {
  renderObservations("busy", () =>
    json({
      project: "busy",
      runs: 300,
      accepted_first_round: 0,
      counts: { run_halted: 300 },
      truncated: true,
      observations: [
        { kind: "run_halted", run_id: "h1", what: "The run halted: context canceled." },
      ],
    }),
  );
  expect(
    await screen.findByText("Showing the 1 newest; the counts above include the rest."),
  ).toBeInTheDocument();
});

test("a failed load shows the server's error", async () => {
  renderObservations("app", () => apiErrorResponse(403, "read endpoint is not authorized"));
  expect(await screen.findByText(/read endpoint is not authorized/)).toBeInTheDocument();
});

test("the request-era kinds show their runs, request, thread ids and gate as names", async () => {
  renderObservations("app", () =>
    json({
      project: "app",
      runs: 2,
      accepted_first_round: 0,
      counts: { check_fixed: 1, review_comment_accepted: 1, operator_edit: 1 },
      observations: [
        {
          id: "0123456789abcdef",
          kind: "check_fixed",
          source: "run",
          run_id: "q1",
          accepted_run_id: "a1",
          what: "Run q1 was quarantined on canonical_verify; run a1 of the same ticket was accepted.",
          checks: [{ check: "canonical_verify", sentence: "The verify command failed." }],
        },
        {
          kind: "review_comment_accepted",
          source: "request",
          run_id: "pr-run-1",
          request_id: "req-1",
          ticket_index: 2,
          thread_ids: ["PRRT_a", "PRRT_b"],
          what: "Review round 1 of ticket 2 answered 2 review thread(s) and its commit was pushed.",
        },
        {
          kind: "operator_edit",
          source: "request",
          run_id: "",
          request_id: "req-1",
          stage: "spec_review",
          anchors: ["spec.md ## Acceptance criteria"],
          what: "The operator sent the draft back at spec_review.",
        },
      ],
    }),
  );
  const items = await screen.findAllByTestId("observation");
  expect(items).toHaveLength(3);
  expect(within(items[0]!).getByRole("link", { name: "a1" })).toHaveAttribute("href", "/runs/a1");
  expect(items[0]).toHaveTextContent("The verify command failed.");
  expect(items[1]).toHaveTextContent("PRRT_a, PRRT_b");
  expect(within(items[1]!).getByRole("link", { name: "req-1" })).toHaveAttribute(
    "href",
    "/requests/req-1",
  );
  expect(items[2]).toHaveTextContent("spec.md ## Acceptance criteria");
  expect(within(items[2]!).queryByRole("link", { name: "" })).toBeNull();
});
