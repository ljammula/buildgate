import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { asObject } from "@/domain/decode";
import { type Run, decodeRun } from "@/domain/run";
import { RunStopCallout } from "@/features/run-detail/RunStopCallout";
import { readFixtureJson } from "@/test/fixtures";

function run(name: string, patch: Record<string, unknown> = {}): Run {
  const raw = { ...asObject(readFixtureJson(`api/${name}`), name), ...patch };
  return decodeRun(raw, name);
}

test("a quarantined run says why: triage, cause and code", () => {
  render(<RunStopCallout run={run("run-quarantined.json")} />);
  const callout = screen.getByTestId("run-stop");
  expect(callout).toHaveTextContent("Why this run stopped");
  expect(callout).toHaveTextContent(
    "The verify command failed on TestKeyScopedToAccount in every round.",
  );
  expect(callout).toHaveTextContent("verify failed after 3 rounds");
  expect(callout).toHaveTextContent("Reason code: verify_failed");
  expect(screen.queryByText("Full error")).not.toBeInTheDocument();
});

test("a wrapped workflow error shows its innermost cause, with the whole error one click away", async () => {
  const wrapped =
    'workflow execution error (type: RunWorkflow, workflowID: w): run build activity: activity error (type: RunBuildActivity, identity: x): build subprocess infrastructure failure (type: ComposeServicesRejected, retryable: true): compose file rejected: kafka: image "apache/kafka:3.8.0" is not under an allow-listed registry/namespace -- add "docker.io/apache/" to compose_services_allowed_registries (type: wrapError, retryable: true): compose file rejected';
  render(
    <RunStopCallout
      run={run("run-quarantined.json", {
        state: "halted",
        halt_error: wrapped,
        halt_reason_code: "compose_services_rejected",
        triage: "",
      })}
    />,
  );
  expect(
    screen.getByText(
      'compose file rejected: kafka: image "apache/kafka:3.8.0" is not under an allow-listed registry/namespace -- add "docker.io/apache/" to compose_services_allowed_registries',
    ),
  ).toBeInTheDocument();
  await userEvent.click(screen.getByText("Full error"));
  expect(screen.getByText(wrapped)).toBeVisible();
});

test("a run that did not stop shows nothing", () => {
  const { container } = render(<RunStopCallout run={run("run-accepted.json")} />);
  expect(container).toBeEmptyDOMElement();
});

test("hostile text in the cause is shown as text", () => {
  const { container } = render(
    <RunStopCallout
      run={run("run-quarantined.json", {
        halt_error: '<img src=x onerror="alert(1)">',
        triage: "",
      })}
    />,
  );
  expect(container.querySelector("img")).toBeNull();
  expect(screen.getByText('<img src=x onerror="alert(1)">')).toBeInTheDocument();
});
