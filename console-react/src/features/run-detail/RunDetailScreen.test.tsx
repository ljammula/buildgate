import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { formatLocalTimestamp } from "@/domain/elapsed";
import { RunDetailScreen } from "@/features/run-detail/RunDetailScreen";
import {
  acceptedRun,
  deniedRelease,
  evidenceRound,
  inProgressRun,
  quarantinedRun,
  waitingRun,
  withEvidence,
} from "@/features/run-detail/testRuns";
import {
  type FakeRoute,
  type RenderAppOptions,
  apiErrorResponse,
  json,
  renderApp,
  sseResponse,
} from "@/test/render";

beforeAll(() => {
  Object.assign(Element.prototype, {
    hasPointerCapture: () => false,
    scrollIntoView: () => undefined,
  });
});

type Wire = Record<string, unknown>;

const pattern = "/runs/:id";

interface RunOptions extends Pick<RenderAppOptions, "config" | "tokens"> {
  /** Progress lines the feed serves; omit for an empty feed that stays open. */
  readonly progress?: readonly unknown[];
  readonly routes?: readonly FakeRoute[];
  readonly query?: string;
}

/**
 * The run page for `run`, with its three feeds answered: the run itself, its
 * state stream (open, empty) and its progress feed.
 */
function renderRun(run: Wire, options: RunOptions = {}) {
  const id = run.id as string;
  return renderApp(<RunDetailScreen />, {
    path: `/runs/${id}${options.query ?? ""}`,
    pattern,
    ...(options.config ? { config: options.config } : {}),
    ...(options.tokens ? { tokens: options.tokens } : {}),
    server: [
      { on: `GET /runs/${id}`, reply: json(run) },
      { on: `GET /runs/${id}/events`, reply: sseResponse("state") },
      { on: `GET /runs/${id}/progress`, reply: sseResponse("progress", options.progress ?? []) },
      ...(options.routes ?? []),
    ],
  });
}

function line(fields: Wire): Wire {
  return fields;
}

const finishedAccepted = line({
  ts: "2026-08-26T11:04:00.000Z",
  source: "factory",
  stage: "finished",
  event: "end",
  outcome: "accepted",
});

// The run page's own Created/Updated fields showed raw UTC verbatim (operator
// demo, 2026-09-26): both now route through the shared local-time formatter.
test("run detail shows local time, not raw UTC, for created_at", async () => {
  renderRun(acceptedRun());

  expect(await screen.findByText(formatLocalTimestamp("2026-08-26T11:00:00Z"))).toBeInTheDocument();
  expect(screen.queryByText("2026-08-26T11:00:00Z")).not.toBeInTheDocument();
});

// Found in the 2026-09-29 todo-kafka-service demo: the run page said nothing
// about the compose sidecars each phase launched.
test("run detail lists the compose services each phase launched", async () => {
  renderRun({
    ...acceptedRun(),
    compose_phases: [
      {
        phase: "build",
        enabled: true,
        services: [
          {
            name: "kafka",
            alias: "kafka",
            port: 19092,
            image: "apache/kafka:3.8.0",
            digest: "apache/kafka@sha256:c89f",
          },
        ],
      },
      {
        phase: "lint",
        enabled: false,
        disabled_reason: "no compose file found in the target repository",
      },
    ],
  });

  expect(await screen.findByRole("heading", { name: "Compose services" })).toBeInTheDocument();
  expect(
    screen.getByText("kafka: apache/kafka:3.8.0 at kafka:19092 (apache/kafka@sha256:c89f)"),
  ).toBeInTheDocument();
  expect(
    screen.getByText("Not launched: no compose file found in the target repository"),
  ).toBeInTheDocument();
});

test("run detail omits the compose section without compose data", async () => {
  renderRun(acceptedRun());

  await screen.findByText("ticket-accepted");
  expect(screen.queryByRole("heading", { name: "Compose services" })).not.toBeInTheDocument();
});

// P0e (role evidence): an attempt's role, worker model and requested-vs-sent
// thinking level render as one line on its card. The fixture attempt is
// thinking "max" with expected_effort "max" but relay_reasoning_effort "high":
// a real silent clamp, which must show.
test("attempt card shows role, model, and a genuine requested/sent clamp", async () => {
  renderRun(acceptedRun());

  expect(
    await screen.findByText(
      "Role: execution · Harness: pifork · Model: gpt-5.6-luna · Thinking: max (sent: high)",
    ),
  ).toBeInTheDocument();
});

// Found via review: comparing `thinking` directly against
// `relay_reasoning_effort` false-positives whenever a model's own
// thinkingLevelMap renames a level (max -> xhigh). expected_effort carries the
// translated value, and the hint is suppressed once it agrees with what was sent.
test("attempt card shows no clamp hint when a declared thinkingLevelMap translation matches what was sent", async () => {
  const run = acceptedRun();
  const attempt = (run.attempts as Wire[])[0] as Wire;
  attempt.expected_effort = "xhigh";
  attempt.relay_reasoning_effort = "xhigh";
  renderRun(run);

  expect(
    await screen.findByText(
      "Role: execution · Harness: pifork · Model: gpt-5.6-luna · Thinking: max",
    ),
  ).toBeInTheDocument();
  expect(screen.queryByText(/\(sent:/)).not.toBeInTheDocument();
});

// A run with no reference_oracle_dir (no -draft-oracles) never visits the two
// oracle-commit stages, so the timeline must not list them (operator demo, 2026-09-26).
test("timeline omits commit_oracles/post_oracle_commit_verify for a run with no oracle stage", async () => {
  renderRun(acceptedRun());

  await screen.findByTestId("timeline-row-prepare_workspace");
  expect(screen.queryByTestId("timeline-row-commit_oracles")).not.toBeInTheDocument();
  expect(screen.queryByTestId("timeline-row-post_oracle_commit_verify")).not.toBeInTheDocument();
});

test("timeline shows commit_oracles/post_oracle_commit_verify for a run with an oracle stage", async () => {
  renderRun({ ...acceptedRun(), reference_oracle_dir: "/data/requests/req-1/oracle" });

  expect(await screen.findByTestId("timeline-row-commit_oracles")).toBeInTheDocument();
  expect(screen.getByTestId("timeline-row-post_oracle_commit_verify")).toBeInTheDocument();
});

// A quarantined run is not terminal: an operator override can still move it
// later, so the page keeps watching it. The stream is a streamed fetch, so a
// served event reaches the page through the same parsing a real server's
// output would: the subscription was attempted and wired to live updates.
test("quarantined run detail screen watches for and applies later state changes", async () => {
  renderRun(quarantinedRun(), {
    routes: [
      {
        on: "GET /runs/run-quarantined/events",
        // After the first fetch has shown the quarantined run: the stream's
        // event is the later state change.
        reply: async () => {
          await new Promise((resolve) => setTimeout(resolve, 100));
          return sseResponse("state", [acceptedRun()])();
        },
      },
    ],
  });

  expect(await screen.findByRole("heading", { name: "ticket-quarantined" })).toBeInTheDocument();
  expect(await screen.findByRole("heading", { name: "ticket-accepted" })).toBeInTheDocument();
  expect(screen.getAllByText("Accepted").length).toBeGreaterThan(0);
});

// A failing connection retries quietly in the background, the way a native
// EventSource would, rather than surfacing as a permanent "Disconnected".
test("quarantined run detail screen keeps retrying rather than showing disconnected", async () => {
  const { server } = renderRun(quarantinedRun(), {
    routes: [{ on: "GET /runs/run-quarantined/events", reply: new Response("", { status: 500 }) }],
  });

  expect(await screen.findByText("ticket-quarantined")).toBeInTheDocument();
  await waitFor(() => {
    expect(server.sent("GET /runs/run-quarantined/events").length).toBeGreaterThan(0);
  });
  expect(screen.queryByText(/Disconnected/)).not.toBeInTheDocument();
});

// A permanent failure (a rotated token, a pruned run) is the one case that is shown.
test("a permanent stream failure is shown as Live updates: Disconnected", async () => {
  renderRun(quarantinedRun(), {
    routes: [
      {
        on: "GET /runs/run-quarantined/events",
        reply: apiErrorResponse(403, "read token rotated"),
      },
    ],
  });

  expect(await screen.findByText(/Disconnected/)).toBeInTheDocument();
  expect(screen.getByText("Live updates")).toBeInTheDocument();
});

// An accepted run genuinely is terminal: no subscription is attempted at all.
test("accepted run detail screen does not attempt to watch further updates", async () => {
  renderRun(acceptedRun());

  expect(await screen.findByText("ticket-accepted")).toBeInTheDocument();
  expect(screen.queryByText(/Live updates/)).not.toBeInTheDocument();
});

test("quarantined run override posts attribution and updates state", async () => {
  const { server } = renderRun(quarantinedRun(), {
    tokens: { overrideToken: "override-token" },
    routes: [
      {
        on: "POST /runs/run-quarantined/override",
        reply: json({ ...acceptedRun(), id: "run-quarantined" }),
      },
    ],
  });

  await userEvent.click(await screen.findByRole("button", { name: "Override run" }));
  const dialog = await screen.findByRole("dialog", { name: "Override quarantined run" });
  await userEvent.type(within(dialog).getByLabelText("Operator"), "operator@example.com");
  await userEvent.type(
    within(dialog).getByLabelText("Reason"),
    "Reviewed the verification evidence",
  );
  await userEvent.click(within(dialog).getByRole("button", { name: "Apply" }));

  await waitFor(() => {
    expect(server.sent("POST /runs/run-quarantined/override")).toHaveLength(1);
  });
  const sent = server.sent("POST /runs/run-quarantined/override")[0];
  expect(sent?.headers).toMatchObject({ Authorization: "Bearer override-token" });
  expect(sent?.body).toEqual({
    by: "operator@example.com",
    reason: "Reviewed the verification evidence",
    state: "accepted",
  });
  expect((await screen.findAllByText("Accepted")).length).toBeGreaterThan(0);
  // The run is accepted now: the override section is gone.
  expect(screen.queryByRole("button", { name: "Override run" })).not.toBeInTheDocument();
});

test("the override dialog opens with the reason the request page carried", async () => {
  renderRun(quarantinedRun(), {
    tokens: { overrideToken: "override-token" },
    query: `?reason=${encodeURIComponent("Request req-1 ticket 1 quarantined")}`,
  });

  await userEvent.click(await screen.findByRole("button", { name: "Override run" }));
  const dialog = await screen.findByRole("dialog", { name: "Override quarantined run" });
  expect(within(dialog).getByLabelText("Reason")).toHaveValue("Request req-1 ticket 1 quarantined");
});

test("the override dialog sends nothing until operator and reason are filled in", async () => {
  const { server } = renderRun(quarantinedRun(), { tokens: { overrideToken: "override-token" } });

  await userEvent.click(await screen.findByRole("button", { name: "Override run" }));
  const dialog = await screen.findByRole("dialog", { name: "Override quarantined run" });
  await userEvent.type(within(dialog).getByLabelText("Operator"), "  ");
  await userEvent.click(within(dialog).getByRole("button", { name: "Apply" }));

  expect(server.sent("POST /runs/run-quarantined/override")).toHaveLength(0);
  expect(screen.getByRole("dialog", { name: "Override quarantined run" })).toBeInTheDocument();
});

// An adversarial review (2026-09-24) found: the override button must be
// disabled when writes are enabled (the loopback no-token relaxation applies)
// but no override token exists: the server's override route never grants that
// relaxation, so an enabled button would 403 with no token the console can supply.
test("quarantined run override button is disabled without an override token even when writesEnabled", async () => {
  renderRun(quarantinedRun(), { config: { writesEnabled: true } });

  expect(await screen.findByRole("button", { name: "Override run" })).toBeDisabled();
});

test("the override section only appears for a quarantined run", async () => {
  renderRun(acceptedRun());

  await screen.findByText("ticket-accepted");
  expect(screen.queryByRole("heading", { name: "Operator override" })).not.toBeInTheDocument();
});

// The "View diff" view both appears (the run has a diff snapshot) and fetches
// and displays the real diff from the backing endpoint, not just the
// file-count summary already on the Overview. It is a view of the run page
// selected by the URL, so a reload lands on it.
test("accepted run detail screen navigates to the diff view", async () => {
  const { server, location } = renderRun(acceptedRun(), {
    routes: [
      {
        on: "GET /runs/run-accepted/diff",
        reply: json({ diff: "+added line\n-removed line\n", truncated: false }),
      },
    ],
  });

  const tab = await screen.findByRole("tab", { name: "View diff" });
  // Fetched only once the view is selected.
  expect(server.sent("GET /runs/run-accepted/diff")).toHaveLength(0);
  await userEvent.click(tab);

  expect(await screen.findByText("+added line")).toBeInTheDocument();
  expect(location()).toBe("/runs/run-accepted?view=diff");
});

test("a shared diff link lands on the diff view, and Overview returns to the run page", async () => {
  const { location } = renderRun(acceptedRun(), {
    query: "?view=diff",
    routes: [
      {
        on: "GET /runs/run-accepted/diff",
        reply: json({ diff: "+added line\n", truncated: false }),
      },
    ],
  });

  expect(await screen.findByText("+added line")).toBeInTheDocument();
  expect(screen.getByRole("tab", { name: "View diff" })).toHaveAttribute("aria-selected", "true");

  await userEvent.click(screen.getByRole("tab", { name: "Overview" }));
  expect(await screen.findByRole("heading", { name: "Timeline" })).toBeInTheDocument();
  expect(location()).toBe("/runs/run-accepted");
});

// Gated on diffAvailable, not merely a result SHA (found via review): a run
// whose evidence collection warned-and-continued on the diff step has a
// result SHA but no snapshot, so the view would only ever show an error.
test("the diff view is not offered when no diff snapshot exists", async () => {
  renderRun({ ...acceptedRun(), diff_available: false });

  await screen.findByText("ticket-accepted");
  expect(screen.queryByRole("tab", { name: "View diff" })).not.toBeInTheDocument();
});

// The run page reaches the release view, which renders the run's real recorded
// decision from the backing endpoint, not a locally inferred one.
test("run detail screen navigates to the release view", async () => {
  const { server, location } = renderRun(acceptedRun(), {
    routes: [{ on: "GET /runs/run-accepted/release", reply: json(deniedRelease) }],
  });

  const tab = await screen.findByRole("tab", { name: "View release decision" });
  expect(server.sent("GET /runs/run-accepted/release")).toHaveLength(0);
  await userEvent.click(tab);

  expect(await screen.findByText("Denied")).toBeInTheDocument();
  expect(screen.getByText("Kill switch engaged")).toBeInTheDocument();
  expect(location()).toBe("/runs/run-accepted?view=release");
});

// A truncated diff is visibly flagged, not silently presented as the complete
// change (a real P2 finding from review: the client used to discard the flag).
test("truncated diff shows a warning banner", async () => {
  renderRun(acceptedRun(), {
    routes: [
      {
        on: "GET /runs/run-accepted/diff",
        reply: json({ diff: "+added line\n", truncated: true }),
      },
    ],
  });

  await userEvent.click(await screen.findByRole("tab", { name: "View diff" }));

  expect(await screen.findByText(/truncated/)).toBeInTheDocument();
  expect(screen.getByText("+added line")).toBeInTheDocument();
});

// The log pane is off by default (no `GET .../log` request until the operator
// opts in) and, once toggled on, renders the streamed text as plain text.
describe("live build log pane", () => {
  test("is off by default -- no log request until toggled on", async () => {
    const { server } = renderRun(acceptedRun());

    expect(await screen.findByRole("switch", { name: /Show live build log/ })).not.toBeChecked();
    expect(server.sent("GET /runs/run-accepted/log?follow=1")).toHaveLength(0);
    expect(screen.queryByRole("region", { name: "Build log" })).not.toBeInTheDocument();
  });

  test("toggling it on streams and renders the log as plain text", async () => {
    const { server } = renderRun(acceptedRun(), {
      routes: [
        {
          on: "GET /runs/run-accepted/log?follow=1",
          reply: () =>
            new Response(
              "building ticket-1\n" +
                'FACTORY_PROGRESS {"stage": "agent", "event": "note", "round": 1, "detail": "read: main.go"}\n' +
                "done\n",
            ),
        },
      ],
    });

    await userEvent.click(await screen.findByRole("switch", { name: /Show live build log/ }));

    const log = await screen.findByRole("region", { name: "Build log" });
    await waitFor(() => {
      expect(log).toHaveTextContent("building ticket-1");
    });
    expect(server.sent("GET /runs/run-accepted/log?follow=1")).toHaveLength(1);
    // A worker's protocol line reads as a step, never as raw JSON.
    expect(log.textContent).toContain("agent        read: main.go");
    expect(log.textContent).not.toContain("FACTORY_PROGRESS");
  });

  test("toggling it off unsubscribes, and on again starts from the full log", async () => {
    const { server } = renderRun(acceptedRun(), {
      routes: [
        { on: "GET /runs/run-accepted/log?follow=1", reply: () => new Response("full log\n") },
      ],
    });

    const toggle = await screen.findByRole("switch", { name: /Show live build log/ });
    await userEvent.click(toggle);
    await screen.findByText(/Log stream closed/);
    await userEvent.click(toggle);
    expect(screen.queryByRole("region", { name: "Build log" })).not.toBeInTheDocument();

    await userEvent.click(toggle);
    const log = await screen.findByRole("region", { name: "Build log" });
    await waitFor(() => {
      expect(log).toHaveTextContent("full log");
    });
    expect(server.sent("GET /runs/run-accepted/log?follow=1")).toHaveLength(2);
  });
});

// The Timeline section, built from `GET /runs/{id}/progress`'s feed
// (progress-contract.md).
describe("Timeline", () => {
  const inProgressFeed = [
    line({
      ts: "2026-09-17T10:00:00.000Z",
      source: "factory",
      stage: "prepare_workspace",
      event: "start",
    }),
    line({
      ts: "2026-09-17T10:00:05.000Z",
      source: "factory",
      stage: "prepare_workspace",
      event: "end",
      outcome: "pass",
    }),
    line({ ts: "2026-09-17T10:00:05.000Z", source: "factory", stage: "preflight", event: "start" }),
  ];

  test("stages advance from pending to running to passed", async () => {
    renderRun(inProgressRun(), { progress: inProgressFeed });

    const prepare = await screen.findByTestId("timeline-row-prepare_workspace");
    const preflight = screen.getByTestId("timeline-row-preflight");
    await waitFor(() => {
      expect(prepare).toHaveTextContent("Passed");
    });
    expect(preflight).toHaveTextContent("Running");
    const list = screen.getByRole("list", { name: "Timeline stages" });
    expect(within(list).getAllByText("Passed")).toHaveLength(1);
    expect(within(list).getAllByText("Running").length).toBeGreaterThan(0);
  });

  // Regression: the whole Timeline once merged into one accessibility node and
  // the glyphs had no label, so a screen reader or Playwright could not tell
  // which stage passed. Each stage is its own list item and names its status.
  test("stages that have not started fold into one row but stay in the list", async () => {
    renderRun(inProgressRun(), { progress: inProgressFeed });
    const list = await screen.findByRole("list", { name: "Timeline stages" });
    const folded = await within(list).findByTestId("timeline-not-started");
    expect(folded).toHaveTextContent(/\d+ later stages not started/);
    // The started stages are listed on their own, outside the fold.
    expect(within(folded).queryByTestId("timeline-row-preflight")).not.toBeInTheDocument();
    expect(folded.querySelector("details")).not.toHaveAttribute("open");
    // Every later stage name is still in the page, inside the fold.
    for (const name of ["Verify", "Full suite", "Gates", "Evidence", "Finished"]) {
      expect(within(folded).getByText(name)).toBeInTheDocument();
    }
  });

  test("each stage row is its own list item that names its status", async () => {
    renderRun(inProgressRun(), { progress: inProgressFeed });

    const list = await screen.findByRole("list", { name: "Timeline stages" });
    const prepare = await within(list).findByTestId("timeline-row-prepare_workspace");
    await waitFor(() => {
      expect(prepare.textContent).toBe("PassedPrepare workspace00:05");
    });
    // A running stage also carries its live elapsed time.
    expect(within(list).getByTestId("timeline-row-preflight").textContent).toMatch(
      /^RunningPreflight\d+:\d\d/,
    );
    expect(within(list).getByTestId("timeline-row-verify").textContent).toBe("PendingVerify");
    for (const item of within(list).getAllByRole("listitem")) {
      expect(item.textContent).not.toBe("");
    }
  });

  test("a worker round line renders under Build", async () => {
    renderRun(inProgressRun(), {
      progress: [
        line({ ts: "2026-09-17T10:00:00.000Z", source: "factory", stage: "build", event: "start" }),
        line({
          ts: "2026-09-17T10:00:01.000Z",
          source: "worker",
          stage: "round",
          event: "start",
          round: 2,
          max_rounds: 6,
        }),
        line({
          ts: "2026-09-17T10:00:10.000Z",
          source: "worker",
          stage: "round",
          event: "end",
          round: 2,
          max_rounds: 6,
          outcome: "fail",
          detail: "verify failed: go test ./...",
        }),
      ],
    });

    expect(
      await screen.findByText(/Round 2 of 6 · verify failed: go test \.\/\.\.\./),
    ).toBeInTheDocument();
    expect(screen.getByText("Round 2/6")).toBeInTheDocument();
  });

  test("agent notes are capped at the last 8, newest at the bottom", async () => {
    const notes = Array.from({ length: 10 }, (_, i) =>
      line({
        ts: `2026-09-17T10:00:0${(i + 1) % 9}.000Z`,
        source: "worker",
        stage: "agent",
        event: "note",
        round: 1,
        detail: `note-${i + 1}`,
      }),
    );
    renderRun(inProgressRun(), {
      progress: [
        line({ ts: "2026-09-17T10:00:00.000Z", source: "factory", stage: "build", event: "start" }),
        line({
          ts: "2026-09-17T10:00:01.000Z",
          source: "worker",
          stage: "round",
          event: "start",
          round: 1,
          max_rounds: 3,
        }),
        ...notes,
      ],
    });

    const region = await screen.findByRole("region", { name: "Agent notes" });
    // Exactly the last 8 of the 10 sent, oldest first: whole lines, since
    // "note-1" is a substring of "note-10".
    expect(region.textContent.split("\n")).toEqual(
      Array.from({ length: 8 }, (_, i) => `note-${i + 3}`),
    );
  });

  test("a replayed progress line is not shown twice", async () => {
    renderRun(inProgressRun(), {
      progress: [
        line({
          ts: "2026-09-17T10:00:01.000Z",
          source: "worker",
          stage: "agent",
          event: "note",
          round: 1,
          detail: "only once",
        }),
        line({
          ts: "2026-09-17T10:00:01.000Z",
          source: "worker",
          stage: "agent",
          event: "note",
          round: 1,
          detail: "only once",
        }),
      ],
    });

    const region = await screen.findByRole("region", { name: "Agent notes" });
    expect(region.textContent).toBe("only once");
  });

  test("a terminal run still renders its recorded Timeline history", async () => {
    renderRun(acceptedRun(), {
      progress: [
        line({
          ts: "2026-08-26T11:00:00.000Z",
          source: "factory",
          stage: "prepare_workspace",
          event: "start",
        }),
        line({
          ts: "2026-08-26T11:00:05.000Z",
          source: "factory",
          stage: "prepare_workspace",
          event: "end",
          outcome: "pass",
        }),
        finishedAccepted,
      ],
    });

    expect(await screen.findByTestId("timeline-row-prepare_workspace")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByTestId("timeline-row-finished")).toBeInTheDocument();
    });
    // A duration is shown for the completed stage (00:05 between the start/end lines).
    expect(screen.getAllByText("00:05")).toHaveLength(1);
  });

  // "Factory build note" (progress-contract.md's "Additions"): a terminal run
  // with evidence rounds renders one factory-authored sub-row per round, and a
  // factory `build`/`note` line renders as the Build row's own subtitle.
  test("a terminal run with evidence rounds renders round rows and the note subtitle", async () => {
    renderRun(
      withEvidence(acceptedRun(), [
        evidenceRound({ index: 1, verify_passed: false, duration_s: 41.0 }),
        evidenceRound({
          index: 2,
          usage: { input: 3800, output: 200 },
          verify_passed: true,
          duration_s: 12.0,
        }),
      ]),
      {
        progress: [
          line({
            ts: "2026-08-26T11:04:00.000Z",
            source: "factory",
            stage: "build",
            event: "note",
            detail: "2 rounds · r1 fail (verify) · r2 pass · 25.2k tokens",
          }),
          { ...finishedAccepted, ts: "2026-08-26T11:05:00.000Z" },
        ],
      },
    );

    expect(await screen.findByText(/Round 1 · fail \(verify\)/)).toBeInTheDocument();
    expect(screen.getByText(/Round 2 · pass/)).toBeInTheDocument();
    expect(
      await screen.findByText("2 rounds · r1 fail (verify) · r2 pass · 25.2k tokens"),
    ).toBeInTheDocument();
  });

  // A round's usage with no totalTokens must still count cacheRead/cacheWrite,
  // not just input/output, so this figure agrees with the cached drafting
  // totals elsewhere in the console (operator demo, 2026-09-26).
  test("a round with no totalTokens counts cacheRead/cacheWrite too", async () => {
    renderRun(
      withEvidence(acceptedRun(), [
        evidenceRound({
          usage: { input: 1000, output: 200, cacheRead: 500, cacheWrite: 300 },
          duration_s: 5.0,
        }),
      ]),
    );

    expect(await screen.findByText(/Round 1 · pass · 2\.0k tokens · 5s/)).toBeInTheDocument();
  });

  // "Silence is a bug": the status strip shows the server-reported
  // waiting_reason in place of the stage label, and a "Last activity" figure.
  // Both come from the run record, independent of the live progress feed, so an
  // empty feed still explains itself.
  test("status strip shows waiting_reason in place of the stage", async () => {
    renderRun(waitingRun("behind 1 run(s) on foo/bar"));

    const strip = await screen.findByTestId("timeline-status-strip");
    expect(within(strip).getByText("behind 1 run(s) on foo/bar")).toBeInTheDocument();
    expect(within(strip).getByText(/Last activity/)).toBeInTheDocument();
  });

  // A non-terminal run whose progress has gone quiet past the threshold shows
  // the shared stalled chip in the strip.
  test("status strip shows a stalled chip for a quiet run", async () => {
    renderRun(inProgressRun());

    const strip = await screen.findByTestId("timeline-status-strip");
    expect(within(strip).getByText("stalled")).toBeInTheDocument();
  });

  // Live bug (operator walk, 2026-09-26, Temporal path): the built-in policy
  // gates never emit their own `gate` progress events, so a run with no named
  // gates showed "Gates" as skipped though gate_results recorded every check
  // passed. The Gates row falls back to gate_results when the feed has none.
  test("Gates row renders passed from gate_results when the feed has no gate events", async () => {
    const checks = [
      "canonical_verify",
      "diff_scope",
      "required_files_changed",
      "tests_added",
      "full_suite_verify",
      "spec_conformity",
    ];
    renderRun(
      {
        ...acceptedRun(),
        gate_results: checks.map((check) => ({
          check,
          command: ["make", "verify"],
          passed: true,
          exit_code: 0,
          duration_ms: 1000,
          log_sha256: `hash-${check}`,
        })),
      },
      { progress: [finishedAccepted] },
    );

    const row = await screen.findByTestId("timeline-row-gate");
    expect(within(row).getByText("Passed")).toBeInTheDocument();
    expect(
      within(row).getByText(
        "6 passed: canonical_verify, diff_scope, required_files_changed, tests_added, full_suite_verify, spec_conformity",
      ),
    ).toBeInTheDocument();
  });

  test("Gates row renders failed from gate_results when one check failed", async () => {
    renderRun(
      {
        ...quarantinedRun(),
        gate_results: [
          {
            check: "canonical_verify",
            command: ["make", "verify"],
            passed: true,
            exit_code: 0,
            duration_ms: 1000,
            log_sha256: "hash-canonical_verify",
          },
          {
            check: "tests_added",
            command: ["make", "verify"],
            passed: false,
            exit_code: 1,
            duration_ms: 500,
            log_sha256: "hash-tests_added",
          },
        ],
      },
      {
        progress: [
          line({
            ts: "2026-08-26T10:03:00.000Z",
            source: "factory",
            stage: "finished",
            event: "end",
            outcome: "quarantined",
          }),
        ],
      },
    );

    const row = await screen.findByTestId("timeline-row-gate");
    expect(within(row).getByText("Failed")).toBeInTheDocument();
    expect(
      within(row).getByText("1 failed of 2: canonical_verify (pass), tests_added (fail)"),
    ).toBeInTheDocument();
  });

  // An in-progress run with neither gate events nor recorded gate_results keeps
  // the pending behaviour: the fallback must not fire before the built-in
  // gates have actually run.
  test("Gates row stays pending when neither gate events nor gate_results exist", async () => {
    renderRun(inProgressRun());

    const row = await screen.findByTestId("timeline-row-gate");
    expect(within(row).getByText("Pending")).toBeInTheDocument();
  });

  test("a permanent progress feed failure is shown inside the Timeline", async () => {
    renderRun(acceptedRun(), {
      routes: [
        { on: "GET /runs/run-accepted/progress", reply: apiErrorResponse(404, "progress gone") },
      ],
    });

    expect(await screen.findByText("Not found")).toBeInTheDocument();
    expect(screen.getByTestId("timeline-row-prepare_workspace")).toBeInTheDocument();
  });
});

// A run recorded against a request links back to it.
describe("request linkage", () => {
  test("open-request link appears and navigates when requestId is set", async () => {
    const { location } = renderRun(
      { ...acceptedRun(), request_id: "request-1" },
      {
        routes: [
          {
            on: "GET /requests/request-1",
            reply: json({
              id: "request-1",
              workspace: "/workspaces/request-1",
              project: "app",
              state: "done",
              submitted_at: "2026-08-26T10:00:00Z",
              updated_at: "2026-08-26T11:04:00Z",
              title: "Add the widget",
            }),
          },
        ],
      },
    );

    const link = await screen.findByRole("link", { name: "Open request" });
    expect(await screen.findByText("Add the widget")).toBeInTheDocument();

    await userEvent.click(link);
    expect(await screen.findByText("Navigated to /requests/request-1")).toBeInTheDocument();
    expect(location()).toBe("/requests/request-1");
  });

  test("open-request link is absent when the run has no requestId", async () => {
    renderRun(acceptedRun());

    await screen.findByText("ticket-accepted");
    expect(screen.queryByRole("link", { name: "Open request" })).not.toBeInTheDocument();
  });

  test("a failed request fetch leaves the link and the ticket in place", async () => {
    renderRun({ ...acceptedRun(), request_id: "request-1" });

    expect(await screen.findByRole("link", { name: "Open request" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "ticket-accepted" })).toBeInTheDocument();
  });
});

// The raw docker argv is collapsed by default, expandable on activation.
test("attempt command is collapsed by default and expandable", async () => {
  renderRun(acceptedRun());

  const button = await screen.findByRole("button", { name: "Command" });
  expect(button).toHaveAttribute("aria-expanded", "false");
  expect(screen.queryByText(/python3 build_app.py ticket-accepted/)).not.toBeInTheDocument();

  await userEvent.click(button);

  expect(screen.getByText(/python3 build_app.py ticket-accepted/)).toBeInTheDocument();
});

// An attempt's log path is an in-app "Open log" action into the existing log
// pane, not plain unclickable text.
test('attempt "Open log" enables the existing log pane', async () => {
  renderRun(acceptedRun(), {
    routes: [{ on: "GET /runs/run-accepted/log?follow=1", reply: () => new Response("log text") }],
  });

  await userEvent.click(await screen.findByRole("button", { name: "Open log" }));

  expect(await screen.findByRole("region", { name: "Build log" })).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: /Show live build log/ })).toBeChecked();
});

// "Open in Temporal UI" only appears when both the server config and the run
// itself carry the necessary fields.
describe("Temporal UI link", () => {
  test("appears when temporalUiUrl and temporalWorkflowId are both set", async () => {
    renderRun(
      { ...acceptedRun(), temporal_workflow_id: "wf 123" },
      { config: { temporalUiUrl: "http://temporal.test" } },
    );

    const link = await screen.findByRole("link", { name: "Open in Temporal UI" });
    expect(link).toHaveAttribute(
      "href",
      "http://temporal.test/namespaces/default/workflows/wf%20123",
    );
  });

  test("is absent when temporalWorkflowId is empty", async () => {
    renderRun(acceptedRun(), { config: { temporalUiUrl: "http://temporal.test" } });

    await screen.findByText("ticket-accepted");
    expect(screen.queryByRole("link", { name: "Open in Temporal UI" })).not.toBeInTheDocument();
  });

  test("is absent when the server advertises no Temporal UI", async () => {
    renderRun({ ...acceptedRun(), temporal_workflow_id: "wf-123" });

    await screen.findByText("ticket-accepted");
    expect(screen.queryByRole("link", { name: "Open in Temporal UI" })).not.toBeInTheDocument();
  });
});

// A terminal run's evidence-round row ("Round N · outcome · tokens · duration")
// is not duplicated by the worker-relayed "Round N of M · outcome" line for the
// same round index.
test("evidence round line is not duplicated by the worker-relayed round line", async () => {
  renderRun(withEvidence(acceptedRun(), [evidenceRound()]), {
    progress: [
      line({
        ts: "2026-08-26T11:00:00.000Z",
        source: "worker",
        stage: "round",
        event: "start",
        round: 1,
        max_rounds: 3,
      }),
      line({
        ts: "2026-08-26T11:00:41.000Z",
        source: "worker",
        stage: "round",
        event: "end",
        round: 1,
        max_rounds: 3,
        outcome: "pass",
      }),
      finishedAccepted,
    ],
  });

  // The evidence-authored line is shown once...
  expect(await screen.findByText(/Round 1 · pass/)).toBeInTheDocument();
  await waitFor(() => {
    expect(screen.getByTestId("timeline-row-finished")).toHaveTextContent("Passed");
  });
  // ...and the worker-relayed "Round 1 of 3 · pass" duplicate for the same index is gone.
  expect(screen.queryByText(/Round 1 of 3/)).not.toBeInTheDocument();
});

test("a run that cannot be loaded shows the error with Retry", async () => {
  const { server } = renderApp(<RunDetailScreen />, {
    path: "/runs/run-gone",
    pattern,
    server: [
      { on: "GET /runs/run-gone", reply: apiErrorResponse(404, "run not found") },
      { on: "GET /runs/run-gone/progress", reply: sseResponse("progress") },
    ],
  });

  expect(await screen.findByText("Not found")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Run detail" })).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => {
    expect(server.sent("GET /runs/run-gone").length).toBeGreaterThan(1);
  });
});
