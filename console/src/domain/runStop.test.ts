import { innermostCause, runStop, stopText } from "@/domain/runStop";

// A real halt_error, recorded when a target repository's compose file named
// an image outside the allowed registries.
const wrapped =
  'workflow execution error (type: RunWorkflow, workflowID: add-a-liveness-endpoint-001, runID: 01a10f9f): run build activity: activity error (type: RunBuildActivity, scheduledEventID: 31, startedEventID: 32, identity: 80084@host@): build subprocess infrastructure failure (type: ComposeServicesRejected, retryable: true): compose file rejected: kafka: image "apache/kafka:3.8.0" is not under an allow-listed registry/namespace -- add "docker.io/apache/" to compose_services_allowed_registries, or use an image under one already listed; no compose service was launched and the build did not start -- fix the target repo\'s compose file (or the operator\'s compose_services_* settings) and retry (type: wrapError, retryable: true): compose file rejected (type: ComposeServicesRejected, retryable: true)';

const cause =
  'compose file rejected: kafka: image "apache/kafka:3.8.0" is not under an allow-listed registry/namespace -- add "docker.io/apache/" to compose_services_allowed_registries, or use an image under one already listed; no compose service was launched and the build did not start -- fix the target repo\'s compose file (or the operator\'s compose_services_* settings) and retry';

test("the innermost cause is the sentence inside the workflow wrappers", () => {
  expect(innermostCause(wrapped)).toBe(cause);
});

test("a plain message is its own cause", () => {
  expect(innermostCause("verify failed after 3 rounds")).toBe("verify failed after 3 rounds");
  expect(innermostCause("required_files_changed: internal/domain/domain.go untouched")).toBe(
    "required_files_changed: internal/domain/domain.go untouched",
  );
  expect(innermostCause("context canceled")).toBe("context canceled");
  expect(innermostCause("")).toBe("");
});

test("a parenthesis that is not a wrapper is left alone", () => {
  expect(innermostCause("gate failed (see log): exit 2")).toBe("gate failed (see log): exit 2");
});

test("a stopped run reports its code, cause, full error and triage", () => {
  expect(
    runStop({
      state: "halted",
      haltError: wrapped,
      haltReasonCode: "compose_services_rejected",
      triage: "",
    }),
  ).toEqual({ code: "compose_services_rejected", cause, full: wrapped, triage: "" });
  expect(
    runStop({
      state: "quarantined",
      haltError: "verify failed after 3 rounds",
      haltReasonCode: "verify_failed",
      triage: "The verify command failed on TestKeyScopedToAccount in every round.",
    }),
  ).toEqual({
    code: "verify_failed",
    cause: "verify failed after 3 rounds",
    full: "",
    triage: "The verify command failed on TestKeyScopedToAccount in every round.",
  });
});

test("a run that did not stop, or stopped with nothing recorded, has nothing to show", () => {
  expect(runStop({ state: "accepted", haltError: "", haltReasonCode: "", triage: "" })).toBeNull();
  expect(
    runStop({ state: "slice_running", haltError: "stale", haltReasonCode: "x", triage: "" }),
  ).toBeNull();
  expect(runStop({ state: "halted", haltError: "", haltReasonCode: "", triage: "" })).toBeNull();
});

test("a request's stop reason splits into the summary and the innermost cause", () => {
  const summary =
    "ticket 1/1 halted: halted: the target repo's compose file was rejected before the build; fix the named services or compose_services_* config and retry";
  expect(stopText(`${summary} (${wrapped})`)).toEqual({
    summary,
    cause,
    full: `${summary} (${wrapped})`,
  });
});

test("a stop reason with no wrapped error is all summary", () => {
  expect(
    stopText("ticket 1/1 quarantined: required_files_changed: internal/domain/domain.go untouched"),
  ).toEqual({
    summary: "ticket 1/1 quarantined: required_files_changed: internal/domain/domain.go untouched",
    cause: "",
    full: "",
  });
});
