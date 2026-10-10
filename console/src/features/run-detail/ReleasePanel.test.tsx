import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { formatLocalTimestamp } from "@/domain/elapsed";
import { ReleasePanel } from "@/features/run-detail/ReleasePanel";
import {
  allowedRelease,
  deniedRelease,
  recordingFailedRelease,
  undecidedRelease,
} from "@/features/run-detail/testRuns";
import { apiErrorResponse, fakeServer, json, renderApp } from "@/test/render";

const route = "GET /runs/run-accepted/release";

function renderRelease(server: ReturnType<typeof fakeServer>, tokens = {}) {
  return renderApp(<ReleasePanel runId="run-accepted" />, { server, tokens });
}

// Proves the release view surfaces the durable audit fields of a denied
// decision, the reasons included, plus the project kill switch's current state
// and its attributable who/why/when history.
test("denied decision shows reasons and kill-switch attribution", async () => {
  const server = fakeServer([{ on: route, reply: json(deniedRelease) }]);
  renderRelease(server, { startToken: "control-token" });

  expect(await screen.findByText("Denied")).toBeInTheDocument();
  expect(server.sent(route)[0]?.headers).toMatchObject({ Authorization: "Bearer control-token" });
  expect(screen.queryByText("Allowed")).not.toBeInTheDocument();
  expect(screen.getByText(/kill switch is engaged for project "checkouts"/)).toBeInTheDocument();
  expect(screen.getByText("Kill switch engaged")).toBeInTheDocument();
  expect(screen.getByText(/By: operator@example.com/)).toBeInTheDocument();
  expect(screen.getByText(/Reason: incident 42/)).toBeInTheDocument();
  expect(
    screen.getByText(`At: ${formatLocalTimestamp("2026-09-03T09:00:00Z")}`),
  ).toBeInTheDocument();
});

// An allowed decision is recorded evidence, not a pending action: nothing here
// merges, pushes or deploys from one, and the page says so.
test("allowed decision states that nothing acts on it", async () => {
  renderRelease(fakeServer([{ on: route, reply: json(allowedRelease) }]));

  expect(await screen.findByText("Allowed")).toBeInTheDocument();
  expect(screen.getByText(/No merge, push, or deploy is performed/)).toBeInTheDocument();
  expect(screen.getByText("Kill switch clear")).toBeInTheDocument();
  expect(screen.getByText(/Never engaged/)).toBeInTheDocument();
});

// Fail-closed: a run with no recorded decision is shown as having none.
test("a run with no recorded decision is not shown as allowed", async () => {
  renderRelease(fakeServer([{ on: route, reply: json(undecidedRelease) }]));

  expect(await screen.findByText("No decision recorded")).toBeInTheDocument();
  expect(screen.queryByText("Allowed")).not.toBeInTheDocument();
  expect(screen.queryByText("Denied")).not.toBeInTheDocument();
  expect(screen.getByText(/No release decision has been recorded/)).toBeInTheDocument();
});

// "Recording failed" must never read the same as a run that was never evaluated.
test("a decision recording failure is shown distinctly from no decision", async () => {
  renderRelease(fakeServer([{ on: route, reply: json(recordingFailedRelease) }]));

  expect(await screen.findByText(/release decision could not be recorded/)).toBeInTheDocument();
  expect(screen.queryByText("No decision recorded")).not.toBeInTheDocument();
  expect(screen.queryByText("Allowed")).not.toBeInTheDocument();
  expect(screen.queryByText("Denied")).not.toBeInTheDocument();
  expect(screen.getByText(/factoryd retry/)).toBeInTheDocument();
});

// The kill switch is CLI-only on purpose, so reaching for it does not depend on
// a healthy factoryd serve: a real invariant of the design.
test("the screen exposes no kill-switch control", async () => {
  renderRelease(fakeServer([{ on: route, reply: json(deniedRelease) }]));
  await screen.findByText("Denied");

  expect(screen.getAllByRole("button").map((b) => b.getAttribute("aria-label"))).toEqual([
    "Refresh",
  ]);
  expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  expect(screen.getByText(/factoryd kill-switch/)).toBeInTheDocument();
});

// Regression (Codex review): the view loaded once, so a page left open across a
// run's acceptance kept showing "No decision recorded". Refresh re-fetches and
// the view updates.
test("refresh reloads the release view after the decision changes", async () => {
  const server = fakeServer([{ on: route, reply: json(undecidedRelease) }]);
  renderRelease(server);
  expect(await screen.findByText("No decision recorded")).toBeInTheDocument();
  expect(server.sent(route)).toHaveLength(1);

  server.set(route, json(allowedRelease));
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));

  expect(await screen.findByText("Allowed")).toBeInTheDocument();
  expect(server.sent(route)).toHaveLength(2);
  expect(screen.queryByText("No decision recorded")).not.toBeInTheDocument();
});

// Regression (Codex review): a refresh failure after a successful load was
// discarded silently, so a stale kill-switch state read as current. The
// retained data stays visible, and the failure is said plainly beside it.
test("a refresh failure surfaces a stale-data warning, not silence", async () => {
  const server = fakeServer([{ on: route, reply: json(deniedRelease) }]);
  renderRelease(server);
  expect(await screen.findByText("Denied")).toBeInTheDocument();
  expect(screen.queryByTestId("release-stale-banner")).not.toBeInTheDocument();

  server.set(route, apiErrorResponse(500, "factory is unreachable"));
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));

  const banner = await screen.findByTestId("release-stale-banner");
  expect(server.sent(route)).toHaveLength(2);
  // A failed refresh must never blank data already loaded.
  expect(screen.getByText("Denied")).toBeInTheDocument();
  expect(within(banner).getByText(/refresh failed/)).toBeInTheDocument();
});

// Regression (Codex review): the banner's Retry stayed enabled while a request
// was in flight, so a second tap could start a concurrent request.
test("the stale-banner retry action disables while a request is in flight", async () => {
  const server = fakeServer([{ on: route, reply: json(deniedRelease) }]);
  renderRelease(server);
  await screen.findByText("Denied");
  server.set(route, apiErrorResponse(500, "boom"));
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
  const banner = await screen.findByTestId("release-stale-banner");

  let release = () => {};
  const hold = new Promise<void>((resolve) => {
    release = resolve;
  });
  server.set(route, async () => {
    await hold;
    return json(deniedRelease);
  });
  await userEvent.click(within(banner).getByRole("button", { name: "Retry" }));

  // Retry disables itself while its own request is in flight, the way Refresh does.
  await waitFor(() => {
    expect(within(banner).getByRole("button", { name: "Retry" })).toBeDisabled();
  });
  expect(screen.getByRole("button", { name: "Refresh" })).toBeDisabled();

  release();
  await waitFor(() => {
    expect(screen.queryByTestId("release-stale-banner")).not.toBeInTheDocument();
  });
  expect(server.sent(route)).toHaveLength(3);
});

test("a failed release fetch is reported, not shown as allowed", async () => {
  renderRelease(
    fakeServer([{ on: route, reply: apiErrorResponse(403, "release endpoint is not authorized") }]),
  );

  // GET /runs/{id}/release is start-token-gated: a 403 carries the start-token guidance.
  expect(await screen.findByText("Not authorized")).toBeInTheDocument();
  expect(screen.getByText(/the token changes every restart/)).toBeInTheDocument();
  expect(screen.queryByText("Allowed")).not.toBeInTheDocument();
});
