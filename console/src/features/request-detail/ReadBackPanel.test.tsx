import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { apiErrorResponse, json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire } from "./testRequests";

beforeEach(seedOperator);

const readBack = (answer = "") => ({
  summary: "Makes POST /refunds idempotent by key.",
  questions: [
    {
      id: "Q1",
      text: "How long is a key kept?",
      ...(answer === "" ? {} : { answer, answered_by: "operator" }),
    },
    { id: "Q2", text: "<b>Scoped per merchant?</b>" },
  ],
});

const specReview = (answer = "") =>
  requestWire({
    state: "spec_review",
    title: "Refunds",
    spec_imported: true,
    read_back: readBack(answer),
  });

test("a handed-over spec under review shows its read-back as text, with the answers going to the build", async () => {
  openRequest(specReview());
  const panel = within(await screen.findByTestId("read-back"));
  expect(panel.getByText("Makes POST /refunds idempotent by key.")).toBeInTheDocument();
  expect(panel.getByText(/go to every ticket's build/)).toBeInTheDocument();
  expect(panel.getByText(/written by the model/)).toBeInTheDocument();
  // Model-written text is never rendered as HTML.
  expect(panel.getByText("Q2: <b>Scoped per merchant?</b>")).toBeInTheDocument();
  expect(panel.getAllByRole("textbox")).toHaveLength(2);
});

test("saving an answer PUTs it for that question alone", async () => {
  const { server } = openRequest(specReview(), {
    extra: [
      { on: "PUT /requests/req-1/read-back/answers/Q1", reply: json(specReview("24 hours.")) },
    ],
  });
  const question = within(await screen.findByTestId("read-back-question-Q1"));
  await userEvent.type(question.getByRole("textbox", { name: "Answer to Q1" }), "24 hours.");
  await userEvent.click(question.getByRole("button", { name: "Save answer" }));
  await waitFor(() => {
    expect(server.sent("PUT /requests/req-1/read-back/answers/Q1")).toHaveLength(1);
  });
  expect(server.sent("PUT /requests/req-1/read-back/answers/Q1")[0]?.body).toEqual({
    answer: "24 hours.",
    by: "operator",
  });
  expect(server.sent("PUT /requests/req-1/read-back/answers/Q2")).toHaveLength(0);
  expect(await question.findByText("Answered by operator.")).toBeInTheDocument();
});

test("a refused save shows the server's reason", async () => {
  openRequest(specReview(), {
    extra: [
      {
        on: "PUT /requests/req-1/read-back/answers/Q1",
        reply: apiErrorResponse(409, "read-back questions are answered only in spec_review"),
      },
    ],
  });
  const question = within(await screen.findByTestId("read-back-question-Q1"));
  await userEvent.type(question.getByRole("textbox", { name: "Answer to Q1" }), "x");
  await userEvent.click(question.getByRole("button", { name: "Save answer" }));
  expect(await question.findByRole("alert")).toBeInTheDocument();
});

test("a spec review with no read-back draws no panel", async () => {
  openRequest(requestWire({ state: "spec_review", title: "Plain" }));
  await screen.findByRole("heading", { level: 1, name: "Plain" });
  expect(screen.queryByTestId("read-back")).toBeNull();
});

test("an unavailable read-back draws no panel", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "Unavailable",
      read_back: { unavailable: "the job did not finish" },
    }),
  );
  await screen.findByRole("heading", { level: 1, name: "Unavailable" });
  expect(screen.queryByTestId("read-back")).toBeNull();
});

test("without write access the answer boxes are disabled", async () => {
  openRequest(specReview(), { writes: false });
  const question = within(await screen.findByTestId("read-back-question-Q1"));
  expect(question.getByRole("textbox", { name: "Answer to Q1" })).toBeDisabled();
  expect(question.getByRole("button", { name: "Save answer" })).toBeDisabled();
});
