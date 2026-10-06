import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { RunDetailScreen } from "@/features/run-detail/RunDetailScreen";
import {
  acceptedRun,
  evidenceRound,
  inProgressRun,
  quarantinedRun,
  withEvidence,
} from "@/features/run-detail/testRuns";
import { type FakeRoute, json, renderApp, sseResponse } from "@/test/render";

beforeAll(() => {
  Object.assign(Element.prototype, {
    hasPointerCapture: () => false,
    scrollIntoView: () => undefined,
  });
});

type Wire = Record<string, unknown>;

function renderRun(run: Wire, routes: readonly FakeRoute[] = []) {
  const id = run.id as string;
  return renderApp(<RunDetailScreen />, {
    path: `/runs/${id}`,
    pattern: "/runs/:id",
    server: [
      { on: `GET /runs/${id}`, reply: json(run) },
      { on: `GET /runs/${id}/events`, reply: sseResponse("state") },
      { on: `GET /runs/${id}/progress`, reply: sseResponse("progress", []) },
      ...routes,
    ],
  });
}

const gate = (check: string, passed: boolean): Wire => ({
  check,
  command: ["make", check],
  passed,
  exit_code: passed ? 0 : 2,
  duration_ms: 4200,
  log_sha256: "cd".repeat(32),
});

function detailsOf(title: string): HTMLDetailsElement {
  const heading = screen.getByRole("heading", { name: title });
  const el = heading.closest("details");
  if (el === null) throw new Error(`${title} is not a disclosure`);
  return el;
}

const finished = (over: Wire = {}): Wire => ({
  ...withEvidence(acceptedRun(), [evidenceRound()]),
  gate_results: [gate("verify", true), gate("lint", true)],
  by_model: [{ model: "gpt-5.6-luna", tokens: 478300 }],
  created_at: "2026-08-26T11:00:00Z",
  updated_at: "2026-08-26T11:20:00Z",
  ...over,
});

describe("a finished run", () => {
  test("opens with one summary line: state, rounds, gates, tokens, duration", async () => {
    renderRun(finished());

    expect(await screen.findByTestId("run-verdict")).toHaveTextContent(
      "Accepted · 1 round · 2 gates passed · 478.3k tokens · 20:00",
    );
  });

  test("keeps attempts, gates and compose services closed, each with a one-line summary", async () => {
    renderRun(
      finished({
        compose_phases: [
          {
            phase: "build",
            enabled: true,
            services: [{ name: "redis", alias: "redis", image: "redis:7", port: 6379 }],
          },
        ],
      }),
    );
    await screen.findByTestId("run-verdict");

    expect(detailsOf("Attempts").open).toBe(false);
    expect(within(detailsOf("Attempts")).getByText("1 attempt · all exited 0")).toBeInTheDocument();
    expect(detailsOf("Gate results").open).toBe(false);
    expect(
      within(detailsOf("Gate results")).getByText("2 gates passed: verify, lint"),
    ).toBeInTheDocument();
    expect(detailsOf("Compose services").open).toBe(false);
    expect(within(detailsOf("Compose services")).getByText("build: redis")).toBeInTheDocument();
  });

  test("a closed block opens in place and shows its rows", async () => {
    renderRun(finished());
    await screen.findByTestId("run-verdict");

    await userEvent.click(screen.getByRole("heading", { name: "Gate results" }));

    expect(detailsOf("Gate results").open).toBe(true);
    expect(within(detailsOf("Gate results")).getByTestId("gate-verify")).toBeVisible();
  });

  test("a failed gate opens its block and puts the failure first, ahead of the Timeline", async () => {
    renderRun(
      finished({
        state: "halted",
        halt_confirmed: true,
        gate_results: [gate("verify", true), gate("lint", false)],
      }),
    );
    await screen.findByTestId("run-verdict");

    const gates = detailsOf("Gate results");
    expect(gates.open).toBe(true);
    expect(gates).toHaveAttribute("data-failed", "true");
    expect(within(gates).getByText("failed · exit code 2")).toBeInTheDocument();
    const rows = within(gates).getAllByRole("listitem");
    expect(rows[0]).toHaveAttribute("data-testid", "gate-lint");
    expect(screen.getByTestId("run-verdict")).toHaveAttribute("data-failed", "true");
    const timeline = screen.getByRole("heading", { name: "Timeline" });
    expect(gates.compareDocumentPosition(timeline) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // A passing run keeps its other blocks closed.
    expect(detailsOf("Attempts").open).toBe(false);
  });

  test("a non-zero attempt exit opens the attempts block", async () => {
    const run = finished();
    const attempt = (run.attempts as Wire[])[0] as Wire;
    renderRun({ ...run, attempts: [{ ...attempt, exit_code: 3 }] });
    await screen.findByTestId("run-verdict");

    expect(detailsOf("Attempts").open).toBe(true);
    expect(within(detailsOf("Attempts")).getByText("1 attempt failed of 1")).toBeInTheDocument();
  });

  test("a halted run with no gates still opens with a failure-coloured line", async () => {
    renderRun({ ...quarantinedRun() });

    expect(await screen.findByTestId("run-verdict")).toHaveAttribute("data-failed", "true");
  });
});

describe("a live run", () => {
  test("shows no summary line and no block that has nothing in it yet", async () => {
    renderRun(inProgressRun());
    await screen.findByRole("heading", { name: "Timeline" });

    expect(screen.queryByTestId("run-verdict")).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Attempts" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Gate results" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Compose services" })).not.toBeInTheDocument();
    for (const text of ["None", "Not available", "Not collected"]) {
      expect(screen.queryByText(text)).not.toBeInTheDocument();
    }
    expect(screen.queryByText("Commit and artifact evidence")).toBeInTheDocument();
    expect(screen.queryByText("Changed files")).not.toBeInTheDocument();
  });

  test("keeps the build log behind its own switch, off", async () => {
    renderRun(inProgressRun());

    expect(await screen.findByRole("switch", { name: /Show live build log/ })).not.toBeChecked();
  });
});

describe("the facts card", () => {
  test("merges run, commit evidence and changed files into one card with one usage figure", async () => {
    renderRun(finished());
    await screen.findByTestId("run-verdict");

    const card = screen.getByRole("heading", { name: "Run" }).closest("section");
    if (card === null) throw new Error("no Run card");
    expect(
      within(card).getByRole("heading", { name: "Commit and artifact evidence" }),
    ).toBeInTheDocument();
    expect(within(card).getByRole("heading", { name: "Changed files" })).toBeInTheDocument();
    expect(within(card).getByText("478.3k tokens")).toBeInTheDocument();
    expect(within(card).getByText("lib/app.dart")).toBeInTheDocument();
    expect(within(card).getByText("2 files +18 −3")).toBeInTheDocument();
    expect(within(card).getByText("committed by factoryd")).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Changed files", level: 2 }),
    ).not.toBeInTheDocument();
  });

  test("the per-model breakdown is a closed disclosure", async () => {
    renderRun(finished());
    await screen.findByTestId("run-verdict");

    const breakdown = screen.getByRole("heading", { name: "By model" }).closest("details");
    expect(breakdown?.open).toBe(false);
    expect(
      within(breakdown as HTMLElement).getByText("gpt-5.6-luna · 478.3k tokens"),
    ).toBeInTheDocument();
  });

  test("ages are relative with the exact local time on hover", async () => {
    renderRun(finished());
    await screen.findByTestId("run-verdict");

    const card = screen.getByRole("heading", { name: "Run" }).closest("section") as HTMLElement;
    const updated = within(card).getByText(/ago$/);
    expect(updated.tagName).toBe("TIME");
    expect(updated).toHaveAttribute("dateTime", "2026-08-26T11:20:00Z");
    expect(updated.getAttribute("title")).toMatch(/^2026-08-26 \d\d:20:00$/);
  });
});
