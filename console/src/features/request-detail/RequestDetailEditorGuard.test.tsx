// An open editor holds the operator's own text: a newer record, a navigation
// or a keystroke must never lose it silently.
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { sha256Hex } from "@/domain/contentHash";
import { json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { type Wire, requestWire } from "./testRequests";

beforeEach(seedOperator);

const ORIGINAL = "# Spec\n\nOriginal detail.";
const specPanel = () => screen.getByRole("region", { name: "Spec" });
const editor = () => screen.getByRole("textbox", { name: "Edit spec.md" });
const approveButton = () => screen.getByRole("button", { name: "Approve" });

const specReview = (spec = ORIGINAL, rest: Record<string, unknown> = {}): Wire =>
  requestWire({ state: "spec_review", title: "Add idempotency keys", spec, ...rest });

async function openEditor(body: { current: Wire }) {
  const view = openRequest(() => body.current, {
    extra: [{ on: "PUT /requests/req-1/spec", reply: json(specReview("saved")) }],
  });
  await screen.findByRole("heading", { level: 1, name: "Add idempotency keys" });
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  await userEvent.click(within(specPanel()).getByRole("button", { name: "Edit" }));
  return view;
}

async function typeIn(text: string) {
  await userEvent.clear(editor());
  await userEvent.type(editor(), text);
}

async function refresh() {
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
}

describe("a newer record arriving under an editor", () => {
  test("never discards unsaved text: it says the request changed, turns Save off and keeps the text copyable", async () => {
    const body = { current: specReview() };
    const view = await openEditor(body);
    await typeIn("my careful paragraph");

    body.current = requestWire({
      state: "planning",
      title: "Add idempotency keys",
      spec: ORIGINAL,
      updated_at: "2026-09-10T09:10:00Z",
    });
    await refresh();

    const notice = await screen.findByTestId("edit-changed-notice");
    expect(notice).toHaveTextContent(
      "This request changed while you were editing: it is now Planning.",
    );
    expect(editor()).toHaveValue("my careful paragraph");
    expect(editor()).toHaveAttribute("readonly");
    expect(within(specPanel()).getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.getByTestId("edit-blocked")).toHaveTextContent("Save is off");
    expect(screen.getByRole("button", { name: "Copy my text" })).toBeInTheDocument();
    expect(view.server.sent("PUT /requests/req-1/spec")).toHaveLength(0);

    // Keep editing (the default) answers the notice and leaves the text and the reason.
    await userEvent.click(within(notice).getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByTestId("edit-changed-notice")).not.toBeInTheDocument();
    expect(editor()).toHaveValue("my careful paragraph");
    expect(screen.getByTestId("edit-blocked")).toBeInTheDocument();
  });

  test("Discard my changes closes the editor", async () => {
    const body = { current: specReview() };
    await openEditor(body);
    await typeIn("typed");
    body.current = requestWire({
      state: "planning",
      title: "Add idempotency keys",
      spec: ORIGINAL,
      updated_at: "2026-09-10T09:10:00Z",
    });
    await refresh();

    await userEvent.click(await screen.findByRole("button", { name: "Discard my changes" }));
    expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
  });

  test("a changed file in the same state keeps the editor, and Save still sends the hash the edit started from", async () => {
    const body = { current: specReview() };
    const view = await openEditor(body);
    await typeIn("# Spec\n\nMine.");

    body.current = specReview("# Spec\n\nSomeone else's detail.", {
      updated_at: "2026-09-10T09:10:00Z",
    });
    await refresh();

    const notice = await screen.findByTestId("edit-changed-notice");
    expect(notice).toHaveTextContent("Save will show you the difference.");
    expect(editor()).toHaveValue("# Spec\n\nMine.");
    await userEvent.click(within(specPanel()).getByRole("button", { name: "Save" }));
    await waitFor(() => {
      expect(view.server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
    });
    expect(view.server.sent("PUT /requests/req-1/spec")[0]?.body).toEqual({
      content: "# Spec\n\nMine.",
      base_sha256: sha256Hex(ORIGINAL),
    });
  });

  test("an editor with nothing unsaved still closes when its file stops being editable", async () => {
    const body = { current: specReview() };
    await openEditor(body);
    body.current = requestWire({
      state: "planning",
      title: "Add idempotency keys",
      spec: ORIGINAL,
      updated_at: "2026-09-10T09:10:00Z",
    });
    await refresh();
    await waitFor(() => {
      expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
    });
  });

  test("Approve and Request changes stay disabled while the editor is open", async () => {
    await openEditor({ current: specReview() });
    expect(approveButton()).toBeDisabled();
    expect(screen.getByRole("button", { name: "Request changes" })).toBeDisabled();
  });
});

describe("leaving with unsaved text", () => {
  test("a link asks first; Stay keeps the text, Leave and discard goes", async () => {
    const view = await openEditor({ current: specReview() });
    await typeIn("unsaved");

    await userEvent.click(screen.getByRole("link", { name: "Back to board" }));
    const dialog = await screen.findByRole("dialog", { name: "Leave this page?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Stay and keep editing" }));
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(editor()).toHaveValue("unsaved");
    expect(view.location()).toBe("/requests/req-1");

    await userEvent.click(screen.getByRole("link", { name: "Back to board" }));
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Leave and discard" }),
    );
    await waitFor(() => {
      expect(view.location()).toBe("/");
    });
  });

  test("a link does not ask when nothing was changed", async () => {
    const view = await openEditor({ current: specReview() });
    await userEvent.click(screen.getByRole("link", { name: "Back to board" }));
    await waitFor(() => {
      expect(view.location()).toBe("/");
    });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  test("closing or reloading the tab is held only while text is unsaved", async () => {
    await openEditor({ current: specReview() });
    const clean = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(clean);
    expect(clean.defaultPrevented).toBe(false);

    await typeIn("unsaved");
    const dirty = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(dirty);
    expect(dirty.defaultPrevented).toBe(true);
  });

  test("the browser's Back button asks first", async () => {
    await openEditor({ current: specReview() });
    await typeIn("unsaved");

    act(() => {
      window.history.back();
    });
    const dialog = await screen.findByRole("dialog", { name: "Leave this page?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Stay and keep editing" }));
    expect(editor()).toHaveValue("unsaved");
  });
});

describe("editor keyboard", () => {
  test("the text is focused on open, and focus returns to Edit when it closes", async () => {
    await openEditor({ current: specReview() });
    expect(editor()).toHaveFocus();

    fireEvent.keyDown(editor(), { key: "Escape" });
    await waitFor(() => {
      expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
    });
    expect(within(specPanel()).getByRole("button", { name: "Edit" })).toHaveFocus();
  });

  test("Ctrl+S saves, and the browser's own save is cancelled", async () => {
    const view = await openEditor({ current: specReview() });
    await typeIn("edited");

    const event = fireEvent.keyDown(editor(), { key: "s", ctrlKey: true });
    expect(event).toBe(false);
    await waitFor(() => {
      expect(view.server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
    });
    expect(view.server.sent("PUT /requests/req-1/spec")[0]?.body).toEqual({
      content: "edited",
      base_sha256: sha256Hex(ORIGINAL),
    });
  });

  test("Cmd+S saves too", async () => {
    const view = await openEditor({ current: specReview() });
    await typeIn("edited");
    fireEvent.keyDown(editor(), { key: "S", metaKey: true });
    await waitFor(() => {
      expect(view.server.sent("PUT /requests/req-1/spec")).toHaveLength(1);
    });
  });

  test("Esc with unsaved text asks before discarding it", async () => {
    await openEditor({ current: specReview() });
    await typeIn("unsaved");

    fireEvent.keyDown(editor(), { key: "Escape" });
    const dialog = await screen.findByRole("dialog", { name: "Discard your changes?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(editor()).toHaveValue("unsaved");

    fireEvent.keyDown(editor(), { key: "Escape" });
    await userEvent.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Discard changes" }),
    );
    await waitFor(() => {
      expect(screen.queryByRole("textbox", { name: "Edit spec.md" })).not.toBeInTheDocument();
    });
  });
});
