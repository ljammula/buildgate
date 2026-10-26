import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { readBackAnswersKey, sha256Hex } from "@/domain/contentHash";
import { apiErrorResponse, json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire } from "./testRequests";

beforeEach(seedOperator);

const readBack = (answer = "", q2Answer = "") => ({
  summary: "Makes POST /refunds idempotent by key.",
  questions: [
    {
      id: "Q1",
      text: "How long is a key kept?",
      ...(answer === ""
        ? {}
        : { answer, answered_by: "operator", answered_at: "2026-10-25T10:00:00Z" }),
    },
    {
      id: "Q2",
      text: "<b>Scoped per merchant?</b>",
      ...(q2Answer === ""
        ? {}
        : { answer: q2Answer, answered_by: "bob", answered_at: "2026-10-25T10:01:00Z" }),
    },
  ],
});

const specReview = (answer = "", q2Answer = "") =>
  requestWire({
    state: "spec_review",
    title: "Refunds",
    spec: "# Spec\n\nRefunds.\n",
    spec_imported: true,
    read_back: readBack(answer, q2Answer),
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
    question_sha256: sha256Hex("How long is a key kept?"),
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

// Saving Q1 returns a record where someone else has since answered Q2.
const answeredUnderneath = () => ({
  extra: [
    {
      on: "PUT /requests/req-1/read-back/answers/Q1",
      reply: json(specReview("24 hours.", "Per merchant.")),
    },
  ],
});

const answerBox = (id: string) =>
  within(screen.getByTestId(`read-back-question-${id}`)).getByRole("textbox", {
    name: `Answer to ${id}`,
  });

async function saveQ1() {
  const q1 = within(await screen.findByTestId("read-back-question-Q1"));
  await userEvent.type(q1.getByRole("textbox", { name: "Answer to Q1" }), "24 hours.");
  await userEvent.click(q1.getByRole("button", { name: "Save answer" }));
}

test("an answer box with no unsaved text starts over when its stored answer changes underneath it", async () => {
  openRequest(specReview(), answeredUnderneath());
  await saveQ1();
  await waitFor(() => {
    expect(answerBox("Q2")).toHaveValue("Per merchant.");
  });
  expect(answerBox("Q1")).toHaveValue("24 hours.");
  const q2 = within(screen.getByTestId("read-back-question-Q2"));
  expect(q2.getByText("Answered by bob.")).toBeInTheDocument();
  expect(q2.queryByRole("status")).toBeNull();
  expect(within(screen.getByTestId("read-back-question-Q1")).queryByRole("status")).toBeNull();
});

test("an answer box keeps unsaved text when its stored answer changes underneath it, and shows the new answer", async () => {
  openRequest(specReview(), answeredUnderneath());
  await screen.findByTestId("read-back-question-Q2");
  await userEvent.type(answerBox("Q2"), "my unsaved text");
  await saveQ1();
  const q2 = within(screen.getByTestId("read-back-question-Q2"));
  const notice = await q2.findByRole("status");
  expect(answerBox("Q2")).toHaveValue("my unsaved text");
  expect(notice).toHaveTextContent("The stored answer changed while you were typing (by bob)");
  expect(q2.getByTestId("read-back-stored-Q2")).toHaveTextContent("Per merchant.");
  expect(notice).toHaveTextContent("Your text is kept and not saved.");
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  const dialog = within(await screen.findByRole("dialog"));
  expect(dialog.getByText(/Unsaved answer to Q2: not part of this approval/)).toBeInTheDocument();
});

test("an answer box keeps unsaved text when its question is replaced underneath it, and says so", async () => {
  const first = readBack("24 hours.").questions[0];
  const replaced = requestWire({
    state: "spec_review",
    title: "Refunds",
    spec: "# Spec\n\nRefunds.\n",
    spec_imported: true,
    read_back: {
      summary: "Makes POST /refunds idempotent by key.",
      questions: [first, { id: "Q2", text: "Scoped per account?" }],
    },
  });
  openRequest(specReview(), {
    extra: [{ on: "PUT /requests/req-1/read-back/answers/Q1", reply: json(replaced) }],
  });
  await screen.findByTestId("read-back-question-Q2");
  await userEvent.type(answerBox("Q2"), "my unsaved text");
  await saveQ1();
  const q2 = within(screen.getByTestId("read-back-question-Q2"));
  expect(await q2.findByRole("status")).toHaveTextContent(
    "This question changed while you were typing: read it again.",
  );
  expect(answerBox("Q2")).toHaveValue("my unsaved text");
  expect(q2.getByText(/Scoped per account\?/)).toBeInTheDocument();
});

test("Approve warns about an answer box with unsaved text and binds the answers shown", async () => {
  const { server } = openRequest(specReview("24 hours."), {
    extra: [{ on: "POST /requests/req-1/approve", reply: json(specReview("24 hours.")) }],
  });
  const q2 = within(await screen.findByTestId("read-back-question-Q2"));
  await userEvent.type(q2.getByRole("textbox", { name: "Answer to Q2" }), "not saved");
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  const dialog = within(await screen.findByRole("dialog"));
  expect(dialog.getByText(/Unsaved answer to Q2: not part of this approval/)).toBeInTheDocument();
  await userEvent.click(dialog.getByRole("button", { name: "Approve" }));
  await waitFor(() => {
    expect(server.sent("POST /requests/req-1/approve")).toHaveLength(1);
  });
  const textBytes = new TextEncoder().encode("How long is a key kept?").length;
  const shown = sha256Hex(`Q1\n${textBytes}\nHow long is a key kept?\n9\n24 hours.\n`);
  expect(server.sent("POST /requests/req-1/approve")[0]?.body).toEqual({
    by: "operator",
    expected_sha256: { "spec.md": sha256Hex("# Spec\n\nRefunds.\n"), [readBackAnswersKey]: shown },
  });
});

test("Approve says nothing about answers when every box is saved", async () => {
  openRequest(specReview("24 hours."));
  await screen.findByTestId("read-back");
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  const dialog = within(await screen.findByRole("dialog"));
  expect(dialog.queryByText(/Unsaved answer/)).toBeNull();
});

test("the audit lists who answered which read-back question when, without the text", async () => {
  openRequest({
    ...specReview("24 hours."),
    read_back_answer_log: [
      { question_id: "Q1", action: "answered", by: "alice", at: "2026-10-25T10:00:00Z" },
      { question_id: "Q1", action: "cleared", by: "bob", at: "2026-10-25T10:05:00Z" },
    ],
  });
  const log = within(await screen.findByTestId("audit-read-back-answers"));
  expect(log.getByText(/Read-back Q1 answered by alice at/)).toBeInTheDocument();
  expect(log.getByText(/Read-back Q1 cleared by bob at/)).toBeInTheDocument();
  expect(log.queryByText(/24 hours/)).toBeNull();
});
