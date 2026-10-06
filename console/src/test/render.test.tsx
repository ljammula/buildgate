import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link } from "react-router";

import { useRequest } from "@/api/requestQueries";
import { apiErrorResponse, fakeServer, fixtureResponse, json, renderApp } from "@/test/render";

function Probe() {
  const request = useRequest("req-spec-review");
  return (
    <div>
      <p>{request.data?.state ?? request.error?.message ?? "loading"}</p>
      <Link to="/runs/run-1">Open run</Link>
    </div>
  );
}

test("serves a contract fixture and records the request", async () => {
  const { server } = renderApp(<Probe />, {
    server: [
      { on: "GET /requests/req-spec-review", reply: fixtureResponse("request-spec-review.json") },
    ],
    tokens: { readToken: "read-t" },
  });
  expect(await screen.findByText("spec_review")).toBeInTheDocument();
  expect(server.sent("GET /requests/req-spec-review")[0]?.headers).toEqual({
    Authorization: "Bearer read-t",
  });
});

test("an unrouted request is a visible 404", async () => {
  renderApp(<Probe />);
  expect(
    await screen.findByText(/no fake route for GET \/requests\/req-spec-review/),
  ).toBeInTheDocument();
});

test("a route can be replaced while the test runs, and a reply is reusable", async () => {
  const server = fakeServer([{ on: "GET /x", reply: json({ n: 1 }) }]);
  expect(await (await server.fetch("/x")).json()).toEqual({ n: 1 });
  expect(await (await server.fetch("/x")).json()).toEqual({ n: 1 });
  server.set("GET /x", apiErrorResponse(503, "busy"));
  expect((await server.fetch("/x")).status).toBe(503);
  expect(server.sent("GET /x")).toHaveLength(3);
});

test("a navigation away from the mounted pattern is observable", async () => {
  const { location } = renderApp(<Probe />, {
    path: "/requests/req-spec-review",
    pattern: "/requests/:id",
  });
  await userEvent.click(screen.getByRole("link", { name: "Open run" }));
  await waitFor(() => {
    expect(location()).toBe("/runs/run-1");
  });
  expect(screen.getByText("Navigated to /runs/run-1")).toBeInTheDocument();
});
