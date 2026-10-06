import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { setOperatorName } from "@/platform/operatorIdentity";
import { readFixtureJson } from "@/test/fixtures";
import { type FakeRoute, apiErrorResponse, json, renderApp } from "@/test/render";

import { NewRequestScreen } from "./NewRequestScreen";

beforeAll(() => {
  Object.assign(Element.prototype, {
    hasPointerCapture: () => false,
    scrollIntoView: () => undefined,
  });
});

beforeEach(() => {
  setOperatorName("operator");
});
afterEach(() => {
  setOperatorName("");
});

function created(id: string) {
  return { ...(readFixtureJson("api/request-building.json") as object), id, state: "submitted" };
}

function mount(routes: readonly FakeRoute[], options: { writes?: boolean; token?: boolean } = {}) {
  return renderApp(<NewRequestScreen />, {
    server: routes,
    path: "/requests/new",
    pattern: "/requests/new",
    config: { writesEnabled: options.writes ?? false },
    tokens: options.token === false ? {} : { overrideToken: "override-token" },
  });
}

describe("NewRequestScreen", () => {
  it("new-request intake posts its fields and reports the created id", async () => {
    const user = userEvent.setup();
    const view = mount([
      { on: "GET /workspaces", reply: json([]) },
      { on: "POST /requests", reply: json(created("req-new"), 201) },
    ]);

    await user.type(await screen.findByLabelText("Workspace path"), "/repos/app");
    await user.type(screen.getByLabelText("Request"), "Add idempotency keys to POST /refunds");
    await user.click(screen.getByRole("checkbox", { name: "Draft oracles" }));
    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.type(screen.getByLabelText("Verify command"), "make ci-verify");
    await user.type(screen.getByLabelText("Preflight profile"), "brownfield");
    await user.click(screen.getByRole("button", { name: "Submit request" }));

    await waitFor(() => {
      expect(view.location()).toBe("/requests/req-new");
    });
    const sent = view.server.sent("POST /requests")[0];
    expect(sent?.headers["Authorization"]).toBe("Bearer override-token");
    expect(sent?.body).toEqual({
      workspace: "/repos/app",
      text: "Add idempotency keys to POST /refunds",
      verify_command: "make ci-verify",
      preflight_profile: "brownfield",
      draft_oracles: true,
      by: "operator",
    });
  });

  it("new-request intake reports required fields without posting", async () => {
    const user = userEvent.setup();
    const view = mount([{ on: "GET /workspaces", reply: json([]) }]);

    await user.click(await screen.findByRole("button", { name: "Submit request" }));

    expect(view.server.sent("POST /requests")).toHaveLength(0);
    expect(screen.getByText("Workspace path is required")).toBeInTheDocument();
    expect(screen.getByText("Request is required")).toBeInTheDocument();
  });

  it("the workspace dropdown fills the workspace field and shows its verify-command hint", async () => {
    const user = userEvent.setup();
    mount([
      {
        on: "GET /workspaces",
        reply: json([
          {
            workspace: "/repos/app",
            has_factory_yml: true,
            resolved_verify_command: "make ci-verify",
            verify_command_source: ".factory.yml",
          },
        ]),
      },
    ]);

    await user.selectOptions(await screen.findByLabelText("Known workspace"), "/repos/app");

    expect(screen.getByLabelText("Workspace path")).toHaveValue("/repos/app");
    expect(
      screen.getByText(/Verify command from .factory.yml: make ci-verify/),
    ).toBeInTheDocument();
  });

  it("submit is disabled and explained when the console cannot write", async () => {
    mount([{ on: "GET /workspaces", reply: json([]) }], { token: false });

    expect(await screen.findByRole("button", { name: "Submit request" })).toBeDisabled();
    expect(screen.getByText(/This console cannot write to the server from here/)).toBeVisible();
  });

  it("shows the server's message when the create is refused", async () => {
    const user = userEvent.setup();
    mount([
      { on: "GET /workspaces", reply: json([]) },
      { on: "POST /requests", reply: apiErrorResponse(400, "workspace is not allowed") },
    ]);

    await user.type(await screen.findByLabelText("Workspace path"), "/nope");
    await user.type(screen.getByLabelText("Request"), "x");
    await user.click(screen.getByRole("button", { name: "Submit request" }));

    expect(await screen.findByText(/workspace is not allowed/)).toBeInTheDocument();
  });

  it("asks for the operator name once when none is stored", async () => {
    setOperatorName("");
    const user = userEvent.setup();
    const view = mount([
      { on: "GET /workspaces", reply: json([]) },
      { on: "POST /requests", reply: json(created("req-2"), 201) },
    ]);

    await user.type(await screen.findByLabelText("Workspace path"), "/repos/app");
    await user.type(screen.getByLabelText("Request"), "x");
    await user.click(screen.getByRole("button", { name: "Submit request" }));
    await user.type(await screen.findByLabelText("Operator name"), "kanna");
    await user.click(screen.getByRole("button", { name: "Continue" }));

    await waitFor(() => {
      expect(view.location()).toBe("/requests/req-2");
    });
    expect(view.server.sent("POST /requests")[0]?.body).toMatchObject({ by: "kanna" });
  });
});
